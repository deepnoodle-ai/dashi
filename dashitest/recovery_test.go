package dashitest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// TestRecoveryFromEachCommitFault runs a turn that calls one local tool and
// then answers. It fails one commit at each step boundary in each way a
// durable store can, reloads the record, and resumes on a new worker. The
// resumed turn must complete without repeating a model call or tool execution
// that the record shows committed, and a lost acknowledgement must be
// reported as unknown rather than assumed to have failed or succeeded.
func TestRecoveryFromEachCommitFault(t *testing.T) {
	// Without a fault the record is: start, model intent and settlement,
	// tool decision, start, and settlement, the answering model intent and
	// settlement at 7 and 8, and the stop. A zero Seq targets the first step
	// of that kind.
	targets := []struct {
		kind dashi.StepKind
		seq  int64
	}{
		{kind: dashi.StepTurnStarted},
		{kind: dashi.StepModelCallStarted},
		{kind: dashi.StepModelCallFinished},
		{kind: dashi.StepToolDecision},
		{kind: dashi.StepToolStarted},
		{kind: dashi.StepToolFinished},
		{kind: dashi.StepModelCallStarted, seq: 7},
		{kind: dashi.StepModelCallFinished, seq: 8},
		{kind: dashi.StepTurnStopped},
	}
	faults := []dashitest.Fault{
		dashitest.Reject,
		dashitest.LoseWrite,
		dashitest.LoseAck,
	}
	// The jsonb store returns every reloaded step respelled, so recovery must
	// not depend on the bytes the runner wrote.
	// The file store is the reference durable recorder, run through the same
	// faults on disk.
	stores := map[string]func(*testing.T) dashitest.Store{
		"memory": func(*testing.T) dashitest.Store {
			return dashi.NewMemoryRecorder()
		},
		"jsonb": func(*testing.T) dashitest.Store {
			return newJSONBStore()
		},
		"file": func(t *testing.T) dashitest.Store {
			r, err := filerecorder.New(filerecorder.Config{Dir: t.TempDir()})
			if err != nil {
				t.Skip(err)
			}
			return r
		},
	}
	for storeName, newStore := range stores {
		for _, target := range targets {
			for _, fault := range faults {
				name := fmt.Sprintf("%s/%s/%s", storeName, target.kind, fault)
				if target.seq != 0 {
					name = fmt.Sprintf(
						"%s/%s@%d/%s",
						storeName,
						target.kind,
						target.seq,
						fault,
					)
				}
				t.Run(name, func(t *testing.T) {
					store := newStore(t)
					rec := &dashitest.FaultRecorder{
						Recorder: store,
						Kind:     target.kind,
						Seq:      target.seq,
						Fault:    fault,
					}
					testRecovery(t, store, rec)
				})
			}
		}
	}
}

func testRecovery(
	t *testing.T,
	store dashitest.Store,
	rec *dashitest.FaultRecorder,
) {
	fault := rec.Fault
	model := &scriptedModel{}
	work := &countingTool{}
	runner := dashi.Runner{
		Client:   model,
		Tools:    work,
		Recorder: rec,
	}
	begin := dashi.Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do the work"))},
	}

	claim(t, store, "t1", "worker-1")
	first, err := runner.Run(
		t.Context(),
		dashi.Turn{ID: "t1"},
		begin,
		dashi.WithOwner("worker-1"),
	)
	fired, ok := rec.Fired()
	if !ok {
		t.Fatalf("no %s step at seq %d was committed", rec.Kind, rec.Seq)
	}
	kind := fired.Kind
	committed := fault == dashitest.LoseAck
	requireReported(t, first, err, fired, fault)

	// A new worker takes over from the durable record alone.
	claim(t, store, "t1", "worker-2")
	saved, err := store.Load(t.Context(), "t1")
	if err != nil && committed {
		t.Fatalf("load after lost acknowledgement: %v", err)
	}
	requireRecordAt(t, saved, fired, committed)
	if len(saved.Steps) > 0 {
		if _, err := dashi.Replay(saved); err != nil {
			t.Fatalf("record does not replay: %v", err)
		}
	}

	// Nothing may depend on a step before it is acknowledged.
	switch kind {
	case dashi.StepModelCallStarted:
		want := countIntents(saved, fired.Seq)
		if model.calls() != want {
			t.Fatalf(
				"model called %d times before intent %d was acknowledged; want %d",
				model.calls(),
				fired.Seq,
				want,
			)
		}
	case dashi.StepToolStarted:
		if work.calls() != 0 {
			t.Fatal("tool ran before its start was acknowledged")
		}
	}

	out := resume(t, saved, begin, runner, dashi.WithOwner("worker-2"))
	if uncertainTool(kind, committed) {
		// The record cannot show whether the tool took effect, so the
		// runner must stop and ask rather than run it again or assume.
		if out.Stop.Kind != dashi.StopResultUnknown {
			t.Fatalf("resumed stop is %+v; want uncertain", out.Stop)
		}
		result := tool.Outcome{
			State:  tool.StateSucceeded,
			Output: new(tool.TextOutput("verified by the host")),
		}
		out = run(
			t,
			out.Turn,
			dashi.Resolve{CommandID: "reconcile", CallID: "call-1", Outcome: &result},
			runner,
			dashi.WithOwner("worker-2"),
		)
		out = run(
			t,
			out.Turn,
			dashi.Continue{CommandID: "after-reconcile"},
			runner,
			dashi.WithOwner("worker-2"),
		)
	}

	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("recovered stop is %+v; want completed", out.Stop)
	}
	if got := out.Reply; got != "done" {
		t.Fatalf("answer is %q; want done", got)
	}
	final, err := store.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Steps) != len(out.Turn.Steps) {
		t.Fatalf(
			"store has %d steps but the outcome has %d",
			len(final.Steps),
			len(out.Turn.Steps),
		)
	}

	// A model result that was never recorded may be requested again; the
	// docs promise Continue resends an interrupted call. Anything recorded
	// must not be repeated.
	wantCalls := 2
	if kind == dashi.StepModelCallFinished && !committed {
		wantCalls = 3
	}
	if model.calls() != wantCalls {
		t.Fatalf("model called %d times; want %d", model.calls(), wantCalls)
	}
	wantRuns := 1
	if kind == dashi.StepToolStarted && committed {
		wantRuns = 0
	}
	if work.calls() != wantRuns {
		t.Fatalf("tool ran %d times; want %d", work.calls(), wantRuns)
	}
	requireEveryIntentSettled(t, final)
}

// TestStaleRetryAfterFailedLoadRepeatsNothing retries a command with an old
// snapshot while Load fails. Run cannot confirm the snapshot, so it returns
// the read error without committing, and no model call or tool execution
// repeats.
func TestStaleRetryAfterFailedLoadRepeatsNothing(t *testing.T) {
	tests := []struct {
		name string
		kind dashi.StepKind
	}{
		{name: "completed"},
		{name: "after model intent", kind: dashi.StepModelCallStarted},
		{name: "after tool start", kind: dashi.StepToolStarted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := dashi.NewMemoryRecorder()
			model := &scriptedModel{}
			work := &countingTool{}
			var rec dashi.Recorder = store
			if tc.kind != "" {
				// A lost acknowledgement stops the first worker right
				// after the step is recorded.
				rec = &dashitest.FaultRecorder{
					Recorder: store,
					Kind:     tc.kind,
					Fault:    dashitest.LoseAck,
				}
			}
			runner := dashi.Runner{
				Client:   model,
				Tools:    work,
				Recorder: rec,
			}
			// The lost acknowledgement comes back as a *CommitError.
			_, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, startCommand())
			if _, lost := errors.AsType[*dashi.CommitError](err); err != nil && !lost {
				t.Fatal(err)
			}
			before, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			sends, runs := model.calls(), work.calls()

			runner.Recorder = unreadable{store}
			_, err = runner.Run(
				t.Context(),
				dashi.Turn{ID: "t1"},
				startCommand(),
			)
			if !errors.Is(err, errUnreadable) {
				t.Fatalf("stale retry returned %v; want the read error", err)
			}
			if model.calls() != sends {
				t.Fatalf(
					"stale retry called the model %d more times",
					model.calls()-sends,
				)
			}
			if work.calls() != runs {
				t.Fatalf("stale retry ran the tool %d more times", work.calls()-runs)
			}
			after, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Steps) != len(before.Steps) {
				t.Fatalf(
					"stale retry changed the record from %d to %d steps",
					len(before.Steps),
					len(after.Steps),
				)
			}
		})
	}
}

// TestStartProceedsWhenTurnNotFound starts a new turn through each shape the
// Loader contract allows for a turn with no steps.
func TestStartProceedsWhenTurnNotFound(t *testing.T) {
	tests := []struct {
		name string
		load func(string) (dashi.Turn, error)
	}{
		{
			name: "wrapped not found",
			load: func(string) (dashi.Turn, error) {
				return dashi.Turn{}, fmt.Errorf(
					"select steps: %w",
					dashi.ErrTurnNotFound,
				)
			},
		},
		{
			name: "empty turn",
			load: func(id string) (dashi.Turn, error) {
				return dashi.Turn{ID: id}, nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedModel{}
			work := &countingTool{}
			out := run(
				t,
				dashi.Turn{ID: "t1"},
				startCommand(),
				dashi.Runner{
					Client:   model,
					Tools:    work,
					Recorder: newTurnLoader{dashi.NewMemoryRecorder(), tc.load},
				},
			)
			if out.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("new turn stopped with %+v; want completed", out.Stop)
			}
			if model.calls() != 2 || work.calls() != 1 {
				t.Fatalf(
					"new turn made %d model calls and %d tool runs; want 2 and 1",
					model.calls(),
					work.calls(),
				)
			}
		})
	}
}

func startCommand() dashi.Start {
	return dashi.Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do the work"))},
	}
}

var errUnreadable = errors.New("dashitest: read unavailable")

// unreadable is a recorder whose reads fail, as during a database outage
// that still lets the commit path through.
type unreadable struct {
	*dashi.MemoryRecorder
}

func (unreadable) Load(context.Context, string) (dashi.Turn, error) {
	return dashi.Turn{}, errUnreadable
}

// newTurnLoader reports every turn as new through load.
type newTurnLoader struct {
	*dashi.MemoryRecorder
	load func(string) (dashi.Turn, error)
}

func (l newTurnLoader) Load(
	_ context.Context,
	id string,
) (dashi.Turn, error) {
	return l.load(id)
}

// resume continues the turn from its reloaded record, or retries the start
// command when no step was recorded.
func resume(
	t *testing.T,
	saved dashi.Turn,
	begin dashi.Start,
	runner dashi.Runner,
	opts ...dashi.RunOption,
) dashi.RunResult {
	t.Helper()
	if len(saved.Steps) == 0 {
		return run(t, dashi.Turn{ID: "t1"}, begin, runner, opts...)
	}
	view, err := dashi.Replay(saved)
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind == dashi.StopCompleted {
		// Retrying the original command returns the recorded outcome.
		return run(t, saved, begin, runner, opts...)
	}
	return run(t, saved, dashi.Continue{CommandID: "resume"}, runner, opts...)
}

func run(
	t *testing.T,
	turn dashi.Turn,
	command dashi.Command,
	runner dashi.Runner,
	opts ...dashi.RunOption,
) dashi.RunResult {
	t.Helper()
	out, err := runner.Run(t.Context(), turn, command, opts...)
	if err != nil {
		t.Fatalf("run %T: %v", command, err)
	}
	return out
}

// uncertainTool reports whether the reloaded record shows a started tool
// with no result. Only the host can reconcile that call.
func uncertainTool(kind dashi.StepKind, committed bool) bool {
	return kind == dashi.StepToolStarted && committed ||
		kind == dashi.StepToolFinished && !committed
}

func requireReported(
	t *testing.T,
	out dashi.RunResult,
	err error,
	fired dashi.Step,
	fault dashitest.Fault,
) {
	t.Helper()
	want := dashi.PersistenceUnknown
	if fault == dashitest.Reject {
		want = dashi.PersistenceRejected
	}
	commitErr, ok := errors.AsType[*dashi.CommitError](err)
	if !ok {
		t.Fatalf("run error %v is not a *dashi.CommitError", err)
	}
	if commitErr.Persistence != want {
		t.Fatalf("reported persistence %q; want %q", commitErr.Persistence, want)
	}
	if commitErr.Step == nil {
		t.Fatal("error does not name the unacknowledged step")
	}
	if commitErr.Step.Seq != fired.Seq ||
		commitErr.Step.Kind != fired.Kind {
		t.Fatalf(
			"unacknowledged step is %s at %d; want %s at %d",
			commitErr.Step.Kind,
			commitErr.Step.Seq,
			fired.Kind,
			fired.Seq,
		)
	}
	if !errors.Is(err, dashitest.ErrInjected) {
		t.Fatalf("write error %v does not wrap the injected fault", err)
	}
	if out.Turn.ID != fired.TurnID {
		t.Fatalf("outcome lost the turn: %q", out.Turn.ID)
	}
	if out.Stop.Kind == dashi.StopCompleted {
		t.Fatal("an unacknowledged step was reported as a completed turn")
	}
}

func requireRecordAt(
	t *testing.T,
	saved dashi.Turn,
	fired dashi.Step,
	committed bool,
) {
	t.Helper()
	want := fired.Seq - 1
	if committed {
		want = fired.Seq
	}
	if int64(len(saved.Steps)) != want {
		t.Fatalf("record has %d steps; want %d", len(saved.Steps), want)
	}
	if committed && saved.Steps[want-1].Kind != fired.Kind {
		t.Fatalf(
			"record ends with %s; want %s",
			saved.Steps[want-1].Kind,
			fired.Kind,
		)
	}
}

// countIntents counts the model intents recorded before seq.
func countIntents(turn dashi.Turn, seq int64) int {
	n := 0
	for _, step := range turn.Steps {
		if step.Seq < seq && step.Kind == dashi.StepModelCallStarted {
			n++
		}
	}
	return n
}

// requireEveryIntentSettled checks that the record explains every model call
// it began, including calls interrupted by the fault.
func requireEveryIntentSettled(t *testing.T, turn dashi.Turn) {
	t.Helper()
	open := map[string]bool{}
	for _, step := range turn.Steps {
		switch step.Kind {
		case dashi.StepModelCallStarted:
			open[step.Model.ModelCallID] = true
		case dashi.StepModelCallFinished, dashi.StepModelCallFinishedByHost:
			delete(open, step.Model.ModelCallID)
		}
	}
	if len(open) > 0 {
		t.Fatalf("model attempts without a settlement: %v", open)
	}
}

func testAgent() dashi.Agent {
	return dashi.Agent{
		ID:       "agent",
		Revision: "r1",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
		Limits: dashi.Limits{
			ModelCalls:      4,
			ProviderPauses:  1,
			ToolConcurrency: 1,
		},
	}
}

// scriptedModel asks for the work tool until it sees a tool result, then
// answers. Deciding from the input rather than a call counter keeps a
// resent request consistent with the original.
type scriptedModel struct {
	mu    sync.Mutex
	sends int
}

func (*scriptedModel) Provider() string {
	return "test"
}

func (*scriptedModel) API() string {
	return "test.chat"
}

func (*scriptedModel) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (*scriptedModel) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (m *scriptedModel) Send(
	_ context.Context,
	call llm.Call,
) (llm.Result, error) {
	m.mu.Lock()
	m.sends++
	m.mu.Unlock()
	result := llm.Result{
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Message:   llm.AssistantMessage(llm.Text("done")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
	}
	if !hasToolResult(call.Request.Snapshot().Input.Messages) {
		call := tool.Call{ID: "call-1", Name: "work", Args: []byte(`{}`)}
		result.Message = llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		})
		result.Stop = llm.Stop{Kind: llm.StopToolUse}
	}
	return result, nil
}

func (m *scriptedModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sends
}

// countingTool is a local tool that is neither read-only nor idempotent, so
// the runner must never run it twice.
type countingTool struct {
	mu   sync.Mutex
	runs int
}

func (c *countingTool) Tools(context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Definition: tool.Definition{
				Name:   "work",
				Schema: []byte(`{"type":"object"}`),
				Kind:   tool.KindLocal,
			},
			Executor: c,
		},
	}, nil
}

func (c *countingTool) Execute(
	context.Context,
	tool.Call,
) (tool.Outcome, error) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	return tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("worked")),
	}, nil
}

func (c *countingTool) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

func hasToolResult(messages []llm.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentToolResult {
				return true
			}
		}
	}
	return false
}

func claim(t *testing.T, s dashitest.Store, id, owner string) {
	t.Helper()
	if err := s.Claim(t.Context(), id, owner); err != nil {
		t.Fatalf("claim %q for %q: %v", id, owner, err)
	}
}
