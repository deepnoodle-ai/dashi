package dashi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// checkTaskBudgets checks every model call recorded in turn: a summary call
// has no task budget; an ordinary call before any compaction has the agent's
// budget as is; one after a compaction has a Remaining below the last; and
// each recorded budget is what turnTaskBudget computes from the Turn as it
// stood when the call started, so a replay gives the same value. It returns
// the number of ordinary calls that carried a Remaining.
func checkTaskBudgets(t *testing.T, turn Turn, budget llm.TaskBudget) int {
	t.Helper()
	withRemaining := 0
	last := budget.Total
	for i, step := range turn.Steps {
		if step.Kind != StepModelCallStarted {
			continue
		}
		got := step.Model.Effective.TaskBudget
		if step.Model.Purpose == ModelPurposeCompaction {
			if got != nil {
				t.Fatalf("step %d: summary call has task budget %+v", i, got)
			}
			continue
		}
		want, err := turnTaskBudget(Turn{ID: turn.ID, Steps: turn.Steps[:i]}, &budget)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Total != budget.Total ||
			(got.Remaining == nil) != (want.Remaining == nil) ||
			got.Remaining != nil && *got.Remaining != *want.Remaining {
			t.Fatalf("step %d: task budget %+v, replay gives %+v", i, got, want)
		}
		if got.Remaining != nil {
			withRemaining++
			if *got.Remaining > last {
				t.Fatalf("step %d: remaining %d rose above %d", i, *got.Remaining, last)
			}
			last = *got.Remaining
		}
	}
	return withRemaining
}

func TestTaskBudgetAcrossActiveCompaction(t *testing.T) {
	c, a := activeFixture()
	budget := llm.TaskBudget{Total: 100_000}
	a.Settings.TaskBudget = &budget
	calls := 0
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: NewMemoryRecorder()}
	out, err := r.Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 2 {
		t.Fatalf("err=%v stop=%+v compactions=%d", err, out.Stop, len(out.ContextCompactions))
	}
	// The three ordinary calls after the first compaction carry a
	// Remaining; the two before it do not.
	if n := checkTaskBudgets(t, out.Turn, budget); n != 3 {
		t.Fatalf("calls with remaining = %d, want 3", n)
	}
	// Each compaction replaced two exchanges of about 2,250 estimated tokens
	// of tool result each, plus the two-token replies.
	sent := out.Turn.Steps[len(out.Turn.Steps)-1]
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallStarted && step.Model.Purpose == "" {
			sent = step
		}
	}
	remaining := *sent.Model.Effective.TaskBudget.Remaining
	if remaining > budget.Total-4*2_000 || remaining < budget.Total-4*3_000 {
		t.Fatalf("remaining after two compactions = %d", remaining)
	}
	if a.Settings.TaskBudget.Remaining != nil {
		t.Fatal("the run changed the agent's task budget")
	}

	// A reloaded Turn computes the same values.
	data, _ := json.Marshal(out.Turn)
	var restored Turn
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	checkTaskBudgets(t, restored, budget)
}

// The caller's Remaining is the starting point, and the result never falls
// below zero.
func TestTurnTaskBudgetStartsFromRemainingAndFloorsAtZero(t *testing.T) {
	c, a := activeFixture()
	a.Settings.TaskBudget = &llm.TaskBudget{Total: 100_000}
	calls := 0
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: NewMemoryRecorder()}
	out, err := r.Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := replacedTurnTokens(out.Turn)
	if err != nil || replaced == 0 {
		t.Fatalf("replaced = %d, err = %v", replaced, err)
	}
	got, err := turnTaskBudget(out.Turn, &llm.TaskBudget{Total: 100_000, Remaining: new(replaced + 7)})
	if err != nil || *got.Remaining != 7 {
		t.Fatalf("remaining = %v, err = %v", got.Remaining, err)
	}
	got, err = turnTaskBudget(out.Turn, &llm.TaskBudget{Total: 100_000, Remaining: new(1)})
	if err != nil || *got.Remaining != 0 {
		t.Fatalf("remaining = %v, err = %v", got.Remaining, err)
	}
	if got, _ := turnTaskBudget(out.Turn, nil); got != nil {
		t.Fatal("no budget became one")
	}
}

// A Turn recovered after a lost acknowledgement sends the same budgets as
// its record gives.
func TestTaskBudgetAfterRecovery(t *testing.T) {
	c, a := activeFixture()
	budget := llm.TaskBudget{Total: 100_000}
	a.Settings.TaskBudget = &budget
	calls := 0
	rec := NewMemoryRecorder()
	fault := &activeFault{Recorder: rec, kind: StepContextCompacted}
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: fault}
	if _, err := r.Run(t.Context(), Turn{ID: "recover"}, activeStart(a)); !isCommitError(err) {
		t.Fatalf("err = %v", err)
	}
	loaded, err := rec.Load(t.Context(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(t.Context(), loaded, Continue{CommandID: "continue"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("err=%v stop=%+v", err, out.Stop)
	}
	if n := checkTaskBudgets(t, out.Turn, budget); n == 0 {
		t.Fatal("no call after recovery carried a remaining budget")
	}
	for _, in := range c.sent {
		if strings.HasPrefix(in.System, activeSummaryPrompt) && in.Settings.TaskBudget != nil {
			t.Fatal("a summary call was sent with a task budget")
		}
	}
}

func isCommitError(err error) bool {
	_, ok := errors.AsType[*CommitError](err)
	return ok
}
