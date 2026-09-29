package dashitest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// steerTable is how a durable host keeps steers: rows in its own table,
// keyed by turn, consumed when Dashi acknowledges them. A host may instead
// consume them in the recorder transaction that saves the step, reading the
// IDs from Step.Items.
type steerTable struct {
	mu   sync.Mutex
	rows map[string][]dashi.Steer
}

func (s *steerTable) Steers(_ context.Context, turnID string) ([]dashi.Steer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rows[turnID]), nil
}

func (s *steerTable) Delivered(_ context.Context, turnID string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[turnID] = slices.DeleteFunc(s.rows[turnID], func(row dashi.Steer) bool {
		return slices.Contains(ids, row.Item.ID)
	})
	return nil
}

func (s *steerTable) len(turnID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows[turnID])
}

func steerRow(id, text string) dashi.Steer {
	return dashi.Steer{Item: dashi.ContextItem{
		ID:      id,
		Message: llm.UserMessage(llm.Text(text)),
	}}
}

// recordedSteers counts how many times the record holds each steer ID.
func recordedSteers(turn dashi.Turn) map[string]int {
	out := map[string]int{}
	for _, step := range turn.Steps {
		for _, item := range step.Items() {
			if item.ID != "" {
				out[item.ID]++
			}
		}
	}
	return out
}

// TestDurableSteeringSurvivesEachCommitFault fails the commit that records a
// steer, and the commits around it, in each way a durable store can. A new
// worker resumes from the reloaded record with the same steer table. The
// steer must end up recorded exactly once, seen by the model, and consumed
// from the table; it is never lost and never delivered twice.
func TestDurableSteeringSurvivesEachCommitFault(t *testing.T) {
	// The record starts: start, the steer's delivery at 2, the first model
	// intent at 3 and its settlement at 4.
	targets := []struct {
		kind dashi.StepKind
		seq  int64
	}{
		{kind: dashi.StepDelivered, seq: 2},
		{kind: dashi.StepModelIntent, seq: 3},
		{kind: dashi.StepStopped},
	}
	faults := []dashitest.Fault{
		dashitest.Reject,
		dashitest.LoseWrite,
		dashitest.LoseAck,
	}
	for _, target := range targets {
		for _, fault := range faults {
			name := fmt.Sprintf("%s@%d/%s", target.kind, target.seq, fault)
			t.Run(name, func(t *testing.T) {
				store := dashi.NewMemoryRecorder()
				rec := &dashitest.FaultRecorder{
					Recorder: store,
					Kind:     target.kind,
					Seq:      target.seq,
					Fault:    fault,
				}
				table := &steerTable{rows: map[string][]dashi.Steer{
					"t1": {steerRow("s1", "use the staging database")},
				}}
				model := &steerModel{}
				runner := dashi.Runner{
					Client:   model,
					Tools:    &countingTool{},
					Recorder: rec,
				}
				_, err := runner.Run(
					t.Context(),
					dashi.Turn{ID: "t1"},
					startCommand(),
					dashi.WithSteering(table),
				)
				if _, ok := rec.Fired(); !ok {
					t.Fatal("the fault did not fire")
				}
				if err == nil {
					t.Fatal("the fault was not reported")
				}
				saved, err := store.Load(t.Context(), "t1")
				if err != nil {
					t.Fatal(err)
				}
				out := resume(
					t,
					saved,
					startCommand(),
					runner,
					dashi.WithSteering(table),
				)
				if out.Stop.Kind != dashi.StopCompleted {
					t.Fatalf("resumed stop is %+v; want completed", out.Stop)
				}
				final, err := store.Load(t.Context(), "t1")
				if err != nil {
					t.Fatal(err)
				}
				if got := recordedSteers(final)["s1"]; got != 1 {
					t.Fatalf("steer recorded %d times; want once", got)
				}
				if table.len("t1") != 0 {
					t.Fatal("the recorded steer was never consumed")
				}
				if !model.sawSteer() {
					t.Fatal("no model call saw the steer")
				}
			})
		}
	}
}

// TestInboxKeepsInDoubtSteersForTheirTurn fails the write that records a
// steer so its outcome is unknown. The steer is in doubt: it cannot be
// cancelled and no other turn receives it. Leftovers on the reloaded turn
// resolves it: returned once when the write did not land, and not at all
// when it did, since the turn's history carries it.
func TestInboxKeepsInDoubtSteersForTheirTurn(t *testing.T) {
	for _, fault := range []dashitest.Fault{dashitest.LoseWrite, dashitest.LoseAck} {
		t.Run(string(fault), func(t *testing.T) {
			store := dashi.NewMemoryRecorder()
			rec := &dashitest.FaultRecorder{
				Recorder: store,
				Kind:     dashi.StepDelivered,
				Fault:    fault,
			}
			inbox := dashi.NewInbox()
			id, err := inbox.SendText("wrong file")
			if err != nil {
				t.Fatal(err)
			}
			runner := dashi.Runner{
				Client:   &steerModel{},
				Tools:    &countingTool{},
				Recorder: rec,
			}
			if _, err := runner.Run(
				t.Context(),
				dashi.Turn{ID: "t1"},
				startCommand(),
				dashi.WithSteering(inbox),
			); err == nil {
				t.Fatal("the fault was not reported")
			}
			if inbox.Cancel(id) {
				t.Fatal("an in-doubt steer was cancelled")
			}
			other, err := inbox.Steers(t.Context(), "t2")
			if err != nil || len(other) != 0 {
				t.Fatalf("another turn received the in-doubt steer: %+v", other)
			}
			saved, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			left, err := inbox.Leftovers(saved)
			if err != nil {
				t.Fatal(err)
			}
			if fault == dashitest.LoseAck {
				if len(left) != 0 {
					t.Fatalf("a recorded steer was returned: %+v", left)
				}
			} else if len(left) != 1 || left[0].Item.ID != id {
				t.Fatalf("leftovers = %+v", left)
			}
			if again, _ := inbox.Leftovers(saved); len(again) != 0 {
				t.Fatalf("second leftovers = %+v", again)
			}
		})
	}
}

// steerModel is scriptedModel that also notes whether a request carried the
// steer.
type steerModel struct {
	scriptedModel
	mu  sync.Mutex
	saw bool
}

func (m *steerModel) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	for _, msg := range call.Request.Snapshot().Input.Messages {
		if strings.Contains(msg.Text(), "staging") {
			m.mu.Lock()
			m.saw = true
			m.mu.Unlock()
		}
	}
	return m.scriptedModel.Send(ctx, call)
}

func (m *steerModel) sawSteer() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saw
}

// draftModel answers "draft" and, during that first call, has a person
// send a correction through the inbox. Once a request carries the
// correction it answers "final". Deciding from the input keeps a resent
// request consistent.
type draftModel struct {
	scriptedModel
	inbox *dashi.Inbox
	mu    sync.Mutex
	sent  bool
	saw   bool
}

func (m *draftModel) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	m.mu.Lock()
	corrected := false
	for _, msg := range call.Request.Snapshot().Input.Messages {
		if strings.Contains(msg.Text(), "metric") {
			corrected = true
		}
	}
	if corrected {
		m.saw = true
	}
	send := !m.sent && !corrected
	m.sent = m.sent || send
	m.mu.Unlock()
	if send {
		if _, err := m.inbox.SendText("use metric units"); err != nil {
			return llm.Result{}, err
		}
	}
	text := "draft"
	if corrected {
		text = "final"
	}
	return llm.Result{
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Message:   llm.AssistantMessage(llm.Text(text)),
		Stop:      llm.Stop{Kind: llm.StopFinished},
	}, nil
}

// TestSteerSupersedingTextSurvivesEachCommitFault fails the loop
// continuation that records a steer superseding a text answer, in each way
// a durable store can, then resumes from the reloaded record with the same
// inbox. The steer is recorded once, the model sees it, and nothing is left
// over.
func TestSteerSupersedingTextSurvivesEachCommitFault(t *testing.T) {
	for _, fault := range []dashitest.Fault{
		dashitest.Reject,
		dashitest.LoseWrite,
		dashitest.LoseAck,
	} {
		t.Run(string(fault), func(t *testing.T) {
			store := dashi.NewMemoryRecorder()
			rec := &dashitest.FaultRecorder{
				Recorder: store,
				Kind:     dashi.StepLoopContinued,
				Fault:    fault,
			}
			inbox := dashi.NewInbox()
			model := &draftModel{inbox: inbox}
			runner := dashi.Runner{Client: model, Recorder: rec}
			if _, err := runner.Run(
				t.Context(),
				dashi.Turn{ID: "t1"},
				startCommand(),
				dashi.WithSteering(inbox),
			); err == nil {
				t.Fatal("the fault was not reported")
			}
			saved, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			out := run(
				t,
				saved,
				dashi.Continue{CommandID: "resume"},
				runner,
				dashi.WithSteering(inbox),
			)
			if out.Stop.Kind != dashi.StopCompleted || out.Answer != "final" {
				t.Fatalf("stop=%+v answer=%q", out.Stop, out.Answer)
			}
			final, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			if got := recordedSteers(final); len(got) != 1 {
				t.Fatalf("steers recorded: %v", got)
			}
			for id, n := range recordedSteers(final) {
				if n != 1 {
					t.Fatalf("steer %s recorded %d times", id, n)
				}
			}
			if !model.saw {
				t.Fatal("no model call saw the steer")
			}
			if left, _ := inbox.Leftovers(final); len(left) != 0 {
				t.Fatalf("leftovers = %+v", left)
			}
		})
	}
}

// TestInboxContinuesAfterInDoubtDelivery fails the write that delivers a
// steer before the first model call, with an unknown outcome, then
// continues the same turn with the same inbox. The in-doubt steer goes to
// that run and is recorded once.
func TestInboxContinuesAfterInDoubtDelivery(t *testing.T) {
	for _, fault := range []dashitest.Fault{dashitest.LoseWrite, dashitest.LoseAck} {
		t.Run(string(fault), func(t *testing.T) {
			store := dashi.NewMemoryRecorder()
			rec := &dashitest.FaultRecorder{
				Recorder: store,
				Kind:     dashi.StepDelivered,
				Fault:    fault,
			}
			inbox := dashi.NewInbox()
			if _, err := inbox.SendText("use the staging database"); err != nil {
				t.Fatal(err)
			}
			model := &steerModel{}
			runner := dashi.Runner{
				Client:   model,
				Tools:    &countingTool{},
				Recorder: rec,
			}
			if _, err := runner.Run(
				t.Context(),
				dashi.Turn{ID: "t1"},
				startCommand(),
				dashi.WithSteering(inbox),
			); err == nil {
				t.Fatal("the fault was not reported")
			}
			saved, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			out := run(
				t,
				saved,
				dashi.Continue{CommandID: "resume"},
				runner,
				dashi.WithSteering(inbox),
			)
			if out.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("stop=%+v", out.Stop)
			}
			final, err := store.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			steers := recordedSteers(final)
			if len(steers) != 1 {
				t.Fatalf("steers recorded: %v", steers)
			}
			for id, n := range steers {
				if n != 1 {
					t.Fatalf("steer %s recorded %d times", id, n)
				}
			}
			if !model.sawSteer() {
				t.Fatal("no model call saw the steer")
			}
			if left, _ := inbox.Leftovers(final); len(left) != 0 {
				t.Fatalf("leftovers = %+v", left)
			}
		})
	}
}
