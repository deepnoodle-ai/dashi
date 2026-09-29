package delegate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	parentTurn = "parent-turn"
	callID     = "call-1"
)

// modelFunc answers one model request. The system text tells parent
// requests from child requests.
type modelFunc func(ctx context.Context, in llm.Input) (llm.Result, error)

type fakeClient struct {
	handle modelFunc
	mu     sync.Mutex
	inputs []llm.Input
}

func (*fakeClient) Provider() string {
	return "test"
}

func (*fakeClient) API() string {
	return "test.chat"
}

func (*fakeClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c *fakeClient) Prepare(
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
		Body:      []byte(`{"ok":true}`),
	})
}

func (c *fakeClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	in := call.Request.Snapshot().Input
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	c.mu.Unlock()
	result, err := c.handle(ctx, in)
	result.Requested = llm.ModelRef{
		Provider: "test",
		Model:    call.Request.Snapshot().Origin.Model,
	}
	return result, err
}

// sends counts model requests whose system text is system.
func (c *fakeClient) sends(system string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, in := range c.inputs {
		if role(in) == system {
			n++
		}
	}
	return n
}

func (c *fakeClient) last(system string) llm.Input {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.inputs) - 1; i >= 0; i-- {
		if role(c.inputs[i]) == system {
			return c.inputs[i]
		}
	}
	return llm.Input{}
}

// role names the agent that made a request: its instructions come first in
// the system text.
func role(in llm.Input) string {
	first, _, _ := strings.Cut(in.System, "\n")
	return first
}

func text(s string) llm.Result {
	return llm.Result{
		Message: llm.AssistantMessage(llm.Text(s)),
		Stop:    llm.Stop{Kind: llm.StopFinished},
	}
}

func costed(r llm.Result, total string) llm.Result {
	r.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 5}
	r.Cost = &llm.Cost{
		Total:    money.MustParseAmount(total),
		Currency: "USD",
		Source:   llm.CostSourceProviderReported,
	}
	return r
}

func toolCall(id, name, args string) llm.Result {
	call := tool.Call{ID: id, Name: name, Args: json.RawMessage(args)}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop: llm.Stop{Kind: llm.StopToolUse},
	}
}

func taskCall(prompt string) llm.Result {
	args, _ := json.Marshal(map[string]string{
		"subagent":    "scout",
		"prompt":      prompt,
		"description": "look",
	})
	return toolCall(callID, delegate.ToolName, string(args))
}

// script is the default model: the parent delegates once and then finishes,
// and the child answers at once.
func script(childAnswer string) modelFunc {
	return func(_ context.Context, in llm.Input) (llm.Result, error) {
		switch {
		case role(in) == "parent" && len(in.Messages) == 1:
			return costed(taskCall("find the parser"), "1"), nil
		case role(in) == "parent":
			return costed(text("parent done"), "1"), nil
		}
		return costed(text(childAnswer), "0.25"), nil
	}
}

func agent(system string) dashi.Agent {
	return dashi.Agent{
		ID:           system,
		Revision:     "r1",
		Provider:     "test",
		API:          "test.chat",
		Model:        "fixture",
		Instructions: system,
	}
}

func scout() delegate.Subagent {
	return delegate.Subagent{
		Name:        "scout",
		Description: "Looks around.",
		Agent:       agent("child"),
	}
}

func yes() *bool {
	v := true
	return &v
}

func probe(name string, readOnly *bool, ran *atomic.Int32) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:        name,
			Description: "A probe.",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Annotations: tool.Annotations{ReadOnly: readOnly},
		},
		Executor: executor(func(
			context.Context,
			tool.Call,
		) (tool.Outcome, error) {
			ran.Add(1)
			return tool.Succeeded("probed"), nil
		}),
	}
}

type executor func(context.Context, tool.Call) (tool.Outcome, error)

func (f executor) Execute(
	ctx context.Context,
	c tool.Call,
) (tool.Outcome, error) {
	return f(ctx, c)
}

type harness struct {
	client   *fakeClient
	rec      *dashi.MemoryRecorder
	base     tool.Toolset
	runner   dashi.Runner
	events   chan delegate.ChildEvent
	observed atomic.Int32
}

// newHarness wires a parent runner whose toolset holds the task tool. store
// replaces the recorder when a test wraps it.
func newHarness(
	t *testing.T,
	handle modelFunc,
	cfg delegate.Config,
	store func(*dashi.MemoryRecorder) dashi.Recorder,
) *harness {
	t.Helper()
	h := &harness{
		client: &fakeClient{handle: handle},
		rec:    dashi.NewMemoryRecorder(),
		events: make(chan delegate.ChildEvent, 1024),
	}
	var recorder dashi.Recorder = h.rec
	if store != nil {
		recorder = store(h.rec)
	}
	var ran atomic.Int32
	h.base = tool.List{
		probe("read", yes(), &ran),
		probe("write", nil, &ran),
	}
	cfg.Subagents = cmpSubagents(cfg.Subagents)
	cfg.Runner.Observer = func(dashi.Event) {
		h.observed.Add(1)
	}
	cfg.OnChild = func(e delegate.ChildEvent) {
		h.events <- e
	}
	h.runner = dashi.Runner{
		Agent:    agent("parent"),
		Client:   h.client,
		Recorder: recorder,
		Tools:    tool.Join(h.base, delegate.New(cfg)),
	}
	return h
}

func cmpSubagents(in []delegate.Subagent) []delegate.Subagent {
	if len(in) > 0 {
		return in
	}
	return []delegate.Subagent{scout()}
}

func (h *harness) start(ctx context.Context) (dashi.Outcome, error) {
	return h.runner.Run(
		ctx,
		dashi.Turn{ID: parentTurn},
		dashi.Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
}

func (h *harness) resume(ctx context.Context) (dashi.Outcome, error) {
	turn, err := h.rec.Load(ctx, parentTurn)
	if err != nil {
		return dashi.Outcome{}, err
	}
	return h.runner.Run(ctx, turn, dashi.Continue{})
}

func (h *harness) view(t *testing.T, id string) dashi.TurnView {
	t.Helper()
	turn, err := h.rec.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// waitStop waits for a turn's record to show a stop of kind.
func (h *harness) waitStop(t *testing.T, id string, kind dashi.StopKind) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if turn, err := h.rec.Load(t.Context(), id); err == nil {
			view, err := dashi.Replay(turn)
			if err == nil && view.Stop.Kind == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn %s never showed stop %s", id, kind)
}

func childID() string {
	return delegate.ChildID(parentTurn, callID)
}

func toolResult(t *testing.T, v dashi.TurnView) string {
	t.Helper()
	view, ok := v.Tools[callID]
	if !ok || view.Status != dashi.ToolSettled {
		t.Fatalf("task call not settled: %+v", view.Status)
	}
	var sb strings.Builder
	for _, b := range view.Result().Output.Content {
		if b.Text != nil {
			sb.WriteString(b.Text.Text)
		}
	}
	return sb.String()
}

func TestTaskReturnsChildAnswerAndLinksChild(t *testing.T) {
	h := newHarness(t, script("it is in config/load.go"), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	if got := toolResult(t, out.TurnView); got != "it is in config/load.go" {
		t.Fatalf("task result=%q", got)
	}
	child := h.view(t, childID())
	want := dashi.CallRef{TurnID: parentTurn, CallID: callID}
	if child.Parent == nil || *child.Parent != want {
		t.Fatalf("child parent=%+v, want %+v", child.Parent, want)
	}
	if child.Agent.ID != "child" || child.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("child=%+v %+v", child.Agent.ID, child.Stop)
	}
	if out.Parent != nil {
		t.Fatalf("root turn has a parent: %+v", out.Parent)
	}
}

func TestReportCountsChildOnceAndKeepsCostsSeparate(t *testing.T) {
	h := newHarness(t, script("done"), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	usd := money.MustParseAmount
	if got := out.Spend.Cost["USD"]; got != usd("2") {
		t.Fatalf("parent view spend=%v, want its own 2 only", got)
	}
	report, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil {
		t.Fatal(err)
	}
	if report.Parent.Cost["USD"] != usd("2") ||
		report.Children.Cost["USD"] != usd("0.25") ||
		report.Inclusive.Cost["USD"] != usd("2.25") {
		t.Fatalf("report=%+v", report)
	}
	if len(report.Inclusive.ToolCost) != 0 || len(report.Children.ToolCost) != 0 {
		t.Fatalf("child model cost leaked into tool cost: %+v", report)
	}
	// Reporting again after the turn resumes must not double count.
	again, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil || again.Inclusive.Cost["USD"] != usd("2.25") {
		t.Fatalf("second report=%+v err=%v", again, err)
	}
}

func TestReportKeepsUnknownPriceFlags(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "child" {
			r := text("done")
			r.Usage = &llm.Usage{InputTokens: 1, OutputTokens: 1}
			return r, nil // no cost: the price is unknown
		}
		return script("")(ctx, in)
	}
	h := newHarness(t, handle, delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	report, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil {
		t.Fatal(err)
	}
	if report.Children.CostUnknown != 1 || report.Inclusive.CostUnknown != 1 {
		t.Fatalf("unknown price lost: %+v", report)
	}
	if report.Parent.CostUnknown != 0 {
		t.Fatalf("child flag moved to the parent: %+v", report.Parent)
	}
}

func TestChildEventsReachOnChildAndTemplateObserver(t *testing.T) {
	h := newHarness(t, script("done"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.observed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.observed.Load() == 0 {
		t.Fatal("template observer saw no child event")
	}
	sawStep := false
	for len(h.events) > 0 {
		e := <-h.events
		want := dashi.CallRef{TurnID: parentTurn, CallID: callID}
		if e.Parent != want || e.Subagent != "scout" ||
			e.Event.TurnID != childID() {
			t.Fatalf("event=%+v", e)
		}
		sawStep = sawStep || e.Event.Step != nil
	}
	if !sawStep {
		t.Fatal("OnChild saw no committed child step")
	}
}

// crashRecorder fails every commit once the trigger matches, as a process
// that died at that point would. Revive lets a restarted worker commit.
type crashRecorder struct {
	dashi.Recorder
	trigger func(dashi.CommitRequest) bool
	dead    atomic.Bool
	starts  atomic.Int32
}

func (c *crashRecorder) Load(
	ctx context.Context,
	id string,
) (dashi.Turn, error) {
	return c.Recorder.(dashi.Loader).Load(ctx, id)
}

func (c *crashRecorder) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	if c.dead.Load() || c.trigger != nil && c.trigger(req) {
		c.dead.Store(true)
		return &dashi.CommitError{
			Persistence: dashi.PersistenceRejected,
			Err:         errors.New("crashed"),
		}
	}
	if req.Step.Kind == dashi.StepStarted &&
		strings.HasPrefix(req.Step.TurnID, "child-") {
		c.starts.Add(1)
	}
	return c.Recorder.Commit(ctx, req)
}

func TestTaskRetryAfterCrashNeverStartsSecondChild(t *testing.T) {
	isChild := func(r dashi.CommitRequest) bool {
		return strings.HasPrefix(r.Step.TurnID, "child-")
	}
	for _, tc := range []struct {
		name       string
		trigger    func(dashi.CommitRequest) bool
		childSends int
	}{
		{
			name: "before child start",
			trigger: func(r dashi.CommitRequest) bool {
				return isChild(r) && r.Step.Kind == dashi.StepStarted
			},
			childSends: 1,
		},
		{
			name: "after child start",
			trigger: func(r dashi.CommitRequest) bool {
				return isChild(r) && r.Step.Kind == dashi.StepModelIntent
			},
			childSends: 1,
		},
		{
			name: "after child completion",
			trigger: func(r dashi.CommitRequest) bool {
				return !isChild(r) && r.Step.Kind == dashi.StepToolSettled
			},
			childSends: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var crash *crashRecorder
			h := newHarness(t, script("answer"), delegate.Config{},
				func(m *dashi.MemoryRecorder) dashi.Recorder {
					crash = &crashRecorder{Recorder: m, trigger: tc.trigger}
					return crash
				})
			if _, err := h.start(t.Context()); err == nil {
				t.Fatal("the crash did not stop the run")
			}
			crash.trigger = nil
			crash.dead.Store(false)
			out, err := h.resume(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("parent stop=%+v", out.Stop)
			}
			if got := toolResult(t, out.TurnView); got != "answer" {
				t.Fatalf("task result=%q", got)
			}
			if got := h.client.sends("child"); got != tc.childSends {
				t.Fatalf("child model calls=%d, want %d", got, tc.childSends)
			}
			started := crash.starts.Load()
			if tc.name == "before child start" {
				started = 1 // the crashed start was never saved
			}
			if started != 1 {
				t.Fatalf("child turns started=%d, want 1", started)
			}
		})
	}
}

// blockingChild makes child requests wait for the run's context to end, and
// answers once release is set.
func blockingChild(started chan<- struct{}, release *atomic.Bool) modelFunc {
	return func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "child" {
			return script("")(ctx, in)
		}
		if release.Load() {
			return costed(text("finished after resume"), "0.25"), nil
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return llm.Result{}, ctx.Err()
	}
}

func TestCancelReachesRunningChild(t *testing.T) {
	started := make(chan struct{}, 1)
	var release atomic.Bool
	// Only the parent sets OnContextDone. The child inherits it.
	h := newHarness(t, blockingChild(started, &release), delegate.Config{}, nil)
	h.runner.OnContextDone = dashi.ContextCancel

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(ctx)
		done <- out
	}()
	<-started
	cancel()
	out := <-done
	if out.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	h.waitStop(t, childID(), dashi.StopCanceled)
	if v := h.view(t, parentTurn); v.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("parent record stop=%+v", v.Stop)
	}
}

func TestInterruptResumesSameChild(t *testing.T) {
	started := make(chan struct{}, 1)
	var release atomic.Bool
	h := newHarness(
		t,
		blockingChild(started, &release),
		delegate.Config{},
		nil,
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(ctx)
		done <- out
	}()
	<-started
	cancel()
	out := <-done
	if out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	h.waitStop(t, childID(), dashi.StopInterrupted)
	if v := h.view(t, parentTurn); v.Tools[callID].Status == dashi.ToolSettled {
		t.Fatal("an interrupted child settled the parent call")
	}

	release.Store(true)
	resumed, err := h.resume(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("resumed parent stop=%+v", resumed.Stop)
	}
	if got := toolResult(t, resumed.TurnView); got != "finished after resume" {
		t.Fatalf("task result=%q", got)
	}
	child := h.view(t, childID())
	if child.ModelAttempts != 1 || child.Interruptions != 1 {
		t.Fatalf("child attempts=%d interruptions=%d",
			child.ModelAttempts, child.Interruptions)
	}
	for _, step := range resumedSteps(t, h) {
		if step.Kind == dashi.StepToolSettled && step.Tool != nil &&
			step.Tool.Outcome != nil &&
			step.Tool.Outcome.State == tool.StateFailed {
			t.Fatal("parent recorded a failed task result")
		}
	}
}

func resumedSteps(t *testing.T, h *harness) []dashi.Step {
	t.Helper()
	turn, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	return turn.Steps
}

// bareRecorder hides Load, so it is not a dashi.Loader.
type bareRecorder struct {
	dashi.Recorder
}

func TestTaskWithoutLoaderWarnsAndIsNotIdempotent(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// An override Recorder that is not a Loader is known at New.
	tools := delegate.New(delegate.Config{
		Runner: dashi.Runner{Recorder: bareRecorder{dashi.NewMemoryRecorder()}},
	})
	list, err := tools.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list {
		if tl.Definition.Name == delegate.ToolName &&
			tl.Definition.Annotations.Idempotent != nil {
			t.Fatal("task is marked idempotent without a Loader")
		}
	}
	// An inherited Recorder that is not a Loader is known at the first call.
	h := newHarness(t, script("x"), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			return bareRecorder{m}
		})
	for range 2 {
		h.rec = dashi.NewMemoryRecorder()
		h.runner.Recorder = bareRecorder{h.rec}
		if _, err := h.runner.Run(
			t.Context(),
			dashi.Turn{ID: parentTurn},
			dashi.Start{
				CommandID: "start",
				Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "without a host recorder"); n != 2 {
		t.Fatalf("warnings=%d, want 2 (one from New, one from the tool): %q",
			n, logs.String())
	}
}

func TestTaskIsIdempotentWithLoader(t *testing.T) {
	h := newHarness(t, script("x"), delegate.Config{}, nil)
	tools, err := h.runner.Tools.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Definition.Name == delegate.ToolName {
			i := tl.Definition.Annotations.Idempotent
			if i == nil || !*i {
				t.Fatal("task is not marked idempotent")
			}
			return
		}
	}
	t.Fatal("no task tool")
}

// childCallsProbe has the child call the write probe once and then answer.
func childCallsProbe(ctx context.Context, in llm.Input) (llm.Result, error) {
	if role(in) != "child" {
		return script("")(ctx, in)
	}
	if len(in.Messages) == 1 {
		return toolCall("c1", "write", `{}`), nil
	}
	return text("child finished"), nil
}

func TestChildToolCallsPassHostHooks(t *testing.T) {
	var mu sync.Mutex
	var seen []dashi.ToolPlan
	hook := dashi.ToolHook{
		Name: "host-approval",
		Func: func(
			_ context.Context,
			p dashi.ToolPlan,
		) (dashi.ToolDecision, error) {
			mu.Lock()
			seen = append(seen, p)
			mu.Unlock()
			if p.Call.Name == "write" {
				return dashi.Deny("host said no"), nil
			}
			return dashi.ToolDecision{}, nil
		},
	}
	h := newHarness(t, childCallsProbe, delegate.Config{}, nil)
	h.runner.Hooks = hook.Hooks()
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range seen {
		if p.Call.Name != "write" {
			continue
		}
		if p.Turn.Parent == nil || p.Turn.Agent.ID != "child" {
			t.Fatalf("hook saw turn parent=%+v agent=%q",
				p.Turn.Parent, p.Turn.Agent.ID)
		}
		child := h.view(t, childID())
		if v := child.Tools["c1"]; v.Status != dashi.ToolSettled ||
			!strings.Contains(v.Verdict.Reason, "host said no") {
			t.Fatalf("child write call=%+v", v.Status)
		}
		return
	}
	t.Fatal("the host hook never saw the child's write call")
}

func TestDeferredApprovalInChildFailsTheCall(t *testing.T) {
	hook := dashi.ToolHook{
		Name: "defer-writes",
		Func: func(
			_ context.Context,
			p dashi.ToolPlan,
		) (dashi.ToolDecision, error) {
			if p.Call.Name == "write" {
				return dashi.Defer("needs a person"), nil
			}
			return dashi.ToolDecision{}, nil
		},
	}
	h := newHarness(t, childCallsProbe, delegate.Config{}, nil)
	h.runner.Hooks = hook.Hooks()
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := toolResult(t, out.TurnView)
	if !strings.Contains(got, "approval that was deferred") {
		t.Fatalf("task result=%q", got)
	}
	if h.view(t, childID()).Stop.Kind != dashi.StopWaiting {
		t.Fatal("the child is not resumable by its ID")
	}
}

func toolNames(in llm.Input) []string {
	var names []string
	for _, d := range in.Tools {
		names = append(names, d.Name)
	}
	return names
}

func TestChildToolsAreFilteredAndNeverIncludeTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  delegate.Subagent
		want []string
	}{
		{
			name: "all tools except task",
			sub:  scout(),
			want: []string{"read", "write"},
		},
		{
			name: "read-only",
			sub: func() delegate.Subagent {
				s := scout()
				s.ReadOnly = true
				return s
			}(),
			want: []string{"read"},
		},
		{
			name: "named tools",
			sub: func() delegate.Subagent {
				s := scout()
				s.Tools = []string{"write", "task"}
				return s
			}(),
			want: []string{"write"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := delegate.Config{Subagents: []delegate.Subagent{tc.sub}}
			h := newHarness(t, script("x"), cfg, nil)
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := toolNames(h.client.last("child"))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("child tools=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestChildSeesOnlyItsPrompt(t *testing.T) {
	h := newHarness(t, script("x"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	in := h.client.last("child")
	if len(in.Messages) != 1 || in.Messages[0].Text() != "find the parser" {
		t.Fatalf("child input=%+v", in.Messages)
	}
}

func TestTaskCutsLongAnswer(t *testing.T) {
	long := strings.Repeat("é", 100)
	cfg := delegate.Config{MaxAnswer: 51}
	h := newHarness(t, script(long), cfg, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := toolResult(t, out.TurnView)
	if !strings.Contains(got, "[answer cut at 50 bytes]") ||
		len(got) > 51+len("\n[answer cut at 50 bytes]") {
		t.Fatalf("task result=%q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("é", 25)) {
		t.Fatalf("cut split a character: %q", got)
	}
}

func TestTaskFailedChildSettlesErrorAndIsNotRerun(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "child" {
			return text("  "), nil // an empty answer fails the child
		}
		return script("")(ctx, in)
	}
	var crash *crashRecorder
	h := newHarness(t, handle, delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			// The parent loses its settlement, so the call runs again.
			crash = &crashRecorder{
				Recorder: m,
				trigger: func(r dashi.CommitRequest) bool {
					return r.Step.TurnID == parentTurn &&
						r.Step.Kind == dashi.StepToolSettled
				},
			}
			return crash
		})
	if _, err := h.start(t.Context()); err == nil {
		t.Fatal("the crash did not stop the run")
	}
	sends := h.client.sends("child")
	crash.trigger = nil
	crash.dead.Store(false)
	out, err := h.resume(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResult(t, out.TurnView); !strings.Contains(got, "failed") {
		t.Fatalf("task result=%q", got)
	}
	if h.client.sends("child") != sends {
		t.Fatal("the retry ran the failed child again")
	}
}

func TestTaskRejectsUnknownSubagent(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "parent" && len(in.Messages) == 1 {
			return toolCall(
				callID,
				delegate.ToolName,
				`{"subagent":"nope","prompt":"x","description":"x"}`,
			), nil
		}
		return script("")(ctx, in)
	}
	h := newHarness(t, handle, delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The enum makes the runner refuse the arguments before the tool runs.
	view := out.Tools[callID]
	if view.Status != dashi.ToolSettled || h.client.sends("child") != 0 {
		t.Fatalf("status=%v child sends=%d", view.Status,
			h.client.sends("child"))
	}
}

func TestParallelTaskCallsGetOwnChildren(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		switch {
		case role(in) == "parent" && len(in.Messages) == 1:
			r := taskCall("first")
			second := toolCall(
				"call-2",
				delegate.ToolName,
				`{"subagent":"scout","prompt":"second","description":"b"}`,
			)
			r.Message.Content = append(
				r.Message.Content,
				second.Message.Content...,
			)
			return r, nil
		case role(in) == "parent":
			return text("parent done"), nil
		}
		return text("answer to " + in.Messages[0].Text()), nil
	}
	h := newHarness(t, handle, delegate.Config{}, nil)
	h.runner.Agent.Limits.ToolConcurrency = 2
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	for id, want := range map[string]string{
		"call-1": "answer to first",
		"call-2": "answer to second",
	} {
		v := h.view(t, delegate.ChildID(parentTurn, id))
		if v.Answer != want || v.Parent.CallID != id {
			t.Fatalf("child %s: answer=%q parent=%+v", id, v.Answer, v.Parent)
		}
	}
	report, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil || report.Children.CostUnknown != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestChildIDIsPureAndCollisionFree(t *testing.T) {
	if delegate.ChildID("a", "b") != delegate.ChildID("a", "b") {
		t.Fatal("ChildID is not deterministic")
	}
	pairs := [][2]string{
		{"a", "b:c"}, {"a:b", "c"}, {"a", "bc"}, {"ab", "c"},
		{"", "abc"}, {"abc", ""}, {"a", "b"},
	}
	seen := map[string][2]string{}
	for _, p := range pairs {
		id := delegate.ChildID(p[0], p[1])
		if prev, dup := seen[id]; dup {
			t.Fatalf("%v and %v share ID %q", prev, p, id)
		}
		seen[id] = p
	}
}

func TestDefaultCatalogHasExploreAndGeneral(t *testing.T) {
	tools := delegate.New(delegate.Config{})
	list, err := tools.Tools(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("tools=%d err=%v", len(list), err)
	}
	schema := string(list[0].Definition.Schema)
	if !strings.Contains(schema, `"explore","general"`) {
		t.Fatalf("schema=%s", schema)
	}
}

// countingRecorder counts the child turns it starts.
type countingRecorder struct {
	dashi.Recorder
	starts atomic.Int32
}

func (c *countingRecorder) Load(
	ctx context.Context,
	id string,
) (dashi.Turn, error) {
	return c.Recorder.(dashi.Loader).Load(ctx, id)
}

func (c *countingRecorder) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	if req.Step.Kind == dashi.StepStarted &&
		strings.HasPrefix(req.Step.TurnID, "child-") {
		c.starts.Add(1)
	}
	return c.Recorder.Commit(ctx, req)
}

// A host that reopens a session gives the runner a new recorder. The task
// tool was built earlier, yet the child must use the new recorder, find the
// child the first run started, and continue it.
func TestReopenedSessionResumesSameChild(t *testing.T) {
	started := make(chan struct{}, 1)
	var release atomic.Bool
	first := &countingRecorder{}
	h := newHarness(
		t,
		blockingChild(started, &release),
		delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			first.Recorder = m
			return first
		},
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(ctx)
		done <- out
	}()
	<-started
	cancel()
	if out := <-done; out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	h.waitStop(t, childID(), dashi.StopInterrupted)
	if first.starts.Load() != 1 {
		t.Fatalf("first run started %d children", first.starts.Load())
	}

	// Reopen: the same store behind a new recorder object.
	second := &countingRecorder{Recorder: h.rec}
	h.runner.Recorder = second
	release.Store(true)
	resumed, err := h.resume(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("resumed parent stop=%+v", resumed.Stop)
	}
	if n := second.starts.Load(); n != 0 {
		t.Fatalf("reopened session started %d new children, want 0", n)
	}
	if got := toolResult(t, resumed.TurnView); got != "finished after resume" {
		t.Fatalf("task result=%q", got)
	}
	child := h.view(t, childID())
	if child.ModelAttempts != 1 || child.Interruptions != 1 {
		t.Fatalf("child attempts=%d interruptions=%d",
			child.ModelAttempts, child.Interruptions)
	}

	// Parent: two settled calls at 1 each. Child: one settled call at 0.25.
	report, err := delegate.Report(t.Context(), second, resumed.TurnView)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Children.Cost["USD"].String(); got != "0.25" {
		t.Fatalf("children cost=%s, want 0.25", got)
	}
	if got := report.Inclusive.Cost["USD"].String(); got != "2.25" {
		t.Fatalf("inclusive cost=%s, want 2.25", got)
	}
}

// A child uses the parent's current model unless its Subagent sets one.
func TestChildInheritsParentModel(t *testing.T) {
	sub := scout()
	sub.Agent.Provider, sub.Agent.API, sub.Agent.Model = "", "", ""
	h := newHarness(t, script("x"),
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	h.runner.Agent.Model = "chosen-by-user"
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.view(t, childID()).Agent.Model; got != "chosen-by-user" {
		t.Fatalf("recorded child model=%q", got)
	}
}

func TestSubagentModelOverridesParent(t *testing.T) {
	sub := scout()
	sub.Agent.Model = "cheap"
	h := newHarness(t, script("x"),
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	h.runner.Agent.Model = "chosen-by-user"
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.view(t, childID()).Agent.Model; got != "cheap" {
		t.Fatalf("child model=%q, want cheap", got)
	}
}

// A crash during a child tool call, after the child's first model call
// settled, must not repeat that model call.
func TestCrashDuringChildToolCallRepeatsNoModelCall(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "child" {
			return script("")(ctx, in)
		}
		if len(in.Messages) == 1 {
			return costed(toolCall("c1", "read", `{}`), "0.25"), nil
		}
		return costed(text("child finished"), "0.25"), nil
	}
	var crash *crashRecorder
	h := newHarness(t, handle, delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			crash = &crashRecorder{Recorder: m, trigger: func(
				r dashi.CommitRequest,
			) bool {
				return strings.HasPrefix(r.Step.TurnID, "child-") &&
					r.Step.Kind == dashi.StepToolSettled
			}}
			return crash
		})
	if _, err := h.start(t.Context()); err == nil {
		t.Fatal("the crash did not stop the run")
	}
	crash.trigger = nil
	crash.dead.Store(false)
	out, err := h.resume(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResult(t, out.TurnView); got != "child finished" {
		t.Fatalf("task result=%q", got)
	}
	// One call asked for the tool and one answered. Neither ran twice.
	if got := h.client.sends("child"); got != 2 {
		t.Fatalf("child model calls=%d, want 2", got)
	}
	if got := crash.starts.Load(); got != 1 {
		t.Fatalf("child turns started=%d, want 1", got)
	}
}

// A nil Recorder is a supported mode. The child runs and the parent gets the
// answer.
func TestTaskWithoutHostRecorderReturnsChildAnswer(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := newHarness(t, script("found it"), delegate.Config{}, nil)
	h.runner.Recorder = nil
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	if got := toolResult(t, out.TurnView); got != "found it" {
		t.Fatalf("task result=%q", got)
	}
	if n := strings.Count(logs.String(), "without a host recorder"); n != 1 {
		t.Fatalf("warnings=%d, want 1: %q", n, logs.String())
	}
}

// With a nil Recorder, task is not marked idempotent. An interrupted resume
// reports the call as uncertain and starts no second child.
func TestTaskWithoutHostRecorderResumeIsUncertain(t *testing.T) {
	started := make(chan struct{}, 1)
	var release atomic.Bool
	h := newHarness(t, blockingChild(started, &release), delegate.Config{}, nil)
	h.runner.Recorder = nil

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(ctx)
		done <- out
	}()
	<-started
	cancel()
	out := <-done
	if out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	before := h.client.sends("child")

	release.Store(true)
	resumed, err := h.runner.Run(t.Context(), out.Turn, dashi.Continue{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind == dashi.StopCompleted {
		t.Fatalf("resume completed; want an uncertain call: %+v", resumed.Stop)
	}
	if got := h.client.sends("child"); got != before {
		t.Fatalf("child model calls=%d, want %d", got, before)
	}
	if st := resumed.Tools[callID].Status; st != dashi.ToolUncertain {
		t.Fatalf("task call status=%s, want uncertain", st)
	}
}

// A child with an output schema returns its structured value.
func TestTaskReturnsStructuredChildOutput(t *testing.T) {
	sub := scout()
	sub.Agent.OutputSchema = json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"integer"}},` +
			`"required":["n"],"additionalProperties":false}`)
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "child" {
			return script("")(ctx, in)
		}
		return costed(
			toolCall("s1", dashi.SubmitOutputTool, `{"n":7}`), "0.25"), nil
	}
	h := newHarness(t, handle,
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResult(t, out.TurnView); got != `{"n":7}` {
		t.Fatalf("task result=%q", got)
	}
}
