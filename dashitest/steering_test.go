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
					Agent:    testAgent(),
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
// resolves it: returned once whether or not the write landed, since no
// model call saw it.
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
				Agent:    testAgent(),
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
			if err != nil || len(left) != 1 || left[0].Item.ID != id {
				t.Fatalf("leftovers = %+v err=%v", left, err)
			}
			if again, _ := inbox.Leftovers(saved); len(again) != 0 {
				t.Fatalf("leftovers returned twice: %+v", again)
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
