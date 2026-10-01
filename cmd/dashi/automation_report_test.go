package main

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestAutomationProjectionCopiesAllowlist(t *testing.T) {
	v := dashi.TurnView{ID: "request", Revision: 5, Origin: dashi.StartUser, Answer: "public\nanswer\u2028",
		Agent:   dashi.Agent{Instructions: "secret-instructions", Provider: "https://user:password@host/v1?key=secret-query#secret-fragment", API: "fixture", Model: "recorded"},
		Output:  &dashi.StructuredOutput{Value: []byte(`{"public":true}`)},
		Pending: []dashi.Pending{{ID: "call", Kind: dashi.PendingApproval, Args: []byte(`{"secret":"arguments"}`), Tool: &dashi.ToolView{Call: tool.Call{Args: []byte(`{"secret":"tool"}`)}}}},
		Stop:    dashi.Stop{Kind: dashi.StopLimited, Limit: &dashi.LimitDetail{Kind: dashi.LimitCost, CostLimit: money.MustParseAmount("0.000000003"), Currency: "CREDIT"}},
		Spend: dashi.Spend{Usage: llm.Usage{InputTokens: 9, CacheReadInputTokens: 4, CacheCreationInputTokensUnavailable: true, Requests: map[string]int{"search": 2}}, UsageIncomplete: true,
			Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.075"), "CREDIT": money.MustParseAmount("2.000000003")}, ToolCost: map[string]money.Amount{"EUR": money.MustParseAmount("0.5")}}}
	got := projectAutomationTurn(v)
	v.Output.Value[2] = 'X'
	v.Spend.Cost["USD"] = 0
	v.Spend.Usage.Requests["search"] = 99
	v.Stop.Limit.Currency = "CHANGED"
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-instructions", "arguments", "password", "secret-query", "secret-fragment", "CHANGED"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("output contains %q: %s", secret, encoded)
		}
	}
	if got.Destination.ID != "https://host/v1" || got.Spend.ModelCost["USD"] != "0.075" || got.Spend.ModelCost["CREDIT"] != "2.000000003" || got.Spend.ToolCost["EUR"] != "0.5" {
		t.Fatalf("route/money=%+v", got)
	}
	if got.Spend.UsageState != "incomplete" || got.Spend.CostUnknown != 0 || got.Spend.CostPartial != 0 || !got.Spend.Usage.CacheCreationInputTokensUnavailable || got.Spend.Usage.Requests["search"] != 2 {
		t.Fatalf("completeness=%+v", got.Spend)
	}
}

func TestAutomationMixedStopsAndSignalPriority(t *testing.T) {
	target := func(id string, kind dashi.StopKind) automationAffected {
		return automationAffected{automationTarget: automationTarget{TurnID: id}, Stop: automationStop{Kind: string(kind)}}
	}
	input := []automationAffected{target("parent", dashi.StopCanceled), target("a-canceled", dashi.StopCanceled), target("b-refused", dashi.StopRefused), target("z-limited", dashi.StopLimited)}
	for repeat := 0; repeat < 3; repeat++ {
		got := rankAutomationAffected(input, "parent")
		if len(got) != 4 || got[0].TurnID != "z-limited" || !got[0].Primary || got[0].Rank != 3 || automationExitCode(0, false, false, got, true) != 4 {
			t.Fatalf("mixed=%+v", got)
		}
		withoutLimit := rankAutomationAffected(input[:3], "parent")
		if withoutLimit[0].TurnID != "b-refused" {
			t.Fatalf("without limit=%+v", withoutLimit)
		}
		if input[0].Primary || input[0].Rank != 0 {
			t.Fatal("input mutated")
		}
	}
	failed := append(slices.Clone(input), target("child-failed", dashi.StopFailed), target("parent-waiting", dashi.StopWaiting))
	if got := rankAutomationAffected(failed, "parent-waiting"); got[0].TurnID != "child-failed" || automationExitCode(0, false, false, got, true) != 1 {
		t.Fatalf("failure priority=%+v", got)
	}
	ties := rankAutomationAffected([]automationAffected{target("a", dashi.StopWaiting), target("parent", dashi.StopWaiting), target("b", dashi.StopUncertain)}, "parent")
	if ties[0].TurnID != "parent" || ties[1].TurnID != "a" || ties[2].TurnID != "b" {
		t.Fatalf("ties=%+v", ties)
	}
	for _, tc := range []struct {
		signal            int
		output, preflight bool
		want              int
	}{{130, true, true, 130}, {143, true, true, 143}, {0, true, true, 1}, {0, false, true, 2}, {0, false, false, 4}} {
		if got := automationExitCode(tc.signal, tc.output, tc.preflight, input, true); got != tc.want {
			t.Fatalf("exit=%d want=%d", got, tc.want)
		}
	}
	if automationExitCode(0, false, false, nil, false) != 3 || automationExitCode(0, false, false, nil, true) != 0 {
		t.Fatal("quiet exit")
	}
}

func TestAutomationWholeRecordTotalsDeduplicateAndKeepUnknowns(t *testing.T) {
	parent := automationSpendRecord{View: dashi.TurnView{ID: "parent", Revision: 7, Spend: dashi.Spend{Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.1")}}}}
	child := automationSpendRecord{Child: true, View: dashi.TurnView{ID: "child", Revision: 3, Spend: dashi.Spend{Usage: llm.Usage{OutputTokens: 2}, Estimated: 1, CostUnknown: 1, CostPartial: 1, UnpricedRequests: map[string]int{"web": 1}, ToolCost: map[string]money.Amount{"EUR": money.MustParseAmount("0.2")}}}}
	got, err := sumAutomationRecords([]automationSpendRecord{parent, child, parent, child}, nil, []string{"unreadable-child"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Covered) != 2 || got.Covered[0].TurnID != "child" || got.Parent.ModelCost["USD"] != "0.1" || got.Inclusive.ModelCost["USD"] != "0.1" || got.Inclusive.ToolCost["EUR"] != "0.2" {
		t.Fatalf("totals=%+v", got)
	}
	if got.Inclusive.UsageState != "incomplete" || got.Inclusive.CostUnknown != 1 || got.Inclusive.CostPartial != 1 || got.Inclusive.Estimated != 1 || got.Inclusive.UnpricedRequests["web"] != 1 || len(got.Inclusive.OmittedRecords) != 1 {
		t.Fatalf("unknowns=%+v", got.Inclusive)
	}
	// Stable sorting also keeps overflow retention independent of input order.
	large := automationSpendRecord{View: dashi.TurnView{ID: "a", Spend: dashi.Spend{Cost: map[string]money.Amount{"USD": money.Amount(math.MaxInt64)}}}}
	small := automationSpendRecord{View: dashi.TurnView{ID: "z", Spend: dashi.Spend{Cost: map[string]money.Amount{"USD": 1}}}}
	a, _ := sumAutomationRecords([]automationSpendRecord{large, small}, nil, nil)
	b, _ := sumAutomationRecords([]automationSpendRecord{small, large}, nil, nil)
	if a.Parent.ModelCost["USD"] != b.Parent.ModelCost["USD"] || a.Parent.CostUnknown != 1 {
		t.Fatal("unstable overflow report")
	}
}
