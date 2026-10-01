package dashi

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func usageOverflowReplies() []llm.Result {
	replies := []llm.Result{
		callResult("lookup-1", "lookup"),
		callResult("lookup-2", "lookup"),
		final("done"),
	}
	for i := range replies {
		count := 1
		if i == 0 {
			count = math.MaxInt
		}
		replies[i].Usage = &llm.Usage{
			InputTokens:      1,
			OutputTokens:     1,
			Requests:         map[string]int{llm.MeterWebSearch: count},
			RequestsComplete: true,
		}
		replies[i].Cost = &llm.Cost{
			Total:    amount("0.25"),
			Currency: "USD",
			Source:   llm.CostSourceProviderReported,
		}
	}
	return replies
}

func TestBudgetIncompleteUsageEndsTokenCappedTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget Budget
		kind   LimitKind
	}{
		{"output", Budget{MaxOutputTokens: 100}, LimitOutputTokens},
		{"total", Budget{MaxTotalTokens: 100}, LimitTotalTokens},
		{
			"both", Budget{MaxOutputTokens: 100, MaxTotalTokens: 100},
			LimitOutputTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newPricedClient(usageOverflowReplies()...)
			out := runBudgetTurn(t, client, &tc.budget,
				llm.Settings{MaxTokens: 10}, tool.List{budgetLookup(nil)}, Hooks{})
			requireBudgetLimit(t, out, tc.kind)
			if !out.Stop.Ended || !out.Spend.UsageIncomplete {
				t.Fatal("uncheckable token cap did not end the turn")
			}
			if out.ReadyToContinue || out.Next() != NextNone {
				t.Fatal("host was directed to continue an ended turn")
			}
			if client.sends != 2 {
				t.Fatalf("call after incomplete usage was sent: %d", client.sends)
			}
			if out.Tools["lookup-2"].Outcome.State != tool.StateNotExecuted {
				t.Fatal("another tool ran after token accounting failed")
			}
			if out.Spend.CostUnknown != 0 ||
				out.Spend.Cost["USD"] != amount("0.5") {
				t.Fatal("token cap failure changed independent model charges")
			}
			if !strings.Contains(out.Stop.Reason, "start a new turn") {
				t.Fatal("stop does not explain the host's next action")
			}
			view, err := Replay(out.Turn)
			if err != nil || !view.Stop.Ended || view.Next() != NextNone {
				t.Fatalf("recorded terminal stop changed in replay: %v", err)
			}
			raise := tc.budget.Clone()
			if raise.MaxOutputTokens > 0 {
				raise.MaxOutputTokens++
			}
			if raise.MaxTotalTokens > 0 {
				raise.MaxTotalTokens++
			}
			_, err = (Runner{Client: client}).Run(t.Context(), out.Turn,
				Continue{CommandID: "raise", Budget: &raise})
			if !errors.Is(err, ErrTurnEnded) {
				t.Fatalf("budget raise revived an ended turn: %v", err)
			}
		})
	}
}

func TestBudgetIncompleteUsagePreservesExactCostAdmission(t *testing.T) {
	for _, budget := range []*Budget{
		nil,
		{MaxCost: &Money{Amount: amount("1"), Currency: "USD"}},
	} {
		client := newPricedClient(usageOverflowReplies()...)
		def := testAgent()
		def.Limits.ModelCalls = 4
		def.Settings.MaxTokens = 10
		out, err := (Runner{
			Client: client,
			Tools:  tool.List{budgetLookup(nil)},
		}).Run(t.Context(), Turn{ID: "cost-only"}, Start{
			CommandID: "start",
			Agent:     def,
			Budget:    budget,
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Stop.Kind != StopCompleted || client.sends != 3 {
			t.Fatal("usage failure blocked independently exact monetary admission")
		}
		if !out.Spend.UsageIncomplete || out.Spend.CostUnknown != 0 {
			t.Fatal("continued execution lost the usage signal")
		}
		if out.Spend.Cost["USD"] != amount("0.75") {
			t.Fatal("exact independent model total changed")
		}
	}
}

func usageReconciliationRecord() Turn {
	turn := accountingRecord(llm.Usage{OutputTokens: 2})
	turn.Steps[0].Started.Budget = &Budget{MaxOutputTokens: 10}
	turn.Steps[1].Model.EstimatedInputTokens = math.MaxInt
	turn.Steps[2].Model.ResultUnknown = true
	turn.Steps[2].Model.Error = "connection lost"
	return turn
}

func appendAccountingStop(turn Turn, stop *Stop) Turn {
	return interruptionRecord(append(turn.Steps, Step{
		Kind: StepTurnStopped,
		Stop: stop,
	})...)
}

func reconciledUsageResult() llm.Result {
	result := final("resolved answer")
	result.Usage = &llm.Usage{InputTokens: 1, OutputTokens: 1}
	result.Cost = &llm.Cost{
		Total:    amount("0.5"),
		Currency: "USD",
		Source:   llm.CostSourceProviderReported,
	}
	return result
}

func TestBudgetReconciliationBeforeTokenStopRestoresAdmission(t *testing.T) {
	turn := appendAccountingStop(usageReconciliationRecord(),
		&Stop{Kind: StopResultUnknown})
	view, err := Replay(turn)
	if err != nil || !view.Spend.UsageIncomplete {
		t.Fatalf("fixture did not expose incomplete usage: %v", err)
	}
	result := reconciledUsageResult()
	client := &scriptedClient{}
	runner := Runner{Client: client}
	out, err := runner.Run(t.Context(), turn, Resolve{
		CommandID:   "reconcile",
		CallID:      "model-1",
		ModelResult: &result,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spend.UsageIncomplete || !out.ReadyToContinue ||
		out.Next() != NextContinue {
		t.Fatal("pre-stop reconciliation did not restore ordinary continuation")
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "continue"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("reconciled answer did not complete normally: %v", err)
	}
	if client.sends != 0 {
		t.Fatal("reconciliation unnecessarily dispatched another call")
	}
}

func TestBudgetLateReconciliationNeverRevivesEndedTurn(t *testing.T) {
	turn := usageReconciliationRecord()
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	stop := incompleteUsageStop(state)
	stop.Ended = true
	turn = appendAccountingStop(turn, stop)
	before, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	result := reconciledUsageResult()
	runner := Runner{Client: &scriptedClient{}}
	out, err := runner.Run(t.Context(), turn, Resolve{
		CommandID: "late", CallID: "model-1", ModelResult: &result,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spend.UsageIncomplete || out.Spend.Cost["USD"] != amount("0.5") {
		t.Fatal("late settlement did not correct reported accounting")
	}
	if !canonjson.SameValue(out.Stop, before.Stop) || out.Reply != before.Reply {
		t.Fatal("late accounting changed the ended stop or fixed answer")
	}
	if out.ReadyToContinue || out.Next() != NextNone {
		t.Fatal("late accounting revived continuation")
	}
	_, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "revive"})
	if !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("ended turn accepted continuation: %v", err)
	}
}

func TestReplayValidatesTokenTerminalFlagAtStopPrefix(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		turn := accountingRecord(llm.Usage{OutputTokens: 2})
		turn.Steps[0].Started.Budget = &Budget{MaxOutputTokens: 10}
		if incomplete {
			turn = usageReconciliationRecord()
		}
		stop := &Stop{
			Kind:  StopLimited,
			Limit: &LimitDetail{Kind: LimitOutputTokens, Limit: 10, Used: 2},
			Ended: incomplete,
		}
		valid := appendAccountingStop(turn, stop)
		if _, err := Replay(valid); err != nil {
			t.Fatalf("valid legacy/new token stop rejected: %v", err)
		}
		stop.Ended = !incomplete
		invalid := appendAccountingStop(turn, stop)
		if _, err := Replay(invalid); !errors.Is(err, ErrInvalidTurn) {
			t.Fatalf("invalid terminal flag accepted: %v", err)
		}
	}
}

func TestBudgetIncompleteSettlementRecoversToTerminalStop(t *testing.T) {
	turn := accountingRecord(
		llm.Usage{InputTokens: math.MaxInt},
		llm.Usage{OutputTokens: 1},
	)
	turn.Steps[0].Started.Budget = &Budget{MaxOutputTokens: 100}
	view, err := Replay(turn)
	if err != nil || !view.Spend.UsageIncomplete || !view.ReadyToContinue {
		t.Fatalf("settled prefix cannot reach stop recovery: %v", err)
	}
	client := &scriptedClient{}
	out, err := (Runner{Client: client}).Run(t.Context(), turn,
		Continue{CommandID: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || !out.Stop.Ended ||
		out.Stop.Limit.Kind != LimitOutputTokens || out.ReadyToContinue ||
		out.Next() != NextNone {
		t.Fatal("settled prefix did not recover to a terminal token stop")
	}
	if client.sends != 0 {
		t.Fatal("stop recovery dispatched new work")
	}
	if _, err := (Runner{Client: client}).Run(t.Context(), out.Turn,
		Continue{CommandID: "revive"}); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("accepted terminal stop revived: %v", err)
	}
}

func TestBudgetIncompleteIntentRecoversBeforeTokenStop(t *testing.T) {
	turn := accountingRecord(llm.Usage{InputTokens: math.MaxInt})
	turn.Steps[0].Started.Budget = &Budget{MaxOutputTokens: 100}
	intent := interruptionModelIntent("model-2")
	intent.Model.EstimatedInputTokens = 1
	intent.Model.Tools = []tool.Definition{{
		Name: "search",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test", API: "test.chat", Type: "web_search",
		},
	}}
	turn = interruptionRecord(append(turn.Steps, intent)...)
	view, err := Replay(turn)
	if err != nil || !view.Spend.UsageIncomplete || !view.ReadyToContinue {
		t.Fatalf("recorded intent cannot reach recovery: %v", err)
	}
	client := &scriptedClient{}
	out, err := (Runner{Client: client}).Run(t.Context(), turn,
		Continue{CommandID: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopResultUnknown || out.Stop.Ended {
		t.Fatal("token accounting ended the turn before dispatch reconciliation")
	}
	if len(out.Pending) != 1 || out.Pending[0].Kind != PendingModel {
		t.Fatal("unknown dispatched model effect disappeared")
	}
	if client.sends != 0 {
		t.Fatal("recovering unknown provider tool work dispatched another call")
	}
}
