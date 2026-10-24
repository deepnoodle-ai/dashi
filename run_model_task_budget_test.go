package dashi

import (
	"context"
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
		want, err := turnTaskBudget(Turn{ID: turn.ID, Steps: turn.Steps[:i]}, &budget, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Total != want.Total ||
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
	replaced, err := replacedTurnTokens(out.Turn, nil)
	if err != nil || replaced == 0 {
		t.Fatalf("replaced = %d, err = %v", replaced, err)
	}
	got, err := turnTaskBudget(out.Turn, &llm.TaskBudget{Total: 100_000, Remaining: new(replaced + 7)}, nil)
	if err != nil || *got.Remaining != 7 {
		t.Fatalf("remaining = %v, err = %v", got.Remaining, err)
	}
	got, err = turnTaskBudget(out.Turn, &llm.TaskBudget{Total: 100_000, Remaining: new(1)}, nil)
	if err != nil || *got.Remaining != 0 {
		t.Fatalf("remaining = %v, err = %v", got.Remaining, err)
	}
	if got, _ := turnTaskBudget(out.Turn, nil, nil); got != nil {
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

// replacedTurnTokens counts each compaction's covered messages, which are
// exactly the Turn's messages no longer sent, and a memo spares a run from
// counting a compaction twice.
func TestReplacedTurnTokensCountsUnsentMessages(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: NewMemoryRecorder()}
	out, err := r.Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
	if err != nil || len(out.ContextCompactions) != 2 {
		t.Fatalf("err = %v, compactions = %d", err, len(out.ContextCompactions))
	}
	full, err := turnMessages(out.Turn, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := turnMessages(out.Turn, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[TurnMessageRef]bool{}
	for _, m := range sent {
		kept[m.Ref] = true
	}
	want := 0
	for _, m := range full {
		if kept[m.Ref] {
			continue
		}
		if m.Message.Role == llm.RoleAssistant {
			want += 2 // the fixture's recorded output tokens
			continue
		}
		want += llm.EstimatedMessageTokens([]llm.Message{m.Message})
	}
	memo := map[int64]int{}
	got, err := replacedTurnTokens(out.Turn, memo)
	if err != nil || got != want || len(memo) != 2 {
		t.Fatalf("replaced = %d, want %d, memo = %v, err = %v", got, want, memo, err)
	}
	for seq := range memo {
		memo[seq] = 1
	}
	if got, _ := replacedTurnTokens(out.Turn, memo); got != 2 {
		t.Fatalf("replaced with memo = %d, want the memoized 2", got)
	}
}

// minTaskBudgetClient fits each input to a 20,000-token task budget
// minimum, as the Messages client does, and records the fitted settings.
type minTaskBudgetClient struct{ *activeClient }

func (c minTaskBudgetClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	fitted, adjustments, err := llm.Capabilities{MinTaskBudget: 20_000}.Fit(in, llm.CapabilityAdjust, nil)
	if err != nil {
		return llm.Request{}, err
	}
	req, err := c.activeClient.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snapshot := req.Snapshot()
	snapshot.Effective = fitted.Settings
	snapshot.Adjustments = adjustments
	return llm.NewRequest(snapshot)
}

// A budget below the provider minimum behaves as the minimum would across
// compactions: the raised countdown never drops by the difference, also when
// the replaced tokens exceed the smaller Total. Each recorded budget is what
// a replay of the Turn gives.
func TestTaskBudgetBelowMinimumAcrossCompaction(t *testing.T) {
	sentBudgets := func(t *testing.T, total, replyTokens int) []llm.TaskBudget {
		c, a := activeFixture()
		for i := range c.results {
			c.results[i].Usage.OutputTokens = replyTokens
		}
		budget := llm.TaskBudget{Total: total}
		a.Settings.TaskBudget = &budget
		calls := 0
		r := Runner{Client: minTaskBudgetClient{c}, Tools: activeTools(&calls)}
		out, err := r.Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
		if err != nil || out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 2 {
			t.Fatalf("err=%v stop=%+v compactions=%d", err, out.Stop, len(out.ContextCompactions))
		}
		var budgets []llm.TaskBudget
		for i, step := range out.Turn.Steps {
			if step.Kind != StepModelCallStarted || step.Model.Purpose != "" {
				continue
			}
			got := *step.Model.Effective.TaskBudget
			budgets = append(budgets, got)
			if got.Remaining == nil {
				continue
			}
			want, err := turnTaskBudget(Turn{ID: out.Turn.ID, Steps: out.Turn.Steps[:i]}, &budget, nil)
			if err != nil || want.Total != got.Total || *want.Remaining != *got.Remaining {
				t.Fatalf("step %d: sent %+v, replay gives %+v, err = %v", i, got, want, err)
			}
		}
		return budgets
	}
	for _, tc := range []struct {
		name        string
		replyTokens int
	}{
		{"replaced below the smaller total", 2},
		// Four replaced replies of 1,500 output tokens and about 9,000
		// tokens of tool results exceed 10,000.
		{"replaced above the smaller total", 1_500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			low, minimum := sentBudgets(t, 10_000, tc.replyTokens), sentBudgets(t, 20_000, tc.replyTokens)
			if len(low) != 5 || len(low) != len(minimum) {
				t.Fatalf("calls = %d and %d", len(low), len(minimum))
			}
			for i := range low {
				if low[i].Total != 20_000 || (low[i].Remaining == nil) != (minimum[i].Remaining == nil) ||
					low[i].Remaining != nil && *low[i].Remaining != *minimum[i].Remaining {
					t.Fatalf("call %d: sent %+v for 10,000, %+v for 20,000", i, low[i], minimum[i])
				}
			}
			last := low[4].Remaining
			if last == nil || *last >= 20_000 || *last == 0 {
				t.Fatalf("last call remaining = %v", last)
			}
			if tc.replyTokens > 2 && *last >= 10_000 {
				t.Fatalf("last call remaining = %d, want replaced tokens above 10,000", *last)
			}
		})
	}
}

// A BeforeModel hook that lowers Total below the derived Remaining gets
// Remaining lowered to its Total, with a warning, instead of failing.
func TestHookLowersTaskBudgetTotal(t *testing.T) {
	c, a := activeFixture()
	a.Settings.TaskBudget = &llm.TaskBudget{Total: 100_000}
	calls := 0
	lower := ModelHook{Name: "lower", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		s := p.Input.Settings
		if s.TaskBudget == nil {
			return ModelDecision{}, nil
		}
		s.TaskBudget.Total = 30_000
		return ModelDecision{Settings: &s}, nil
	}}
	r := Runner{Client: c, Tools: activeTools(&calls), Hooks: lower.Hooks()}
	out, err := r.Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("err=%v stop=%+v", err, out.Stop)
	}
	clamped := 0
	for _, step := range out.Turn.Steps {
		if step.Kind != StepModelCallStarted || step.Model.Purpose != "" {
			continue
		}
		b := step.Model.Effective.TaskBudget
		if b.Total != 30_000 || b.Remaining != nil && *b.Remaining != 30_000 {
			t.Fatalf("sent %+v", b)
		}
		if b.Remaining != nil {
			clamped++
		}
	}
	warned := 0
	for _, w := range out.Warnings {
		if w.Code == WarningTaskBudgetAdjusted {
			warned++
		}
	}
	// A hook pass whose call gave way to a compaction warns too.
	if clamped != 3 || warned < clamped {
		t.Fatalf("clamped = %d, warnings = %+v", clamped, out.Warnings)
	}
	// A Remaining the caller sets out of range is still an error on a
	// direct call.
	in := llm.Input{Model: "m", Settings: llm.Settings{TaskBudget: &llm.TaskBudget{Total: 30_000, Remaining: new(40_000)}}}
	if err := in.Validate(); err == nil {
		t.Fatal("an out-of-range Remaining passed validation")
	}
}

// A Remaining that cannot be derived sends the agent's budget with a
// warning instead of failing the call.
// A compaction whose replaced messages counted no tokens still sends
// Remaining: the provider no longer sees those messages.
func TestTurnTaskBudgetSendsRemainingAfterZeroTokenCompaction(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "budget"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	memo := map[int64]int{}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepContextCompacted {
			memo[step.Seq] = 0
		}
	}
	if len(memo) == 0 {
		t.Fatal("fixture made no compaction")
	}
	got, err := turnTaskBudget(out.Turn, &llm.TaskBudget{Total: 30_000}, memo)
	if err != nil || got.Remaining == nil || *got.Remaining != 30_000 {
		t.Fatalf("budget = %+v, err = %v", got, err)
	}
}

func TestCallTaskBudgetWarnsWhenRemainingUnknown(t *testing.T) {
	broken := Turn{ID: "broken", Steps: []Step{{
		Seq:       1,
		Kind:      StepContextCompacted,
		Compacted: &ContextCompactionFact{ModelCallID: "missing", Summary: "s"},
	}}}
	budget := &llm.TaskBudget{Total: 30_000, Remaining: new(25_000)}
	got, warnings := callTaskBudget(broken, budget, nil)
	if got == budget || got.Total != 30_000 || *got.Remaining != 25_000 ||
		len(warnings) != 1 || warnings[0].Code != WarningTaskBudgetAdjusted {
		t.Fatalf("budget = %+v, warnings = %+v", got, warnings)
	}
}
