package dashi

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// amount keeps exact money literals short in budget tests.
func amount(s string) money.Amount {
	return money.MustParseAmount(s)
}

type pricedClient struct {
	scriptedClient
	pricing llm.Pricing
}

func newPricedClient(results ...llm.Result) *pricedClient {
	return &pricedClient{
		scriptedClient: scriptedClient{results: results},
		pricing: llm.Pricing{
			Currency:  "USD",
			UpdatedAt: "2026-09-28",
			Base: llm.Rates{
				Input:      amount("1"),
				Output:     amount("1"),
				CacheRead:  amount("1"),
				CacheWrite: amount("1"),
			},
		},
	}
}

func (c *pricedClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	info := llm.ModelInfo{
		Pricing: &c.pricing,
		Evidence: llm.ModelEvidence{
			Model:       in.Model,
			API:         "test.chat",
			Endpoint:    "https://example.test/model",
			Owner:       "test",
			Source:      "fixture",
			Method:      llm.EvidenceFixture,
			QualifiedAt: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		},
	}
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:            in,
		Effective:        in.Settings,
		Body:             []byte(`{"ok":true}`),
		ModelInfo:        &info,
		CapabilityPolicy: llm.CapabilityAdjust,
		// A preparer's own count, so cap fixtures do not depend on the
		// size of this placeholder body.
		EstimatedInputTokens: pricedInputTokens,
	})
}

// pricedInputTokens is the input estimate pricedClient reports for every
// request.
const pricedInputTokens = 1035

func runBudgetTurn(
	t *testing.T,
	client llm.Client,
	budget *Budget,
	settings llm.Settings,
	tools tool.Toolset,
	hooks Hooks,
) RunResult {
	t.Helper()
	out, err := runBudgetTurnErr(client, budget, settings, tools, hooks)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runBudgetTurnErr(
	client llm.Client,
	budget *Budget,
	settings llm.Settings,
	tools tool.Toolset,
	hooks Hooks,
) (RunResult, error) {
	def := testAgent()
	def.Settings = settings
	return (Runner{
		Client: client,
		Tools:  tools,
		Hooks:  hooks,
	}).Run(
		context.Background(),
		Turn{ID: "budget"},
		Start{
			CommandID: "start",
			Agent:     def,
			Budget:    budget,
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
}

func budgetLookup(cost *tool.Cost) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("done")),
				Cost:   cost,
			}, nil
		}),
	}
}

func requireBudgetLimit(
	t *testing.T,
	out RunResult,
	kind LimitKind,
) *LimitDetail {
	t.Helper()
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil {
		t.Fatalf("expected limited stop, got %+v", out.Stop)
	}
	if out.Stop.Limit.Kind != kind {
		t.Fatalf("limit kind=%q, want %q", out.Stop.Limit.Kind, kind)
	}
	return out.Stop.Limit
}

func TestBudgetCloneCopiesCostCap(t *testing.T) {
	budget := Budget{MaxCost: &Money{Amount: amount("1"), Currency: "USD"}}
	clone := budget.Clone()
	clone.MaxCost.Amount = amount("2")
	if budget.MaxCost.Amount != amount("1") {
		t.Fatal("budget clone shared its cost cap")
	}
}

func TestSpendCloneCopiesUsageAndCosts(t *testing.T) {
	spend := Spend{
		Usage:    llm.Usage{Requests: map[string]int{"web_search": 1}},
		Cost:     map[string]money.Amount{"USD": amount("1")},
		ToolCost: map[string]money.Amount{"EUR": amount("2")},
	}
	clone := spend.Clone()
	clone.Usage.Requests["web_search"] = 2
	clone.Cost["USD"] = amount("2")
	clone.ToolCost["EUR"] = amount("3")
	if spend.Usage.Requests["web_search"] != 1 {
		t.Fatal("spend clone shared provider tool request counts")
	}
	if spend.Cost["USD"] != amount("1") {
		t.Fatal("spend clone shared model cost totals")
	}
	if spend.ToolCost["EUR"] != amount("2") {
		t.Fatal("spend clone shared tool cost totals")
	}
}

func TestSpendAddCostRejectsOverflow(t *testing.T) {
	most := money.AmountFromNanos(math.MaxInt64)
	spend := Spend{Cost: map[string]money.Amount{"USD": most}}
	if err := addCurrencyCost(spend.Cost, "USD", 1); err == nil {
		t.Fatal("overflowing cumulative cost was accepted")
	}
	if spend.Cost["USD"] != most {
		t.Fatal("failed cost addition changed spend")
	}
}

func TestBudgetStopsUnpricedModelBeforeDispatch(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.5"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.UnpricedModel != "fixture" {
		t.Fatalf("unpriced model=%q", limit.UnpricedModel)
	}
	if client.sends != 0 {
		t.Fatalf("unpriced model was sent %d times", client.sends)
	}
	if !strings.Contains(out.Stop.Reason, "supply this model's prices") {
		t.Fatalf("reason=%q", out.Stop.Reason)
	}
}

func TestBudgetStopsWithoutFiniteOutputAllowance(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.5"), Currency: "USD"}},
		llm.Settings{},
		nil,
		Hooks{},
	)
	requireBudgetLimit(t, out, LimitCost)
	if client.sends != 0 {
		t.Fatalf("unbounded request sent %d calls", client.sends)
	}
	if !strings.Contains(out.Stop.Reason, "finite max_tokens") {
		t.Fatalf("reason=%q", out.Stop.Reason)
	}
}

func TestBudgetStopsWhenInputAloneExceedsCostCap(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.00001"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.SuggestedMaxOutputTokens != 0 {
		t.Fatalf("fit tokens=%d", limit.SuggestedMaxOutputTokens)
	}
	if client.sends != 0 {
		t.Fatalf("unaffordable input sent %d calls", client.sends)
	}
	if !strings.Contains(out.Stop.Reason, "reduce input/history") {
		t.Fatalf("reason=%q", out.Stop.Reason)
	}
}

func TestBudgetStopsForUnboundedProviderToolRequests(t *testing.T) {
	client := newPricedClient()
	input := llm.Input{
		Model:    "fixture",
		Settings: llm.Settings{MaxTokens: 100},
		Tools: []tool.Definition{
			{
				Name: "web_search",
				Kind: tool.KindProviderTool,
				ProviderTool: &tool.ProviderToolSpec{
					Provider: "test",
					API:      "test.chat",
					Type:     "web_search",
				},
			},
		},
	}
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	state := &replayState{
		TurnView: TurnView{
			Budget: &Budget{MaxCost: &Money{Amount: amount("0.5"), Currency: "USD"}},
		},
	}
	_, _, stop := checkSendBudget(state, request, "fixture")
	if stop == nil || stop.Limit == nil || stop.Limit.Kind != LimitCost {
		t.Fatalf("provider tool cost was recorded: stop=%+v", stop)
	}
	if !strings.Contains(stop.Reason, "remove provider tools") {
		t.Fatalf("reason=%q", stop.Reason)
	}
}

func TestBudgetCostShortfallDoesNotShrinkRequest(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.0015"), Currency: "USD"}},
		llm.Settings{MaxTokens: 1000},
		nil,
		Hooks{},
	)
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.SuggestedMaxOutputTokens <= 0 || limit.SuggestedMaxOutputTokens >= 1000 {
		t.Fatalf("fit tokens=%d", limit.SuggestedMaxOutputTokens)
	}
	if client.sends != 0 {
		t.Fatalf("shortfall sent %d calls", client.sends)
	}
	if client.inputs[0].Settings.MaxTokens != 1000 {
		t.Fatalf("max tokens was silently reduced: %+v", client.inputs[0].Settings)
	}
}

func TestBudgetClampsOutputAndRecordsCharge(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 40}
	client := newPricedClient(answer)
	out := runBudgetTurn(
		t,
		client,
		&Budget{
			MaxOutputTokens: 50,
			MaxCost:         &Money{Amount: amount("0.01"), Currency: "USD"},
		},
		llm.Settings{MaxTokens: 1000},
		nil,
		Hooks{},
	)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("effective max tokens=%d", client.inputs[0].Settings.MaxTokens)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Cost["USD"] != amount("0.00014") {
		t.Fatalf("cost=%s", view.Spend.Cost["USD"])
	}
	if view.Spend.Usage.OutputTokens != 40 {
		t.Fatalf("output tokens=%d", view.Spend.Usage.OutputTokens)
	}
	if view.Spend.CallsWithoutFinalUsage != 0 {
		t.Fatalf("calls without final usage=%d", view.Spend.CallsWithoutFinalUsage)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningBudgetClamp {
		t.Fatalf("warnings=%+v", view.Warnings)
	}
	callStart := out.Turn.Steps[1].Model
	charge := view.ModelCallCosts[0]
	if callStart.WorstCaseCost == nil || charge.Cost == nil {
		t.Fatalf("missing risk or charge: start=%+v charge=%+v", callStart, charge)
	}
	if callStart.WorstCaseCost.Total <= charge.Cost.Total {
		t.Fatalf("risk=%s charge=%s", callStart.WorstCaseCost.Total, charge.Cost.Total)
	}
	if charge.UsageSource != UsageSourceReported || !charge.Exact {
		t.Fatalf("reported usage not exact: %+v", charge)
	}
}

func TestValidationFailureKeepsFinishedProviderUsage(t *testing.T) {
	answer := final(strings.Repeat("x", 48))
	answer.Requested.Model = "different-model"
	answer.Usage = &llm.Usage{InputTokens: 7, OutputTokens: 1}
	client := newPricedClient(answer)
	out := runBudgetTurn(t, client, nil, llm.Settings{}, nil, Hooks{})
	if out.Stop.Kind != StopFailed {
		t.Fatalf("stop=%+v", out.Stop)
	}
	finished := out.Turn.Steps[2].Model
	if finished == nil || finished.Error == "" || finished.Result == nil ||
		finished.Result.Stop.Kind != llm.StopFinished {
		t.Fatalf("finish=%+v", finished)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelCallCosts[0].UsageSource != UsageSourceReported {
		t.Fatal("finished provider usage was not reported")
	}
	if view.Spend.Usage.InputTokens != 7 ||
		view.Spend.Usage.OutputTokens != 1 || view.Spend.CallsWithoutFinalUsage != 0 {
		t.Fatalf("spend=%+v", view.Spend)
	}
	wantCharge, ok := client.pricing.Estimate(answer.Usage)
	if !ok || !reflect.DeepEqual(view.ModelCallCosts[0].Cost, &wantCharge) {
		t.Fatalf("charge=%+v, want %+v", view.ModelCallCosts[0].Cost, wantCharge)
	}
}

func TestInvalidStopCountsReportedUsageAsCutOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind llm.StopKind
	}{
		{name: "missing"},
		{name: "invalid", kind: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := final(strings.Repeat("x", 48))
			answer.Stop.Kind = tc.kind
			answer.Usage = &llm.Usage{InputTokens: 7, OutputTokens: 1}
			out := runBudgetTurn(
				t,
				newPricedClient(answer),
				nil,
				llm.Settings{},
				nil,
				Hooks{},
			)
			if out.Stop.Kind != StopFailed {
				t.Fatalf("stop=%+v", out.Stop)
			}
			finished := out.Turn.Steps[2].Model
			if finished == nil || finished.Error == "" {
				t.Fatalf("finish=%+v", finished)
			}
			if got := ModelCallCostOf(out.Turn.Steps[1].Model, finished); got.UsageSource != UsageSourceCutOff {
				t.Fatalf("charge=%+v", got)
			}
			// Only the reported output counts; no visible-text estimate.
			if out.Spend.Usage.OutputTokens != 1 ||
				out.Spend.CallsWithoutFinalUsage != 1 {
				t.Fatalf("spend=%+v", out.Spend)
			}
		})
	}
}

func TestBudgetRaiseResumesAndKeepsAudit(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	second := final("done")
	second.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	client := newPricedClient(first, second)
	lookup := budgetLookup(nil)
	initial := &Budget{MaxCost: &Money{Amount: amount("0.0016"), Currency: "USD"}}
	def := testAgent()
	def.Settings.MaxTokens = 500
	rec := NewMemoryRecorder()
	runner := Runner{
		Client:   client,
		Tools:    tool.List{lookup},
		Recorder: rec,
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "raise"},
		Start{
			CommandID: "start",
			Agent:     def,
			Budget:    initial,
			Input:     []llm.Message{llm.UserMessage(llm.Text("lookup"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	requireBudgetLimit(t, out, LimitCost)
	if out.ReadyToContinue {
		t.Fatal("spent turn can continue without a raise")
	}
	if client.sends != 1 {
		t.Fatalf("first phase sent %d calls", client.sends)
	}
	raised := &Budget{MaxCost: &Money{Amount: amount("0.003"), Currency: "USD"}}
	out, err = runner.Run(
		t.Context(),
		out.Turn,
		Continue{CommandID: "raise", Budget: raised},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("continued stop=%+v", out.Stop)
	}
	if client.sends != 2 {
		t.Fatalf("total sends=%d", client.sends)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Raises) != 1 {
		t.Fatalf("raises=%+v", view.Raises)
	}
	if view.Raises[0].At.IsZero() {
		t.Fatal("budget raise has no audit time")
	}
	if view.Raises[0].Budget.MaxCost.Amount != amount("0.003") {
		t.Fatalf("raised cap=%s", view.Raises[0].Budget.MaxCost.Amount)
	}
}

func TestBudgetMissingUsageCountsZero(t *testing.T) {
	client := newPricedClient(final("partial text"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	charge := view.ModelCallCosts[0]
	if view.Spend.CallsWithoutFinalUsage != 1 ||
		charge.UsageSource != UsageSourceMissing || charge.Exact {
		t.Fatalf("missing usage marker: spend=%+v charge=%+v", view.Spend, charge)
	}
	// Dashi fills in no token count: neither the input estimate nor the
	// visible text.
	if !reflect.DeepEqual(view.Spend.Usage, llm.Usage{}) || charge.Usage != nil {
		t.Fatalf("usage=%+v charge=%+v", view.Spend.Usage, charge.Usage)
	}
	if charge.Cost == nil || charge.Cost.Total != 0 || view.Spend.CostUnknown != 0 {
		t.Fatalf("charge=%+v spend=%+v", charge.Cost, view.Spend)
	}
}

// hostBudgetHooks deny every call with a resumable host limit once the
// estimate is known.
func hostBudgetHooks() Hooks {
	return SendHook{
		Name: "host-budget",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			if plan.WorstCaseCost == nil || plan.Remaining == nil {
				return SendDecision{Deny: true, Reason: "missing estimate"}, nil
			}
			return SendDecision{
				Reason: "insufficient credits",
				Limit: &LimitDetail{
					Kind: LimitHost,
					Code: "insufficient_credits",
				},
			}, nil
		},
	}.Hooks()
}

func TestBudgetHostDenialIsResumableLimit(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		hostBudgetHooks(),
	)
	limit := requireBudgetLimit(t, out, LimitHost)
	if limit.Code != "insufficient_credits" {
		t.Fatalf("host code=%q", limit.Code)
	}
	if !out.ReadyToContinue {
		t.Fatal("host limit should be resumable")
	}
	if client.sends != 0 {
		t.Fatalf("denied plan sent %d calls", client.sends)
	}
}

func TestBudgetToolCostIsTrackedOutsideModelCap(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	client := newPricedClient(first, final("unused"))
	lookup := budgetLookup(&tool.Cost{Amount: amount("0.5"), Currency: "EUR"})
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		tool.List{lookup},
		Hooks{},
	)
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if client.sends != 2 {
		t.Fatalf("sends=%d", client.sends)
	}
	if view.Spend.ToolCost["EUR"] != amount("0.5") {
		t.Fatalf("EUR tool cost=%s", view.Spend.ToolCost["EUR"])
	}
	if view.Spend.Cost["USD"] <= 0 {
		t.Fatalf("USD model cost=%s", view.Spend.Cost["USD"])
	}
	if view.Spend.CostUnknown != 0 {
		t.Fatalf("unknown costs=%d", view.Spend.CostUnknown)
	}
}

func TestBudgetOutputCapRaiseResumesWithRemainingAllowance(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 50}
	second := final("done")
	second.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 20}
	client := newPricedClient(first, second)
	lookup := budgetLookup(nil)
	def := testAgent()
	def.Settings.MaxTokens = 100
	runner := Runner{
		Client: client,
		Tools:  tool.List{lookup},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "tokens"},
		Start{
			CommandID: "start",
			Agent:     def,
			Budget:    &Budget{MaxOutputTokens: 50},
			Input:     []llm.Message{llm.UserMessage(llm.Text("lookup"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	requireBudgetLimit(t, out, LimitOutputTokens)
	if client.sends != 1 {
		t.Fatalf("first phase sends=%d", client.sends)
	}
	if client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("first allowance=%d", client.inputs[0].Settings.MaxTokens)
	}
	out, err = runner.Run(
		t.Context(),
		out.Turn,
		Continue{CommandID: "raise", Budget: &Budget{MaxOutputTokens: 100}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("continued stop=%+v", out.Stop)
	}
	if client.sends != 2 {
		t.Fatalf("total sends=%d", client.sends)
	}
	if client.inputs[1].Settings.MaxTokens != 50 {
		t.Fatalf("remaining allowance=%d", client.inputs[1].Settings.MaxTokens)
	}
}

func TestBudgetRejectsDecreaseWithoutRecording(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{
			MaxOutputTokens: 50,
			MaxCost:         &Money{Amount: amount("0.0001"), Currency: "USD"},
		},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	if out.Stop.Kind != StopLimited {
		t.Fatalf("stop=%+v", out.Stop)
	}
	_, err := (Runner{Client: client}).Run(
		t.Context(),
		out.Turn,
		Continue{
			CommandID: "lower",
			Budget: &Budget{
				MaxOutputTokens: 40,
				MaxCost:         &Money{Amount: amount("0.001"), Currency: "USD"},
			},
		},
	)
	if err == nil {
		t.Fatal("decreasing a cap should fail before a write")
	}
	if len(out.Turn.Steps) != 2 {
		t.Fatalf("steps=%d", len(out.Turn.Steps))
	}
}

func providerToolUse(id string) llm.Content {
	return llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:   id,
			Name: "web_search",
			Type: "web_search_call",
			Origin: content.Origin{
				Provider: "test",
				API:      "test.chat",
				Model:    "fixture",
				Scope:    "fixture",
			},
		},
	}
}

func TestBudgetProviderToolRequestsUseAdapterCounts(t *testing.T) {
	client := newPricedClient()
	client.pricing.Requests = map[string]money.Amount{
		llm.MeterWebSearch: amount("0.01"),
	}
	result := final("answer")
	// The adapter reports one billed search; the block count (two, one of
	// them a failed search) does not override it.
	result.Usage = &llm.Usage{
		InputTokens:  10,
		OutputTokens: 10,
		Requests:     map[string]int{llm.MeterWebSearch: 1},
		// A complete adapter response carries final counts.
		RequestsComplete: true,
	}
	result.Message.Content = append(
		[]llm.Content{providerToolUse("search-1"), providerToolUse("search-2")},
		result.Message.Content...,
	)
	client.results = []llm.Result{result}
	out := runBudgetTurn(t, client, nil, llm.Settings{MaxTokens: 100}, nil, Hooks{})
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		view.Spend.Usage.Requests,
		map[string]int{llm.MeterWebSearch: 1},
	) {
		t.Fatalf("requests=%v", view.Spend.Usage.Requests)
	}
	if view.Spend.Cost["USD"] != amount("0.01002") ||
		view.Spend.CostPartial != 0 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

func TestBudgetPartialCostReportsKnownPart(t *testing.T) {
	client := newPricedClient()
	client.pricing.Requests = map[string]money.Amount{
		llm.MeterWebSearch: amount("0.01"),
	}
	result := final("answer")
	result.Usage = &llm.Usage{
		InputTokens:  10,
		OutputTokens: 10,
		Requests: map[string]int{
			llm.MeterWebSearch:     1,
			llm.MeterCodeExecution: 2,
		},
		RequestsComplete: true,
	}
	client.results = []llm.Result{result}
	out := runBudgetTurn(t, client, nil, llm.Settings{MaxTokens: 100}, nil, Hooks{})
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	spend := view.Spend
	if spend.Cost["USD"] != amount("0.01002") || spend.CostUnknown != 0 ||
		spend.CostPartial != 1 ||
		!reflect.DeepEqual(
			spend.UnpricedRequests,
			map[string]int{llm.MeterCodeExecution: 2},
		) {
		t.Fatalf("spend=%+v", spend)
	}
	var charge *llm.Cost
	for _, c := range view.ModelCallCosts {
		charge = c.Cost
	}
	if charge == nil || !charge.Partial() {
		t.Fatalf("final cost=%+v", charge)
	}
	clone := spend.Clone()
	clone.UnpricedRequests[llm.MeterCodeExecution] = 9
	if spend.UnpricedRequests[llm.MeterCodeExecution] != 2 {
		t.Fatal("spend clone shares unpriced requests")
	}
}

func TestBudgetUnforeseenProviderToolRequestStopsCappedTurn(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{
		InputTokens:      10,
		OutputTokens:     10,
		Requests:         map[string]int{llm.UnrecognizedMeter("mystery_call"): 1},
		RequestsComplete: true,
	}
	client := newPricedClient(first, final("done"))
	def := testAgent()
	def.Settings.MaxTokens = 100
	runner := Runner{
		Client: client,
		Tools:  tool.List{budgetLookup(nil)},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "unforeseen"},
		Start{
			CommandID: "start",
			Agent:     def,
			Budget:    &Budget{MaxCost: &Money{Amount: amount("1"), Currency: "USD"}},
			Input:     []llm.Message{llm.UserMessage(llm.Text("lookup"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.CostUsed != amount("0.00002") {
		t.Fatalf("known part used=%s", limit.CostUsed)
	}
	for _, want := range []string{
		"could not be attributed (unrecognized:mystery_call)",
		"start a new turn",
	} {
		if !strings.Contains(out.Stop.Reason, want) {
			t.Fatalf("reason %q lacks %q", out.Stop.Reason, want)
		}
	}
	if strings.Contains(out.Stop.Reason, "supply prices") ||
		strings.Contains(out.Stop.Reason, "another currency") {
		t.Fatalf("reason names a cause that did not occur: %q", out.Stop.Reason)
	}
	if client.sends != 1 || out.ReadyToContinue {
		t.Fatalf("sends=%d ready=%v", client.sends, out.ReadyToContinue)
	}
	_, err = runner.Run(
		t.Context(),
		out.Turn,
		Continue{
			CommandID: "raise",
			Budget:    &Budget{MaxCost: &Money{Amount: amount("5"), Currency: "USD"}},
		},
	)
	if err == nil {
		t.Fatal("a raise must not clear a charge whose amount is unknown")
	}
}

type cancelingPricedClient struct {
	*pricedClient
	cancel context.CancelFunc
	usage  *llm.Usage
}

func (c *cancelingPricedClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	sink := call.Sink
	if c.usage != nil {
		sink(llm.Delta{Kind: llm.DeltaUsage, Usage: c.usage})
	}
	sink(llm.Delta{Kind: llm.DeltaText, Text: "partial"})
	c.cancel()
	return llm.Result{}, ctx.Err()
}

func runInterruptedBudgetTurn(t *testing.T, usage *llm.Usage) RunResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &cancelingPricedClient{
		pricedClient: newPricedClient(),
		cancel:       cancel,
		usage:        usage,
	}
	def := testAgent()
	def.Settings.MaxTokens = 100
	out, err := (Runner{
		Client: client,
	}).Run(
		ctx,
		Turn{ID: "partial"},
		Start{
			CommandID: "start",
			Agent:     def,
			Budget: &Budget{
				MaxOutputTokens: 2,
				MaxCost:         &Money{Amount: amount("0.01"), Currency: "USD"},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	return out
}

func TestBudgetInterruptedCallWithoutUsageCountsZero(t *testing.T) {
	out := runInterruptedBudgetTurn(t, nil)
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.CallsWithoutFinalUsage != 1 ||
		view.ModelCallCosts[0].UsageSource != UsageSourceMissing {
		t.Fatalf("spend=%+v calls=%+v", view.Spend, view.ModelCallCosts)
	}
	if view.Spend.Usage.OutputTokens != 0 || view.Spend.Cost["USD"] != 0 ||
		view.Spend.CostUnknown != 0 {
		t.Fatalf("spend=%+v", view.Spend)
	}
	if r := out.Turn.Steps[2].Model.Result; r != nil && r.Usage != nil {
		t.Fatalf("streamed text was recorded as usage: %+v", r.Usage)
	}
}

func TestBudgetCutOffCallCountsOnlyReportedUsage(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reportedOutput int
		charge         string
	}{
		{name: "input only", charge: "0.0001"},
		{name: "earlier output frame", reportedOutput: 1, charge: "0.000101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runInterruptedBudgetTurn(
				t,
				&llm.Usage{InputTokens: 100, OutputTokens: tc.reportedOutput},
			)
			view, err := out.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			if view.Spend.Usage.InputTokens != 100 {
				t.Fatalf("observed input tokens=%d", view.Spend.Usage.InputTokens)
			}
			// The streamed text adds no output estimate.
			if view.Spend.Usage.OutputTokens != tc.reportedOutput {
				t.Fatalf("output tokens=%d", view.Spend.Usage.OutputTokens)
			}
			if view.Spend.CallsWithoutFinalUsage != 1 ||
				view.ModelCallCosts[0].UsageSource != UsageSourceCutOff {
				t.Fatalf("spend=%+v calls=%+v", view.Spend, view.ModelCallCosts)
			}
			if view.Spend.Cost["USD"] != amount(tc.charge) {
				t.Fatalf("replayed charge=%s", view.Spend.Cost["USD"])
			}
			finished := out.Turn.Steps[2].Model
			if finished.Result == nil || finished.Result.Usage == nil ||
				finished.Result.Usage.OutputTokens != tc.reportedOutput {
				t.Fatalf("partial usage was not recorded as reported: %+v", finished)
			}
		})
	}
}

func TestBudgetReportsProviderTokenOverrunOnFinalReply(t *testing.T) {
	answer := final("answer")
	answer.Usage = &llm.Usage{InputTokens: 20, OutputTokens: 60}
	client := newPricedClient(answer)
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxOutputTokens: 50},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	requireBudgetLimit(t, out, LimitOutputTokens)
	if out.Reply != "answer" {
		t.Fatalf("answer=%q", out.Reply)
	}
}

func TestBudgetTotalTokensStopsBetweenCalls(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 40, OutputTokens: 20}
	client := newPricedClient(first, final("unused"))
	lookup := budgetLookup(nil)
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxTotalTokens: 60},
		llm.Settings{MaxTokens: 100},
		tool.List{lookup},
		Hooks{},
	)
	limit := requireBudgetLimit(t, out, LimitTotalTokens)
	if limit.Used != 60 {
		t.Fatalf("used tokens=%d", limit.Used)
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d", client.sends)
	}
	if out.ReadyToContinue {
		t.Fatal("spent turn can continue without a raise")
	}
}

// A started call counts no usage (decision 47), so the next call start
// cannot overflow; the overflow arrives with its reported usage and stops
// the turn, since the token cap can no longer be enforced.
func TestBudgetCumulativeOverflowStopsAtTheFinish(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: math.MaxInt - 1}
	second := final("done")
	second.Usage = &llm.Usage{OutputTokens: 2}
	client := newPricedClient(first, second)
	out, err := runBudgetTurnErr(
		client,
		&Budget{MaxTotalTokens: math.MaxInt},
		llm.Settings{MaxTokens: 100},
		tool.List{budgetLookup(nil)},
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if client.sends != 2 {
		t.Fatalf("sends=%d", client.sends)
	}
	if out.Stop.Kind != StopLimited || !out.Stop.Ended ||
		!out.Spend.UsageIncomplete {
		t.Fatalf("stop=%+v spend=%+v", out.Stop, out.Spend)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.InputTokens != math.MaxInt-1 {
		t.Fatalf("recorded input tokens=%d", view.Spend.Usage.InputTokens)
	}
	if view.Spend.Usage.OutputTokens != 0 {
		t.Fatalf("overflowing output recorded=%d", view.Spend.Usage.OutputTokens)
	}
}

type providerToolCancelClient struct {
	*pricedClient
	cancel context.CancelFunc
}

func (c *providerToolCancelClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	// Anthropic's message_start frame reports input usage with no request
	// counts; the stream is cut off before the final usage arrives.
	call.Sink(llm.Delta{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 100}})
	for _, id := range []string{"search-1", "search-2"} {
		use := providerToolUse(id)
		call.Sink(llm.Delta{Content: &use})
	}
	c.cancel()
	return llm.Result{}, ctx.Err()
}

func TestBudgetInterruptedProviderToolUseRecordsUnreportedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &providerToolCancelClient{pricedClient: newPricedClient(), cancel: cancel}
	client.pricing.Requests = map[string]money.Amount{
		llm.MeterWebSearch: amount("0.01"),
	}
	def := testAgent()
	def.Settings.MaxTokens = 100
	out, err := (Runner{Client: client}).Run(
		ctx,
		Turn{ID: "cut"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("search"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.Requests[llm.MeterUnreported] != 2 ||
		view.Spend.CostPartial != 1 ||
		view.Spend.UnpricedRequests[llm.MeterUnreported] != 2 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

// providerToolCollectedCancelClient streams nothing; after the run's context
// ends it returns a message holding provider tool records, with no ID or
// usage.
type providerToolCollectedCancelClient struct {
	*pricedClient
	cancel context.CancelFunc
}

func (c *providerToolCollectedCancelClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	c.cancel()
	return llm.Result{Message: llm.AssistantMessage(
		providerToolUse("search-1"),
		providerToolUse("search-2"),
		providerToolUse("search-3"),
	)}, ctx.Err()
}

// A run-cancelled call never goes into the messages the model sees, so it
// keeps the message the client returned: its provider tool records stay for
// audit and count as
// unreported requests.
func TestRunCancelKeepsReturnedProviderToolContent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &providerToolCollectedCancelClient{
		pricedClient: newPricedClient(),
		cancel:       cancel,
	}
	out, err := (Runner{Client: client}).Run(
		ctx,
		Turn{ID: "cut"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("search"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var finished *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished {
			finished = step.Model
		}
	}
	if finished == nil || finished.Interrupted != InterruptRun ||
		finished.Result == nil {
		t.Fatalf("finished=%+v", finished)
	}
	uses := 0
	for _, part := range finished.Result.Message.Content {
		if part.Kind == llm.ContentProviderToolUse {
			uses++
		}
	}
	if uses != 3 {
		t.Fatalf("kept provider tool uses=%d, want 3", uses)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.Requests[llm.MeterUnreported] != 3 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

func TestUncheckableCostReasonNamesEachCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spend   Spend
		want    []string
		without []string
	}{
		{
			name: "priceable",
			spend: Spend{
				CostUnknown:      1,
				CostPartial:      1,
				UnpricedRequests: map[string]int{llm.MeterCodeExecution: 1},
			},
			want:    []string{"no price (code_execution)", "supply prices"},
			without: []string{"attributed", "another currency"},
		},
		{
			name: "reserved and priceable",
			spend: Spend{
				CostUnknown: 2,
				CostPartial: 2,
				UnpricedRequests: map[string]int{
					llm.MeterUnreported:    1,
					llm.MeterCodeExecution: 1,
				},
			},
			want: []string{
				"no price (code_execution)",
				"could not be attributed (unreported)",
			},
			without: []string{"another currency"},
		},
		{
			name:    "missing charge",
			spend:   Spend{CostUnknown: 1},
			want:    []string{"unknown or in another currency"},
			without: []string{"provider tool"},
		},
		{
			name: "partial and missing charge",
			spend: Spend{
				CostUnknown:      2,
				CostPartial:      1,
				UnpricedRequests: map[string]int{llm.MeterUnreported: 1},
			},
			want: []string{"attributed (unreported)", "another currency"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := uncheckableCostReason(tc.spend)
			for _, want := range append(tc.want, "start a new turn") {
				if !strings.Contains(reason, want) {
					t.Fatalf("reason %q lacks %q", reason, want)
				}
			}
			for _, bad := range tc.without {
				if strings.Contains(reason, bad) {
					t.Fatalf("reason %q contains %q", reason, bad)
				}
			}
		})
	}
}

func TestBudgetPartialCostsSumAcrossCalls(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{
		InputTokens:  10,
		OutputTokens: 10,
		Requests:     map[string]int{llm.MeterCodeExecution: 2},
	}
	second := final("done")
	second.Usage = &llm.Usage{
		InputTokens:  10,
		OutputTokens: 10,
		Requests:     map[string]int{llm.MeterCodeExecution: 3},
	}
	client := newPricedClient(first, second)
	out := runBudgetTurn(
		t,
		client,
		nil,
		llm.Settings{MaxTokens: 100},
		tool.List{budgetLookup(nil)},
		Hooks{},
	)
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.CostPartial != 2 ||
		view.Spend.UnpricedRequests[llm.MeterCodeExecution] != 5 ||
		view.Spend.Cost["USD"] != amount("0.00004") {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

func TestUnreportedRequestsSkipFailedUsesAndCoverSilentProviderTools(t *testing.T) {
	failed := providerToolUse("search-2")
	result := &llm.Result{Message: llm.Message{Content: []llm.Content{
		providerToolUse("search-1"),
		failed,
		{
			Kind: llm.ContentProviderToolResult,
			ProviderToolResult: &llm.ProviderToolResult{
				CallID:  "search-2",
				Type:    "web_search_tool_result",
				IsError: true,
			},
		},
	}}}
	usage := &llm.Usage{}
	addUnreportedRequests(usage, &ModelFact{}, result)
	if usage.Requests[llm.MeterUnreported] != 1 {
		t.Fatalf("a refused use counted as billed: %v", usage.Requests)
	}

	providerTool := &ModelFact{Tools: []tool.Definition{{
		Name: "google_search",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "googleSearch",
		},
	}}}
	usage = &llm.Usage{InputTokens: 10}
	addUnreportedRequests(usage, providerTool, nil)
	if usage.Requests[llm.MeterUnreported] != 1 {
		t.Fatalf("silent provider tool left the call looking complete: %v", usage.Requests)
	}
	usage = &llm.Usage{InputTokens: 10}
	addUnreportedRequests(usage, &ModelFact{}, nil)
	if len(usage.Requests) != 0 {
		t.Fatalf("call without provider tools marked unreported: %v", usage.Requests)
	}
}

type groundingCutClient struct {
	*pricedClient
	cancel context.CancelFunc
}

func (c *groundingCutClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	// Gemini sends usage on each chunk and grounding metadata near the end;
	// this stream is cut before any grounding arrives.
	call.Sink(llm.Delta{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 50}})
	call.Sink(llm.Delta{Kind: llm.DeltaText, Text: "Spain won"})
	c.cancel()
	return llm.Result{}, ctx.Err()
}

func TestBudgetInterruptedSilentProviderToolIsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &groundingCutClient{pricedClient: newPricedClient(), cancel: cancel}
	def := testAgent()
	def.Settings.MaxTokens = 100
	search := tool.Tool{Definition: tool.Definition{
		Name: "google_search",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "googleSearch",
		},
	}}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{search},
	}).Run(
		ctx,
		Turn{ID: "grounding-cut"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("who won?"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// A cut-off call with a provider tool may have run it, so its result is
	// unknown.
	if out.Stop.Kind != StopResultUnknown {
		t.Fatalf("stop=%+v", out.Stop)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.CostPartial != 1 ||
		view.Spend.UnpricedRequests[llm.MeterUnreported] != 1 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

func TestUnreportedRequestsMarkNonFinalCounts(t *testing.T) {
	inferred := &llm.Result{Message: llm.Message{Content: []llm.Content{
		providerToolUse("search-1"),
	}}}
	// Counts inferred from the one visible block, with no final report.
	usage := &llm.Usage{Requests: map[string]int{llm.MeterWebSearch: 1}}
	addUnreportedRequests(usage, &ModelFact{}, inferred)
	if usage.Requests[llm.MeterWebSearch] != 1 ||
		usage.Requests[llm.MeterUnreported] != 1 {
		t.Fatalf("non-final counts left the call complete: %v", usage.Requests)
	}
	usage = &llm.Usage{InputTokens: 10}
	addUnreportedRequests(usage, &ModelFact{}, &llm.Result{})
	if len(usage.Requests) != 0 {
		t.Fatalf("call without provider tool activity marked unreported: %v", usage.Requests)
	}
}

// A worker that stops mid-call leaves a call with no reported usage. Its
// recovery charges nothing, so a tight cap does not stall the turn on a
// phantom charge (issue 255).
func TestBudgetWorkerStoppedCallChargesNothing(t *testing.T) {
	client := newPricedClient(final("lost"))
	budget := &Budget{MaxCost: &Money{Amount: amount("0.002"), Currency: "USD"}}
	out := runBudgetTurn(t, client, budget, llm.Settings{MaxTokens: 100}, nil, Hooks{})
	crashed := prefixThrough(t, out.Turn, StepModelCallStarted, 1)
	retry := newPricedClient(final("done"))
	out, err := (Runner{Client: retry}).Run(
		t.Context(),
		crashed,
		Continue{CommandID: "recover"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted || retry.sends != 1 || out.Reply != "done" {
		t.Fatalf("stop=%+v sends=%d reply=%q", out.Stop, retry.sends, out.Reply)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	lost := view.ModelCallCosts[0]
	if lost.UsageSource != UsageSourceMissing || lost.Usage != nil ||
		lost.Cost == nil || lost.Cost.Total != 0 || lost.Exact {
		t.Fatalf("lost call=%+v", lost)
	}
	if view.Spend.CostUnknown != 0 || view.Spend.CallsWithoutFinalUsage != 2 {
		t.Fatalf("spend=%+v", view.Spend)
	}
	if finish := finishes(out.Turn)[0]; finish.Interrupted != InterruptWorker {
		t.Fatalf("recovered finish=%+v", finish)
	}
}
