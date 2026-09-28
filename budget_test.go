package dashi

import (
	"context"
	"math"
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
	})
}

func runBudgetTurn(
	t *testing.T,
	client llm.Client,
	budget *Budget,
	settings llm.Settings,
	tools tool.Toolset,
	policy Policy,
) Outcome {
	t.Helper()
	def := testAgent()
	def.Settings = settings
	if policy == nil {
		policy = AllowLocalPolicy{}
	}
	out, err := (Runner{
		Agent:  def,
		Client: client,
		Tools:  tools,
		Policy: policy,
	}).Run(
		t.Context(),
		Turn{ID: "budget"},
		Start{
			CommandID: "start",
			Budget:    budget,
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
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
	out Outcome,
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
		t.Fatal("spend clone shared hosted request counts")
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
		nil,
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
		nil,
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
		nil,
	)
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.FitTokens != 0 {
		t.Fatalf("fit tokens=%d", limit.FitTokens)
	}
	if client.sends != 0 {
		t.Fatalf("unaffordable input sent %d calls", client.sends)
	}
	if !strings.Contains(out.Stop.Reason, "reduce input/history") {
		t.Fatalf("reason=%q", out.Stop.Reason)
	}
}

func TestBudgetStopsForUnboundedHostedRequests(t *testing.T) {
	client := newPricedClient()
	input := llm.Input{
		Model:    "fixture",
		Settings: llm.Settings{MaxTokens: 100},
		Tools: []tool.Definition{
			{
				Name: "web_search",
				Kind: tool.KindProviderHosted,
				Hosted: &tool.HostedSpec{
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
		t.Fatalf("hosted cost was admitted: stop=%+v", stop)
	}
	if !strings.Contains(stop.Reason, "remove hosted tools") {
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
		nil,
	)
	limit := requireBudgetLimit(t, out, LimitCost)
	if limit.FitTokens <= 0 || limit.FitTokens >= 1000 {
		t.Fatalf("fit tokens=%d", limit.FitTokens)
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
		nil,
	)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("effective max tokens=%d", client.inputs[0].Settings.MaxTokens)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Cost["USD"] != amount("0.00014") {
		t.Fatalf("cost=%s", view.Spend.Cost["USD"])
	}
	if view.Spend.Usage.OutputTokens != 40 {
		t.Fatalf("output tokens=%d", view.Spend.Usage.OutputTokens)
	}
	if view.Spend.Estimated != 0 {
		t.Fatalf("estimated calls=%d", view.Spend.Estimated)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningBudgetClamp {
		t.Fatalf("warnings=%+v", view.Warnings)
	}
	intent := out.Turn.Steps[1].Model
	settled := out.Turn.Steps[2].Model
	if intent.AtRisk == nil || settled.Charge == nil {
		t.Fatalf("missing risk or charge: intent=%+v settled=%+v", intent, settled)
	}
	if intent.AtRisk.Total <= settled.Charge.Total {
		t.Fatalf("risk=%s charge=%s", intent.AtRisk.Total, settled.Charge.Total)
	}
	if settled.EstimatedUsage {
		t.Fatal("reported usage marked estimated")
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
		Agent:    def,
		Client:   client,
		Tools:    tool.List{lookup},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "raise"},
		Start{
			CommandID: "start",
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
	view, err := Replay(out.Turn)
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

func TestBudgetMissingUsageUsesRecordedBestGuess(t *testing.T) {
	client := newPricedClient(final("partial text"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		nil,
	)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	intent := out.Turn.Steps[1].Model
	settled := out.Turn.Steps[2].Model
	if view.Spend.Estimated != 1 || !settled.EstimatedUsage {
		t.Fatalf(
			"missing estimation marker: spend=%+v settled=%+v",
			view.Spend,
			settled,
		)
	}
	if view.Spend.Usage.InputTokens != intent.EstimatedInputTokens {
		t.Fatalf(
			"estimated input=%d, recorded=%d",
			intent.EstimatedInputTokens,
			view.Spend.Usage.InputTokens,
		)
	}
	if view.Spend.Usage.OutputTokens != 3 {
		t.Fatalf("estimated output=%d", view.Spend.Usage.OutputTokens)
	}
	if settled.Charge == nil || intent.AtRisk == nil {
		t.Fatalf("missing charge or risk: intent=%+v settled=%+v", intent, settled)
	}
	if settled.Charge.Total >= intent.AtRisk.Total {
		t.Fatalf("charge=%s risk=%s", settled.Charge.Total, intent.AtRisk.Total)
	}
}

type hostBudgetPolicy struct{ AllowLocalPolicy }

func (hostBudgetPolicy) BeforeSend(
	_ context.Context,
	plan SendPlan,
) (ModelAuthorization, error) {
	if plan.AtRisk == nil || plan.Remaining == nil {
		return ModelAuthorization{Reason: "missing estimate"}, nil
	}
	return ModelAuthorization{
		Reason: "insufficient credits",
		Limit:  &LimitDetail{Kind: LimitHost, Code: "insufficient_credits"},
	}, nil
}

func TestBudgetHostDenialIsResumableLimit(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		hostBudgetPolicy{},
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
		nil,
	)
	view, err := Replay(out.Turn)
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
		Agent:  def,
		Client: client,
		Tools:  tool.List{lookup},
		Policy: AllowLocalPolicy{},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "tokens"},
		Start{
			CommandID: "start",
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
		nil,
	)
	if out.Stop.Kind != StopLimited {
		t.Fatalf("stop=%+v", out.Stop)
	}
	def := testAgent()
	def.Settings.MaxTokens = 100
	_, err := (Runner{
		Agent:  def,
		Client: client,
		Policy: AllowLocalPolicy{},
	}).Run(
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

func TestBudgetHostedUseAddsPricedRequestCount(t *testing.T) {
	client := newPricedClient()
	client.pricing.Requests = map[string]money.Amount{
		"web_search": amount("0.01"),
	}
	result := final("answer")
	result.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	result.Message.Content = append(
		[]llm.Content{
			{
				Kind: llm.ContentHostedUse,
				HostedUse: &llm.HostedUse{
					ID:   "search-1",
					Name: "web_search",
					Type: "web_search_call",
					Origin: content.Origin{
						Provider: "test",
						API:      "test.chat",
						Model:    "fixture",
						Scope:    "fixture",
					},
				},
			},
		},
		result.Message.Content...,
	)
	client.results = []llm.Result{result}
	out := runBudgetTurn(t, client, nil, llm.Settings{MaxTokens: 100}, nil, nil)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.Requests["web_search"] != 1 {
		t.Fatalf("web searches=%d", view.Spend.Usage.Requests["web_search"])
	}
	if view.Spend.Cost["USD"] != amount("0.01002") {
		t.Fatalf("cost=%s", view.Spend.Cost["USD"])
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

func runInterruptedBudgetTurn(t *testing.T, usage *llm.Usage) Outcome {
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
		Agent:  def,
		Client: client,
		Policy: AllowLocalPolicy{},
	}).Run(
		ctx,
		Turn{ID: "partial"},
		Start{
			CommandID: "start",
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

func TestBudgetInterruptedCallCountsPartialBestGuess(t *testing.T) {
	out := runInterruptedBudgetTurn(t, nil)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Estimated != 1 {
		t.Fatalf("estimated calls=%d", view.Spend.Estimated)
	}
	if view.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("partial output estimate=%d", view.Spend.Usage.OutputTokens)
	}
	if view.Spend.Cost["USD"] <= 0 {
		t.Fatalf("cost=%s", view.Spend.Cost["USD"])
	}
	if out.Turn.Steps[2].Model.Charge == nil ||
		!out.Turn.Steps[2].Model.EstimatedUsage {
		t.Fatalf("settlement=%+v", out.Turn.Steps[2].Model)
	}
}

func TestBudgetInterruptedPartialUsageCountsVisibleOutput(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reportedOutput int
	}{
		{name: "input only"},
		{name: "earlier output frame", reportedOutput: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runInterruptedBudgetTurn(
				t,
				&llm.Usage{InputTokens: 100, OutputTokens: tc.reportedOutput},
			)
			view, err := Replay(out.Turn)
			if err != nil {
				t.Fatal(err)
			}
			if view.Spend.Usage.InputTokens != 100 {
				t.Fatalf("observed input tokens=%d", view.Spend.Usage.InputTokens)
			}
			if view.Spend.Usage.OutputTokens != 2 {
				t.Fatalf("estimated output tokens=%d", view.Spend.Usage.OutputTokens)
			}
			if view.Spend.Estimated != 1 {
				t.Fatalf("estimated attempts=%d", view.Spend.Estimated)
			}
			if view.Spend.Cost["USD"] != amount("0.000102") {
				t.Fatalf("replayed charge=%s", view.Spend.Cost["USD"])
			}
			settled := out.Turn.Steps[2].Model
			if settled.Result == nil || settled.Result.Usage == nil {
				t.Fatalf("partial usage was not recorded: %+v", settled)
			}
			if settled.Result.Usage.OutputTokens != tc.reportedOutput {
				t.Fatalf("observed output count was altered: %+v", settled.Result.Usage)
			}
			if !settled.EstimatedUsage {
				t.Fatal("partial usage was not marked estimated")
			}
			if settled.Charge == nil {
				t.Fatal("partial usage has no estimated charge")
			}
			if settled.Charge.Total != amount("0.000102") {
				t.Fatalf("estimated charge=%s", settled.Charge.Total)
			}
			def := testAgent()
			def.Settings.MaxTokens = 100
			client := newPricedClient(final("unused"))
			continued, err := (Runner{
				Agent:  def,
				Client: client,
				Policy: AllowLocalPolicy{},
			}).Run(t.Context(), out.Turn, Continue{CommandID: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			requireBudgetLimit(t, continued, LimitOutputTokens)
			if client.sends != 0 {
				t.Fatalf("spent output cap sent %d calls", client.sends)
			}
		})
	}
}

func TestBudgetReportsProviderTokenOverrunOnFinalAnswer(t *testing.T) {
	answer := final("answer")
	answer.Usage = &llm.Usage{InputTokens: 20, OutputTokens: 60}
	client := newPricedClient(answer)
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxOutputTokens: 50},
		llm.Settings{MaxTokens: 100},
		nil,
		nil,
	)
	requireBudgetLimit(t, out, LimitOutputTokens)
	if out.Turn.AnswerText() != "answer" {
		t.Fatalf("answer=%q", out.Turn.AnswerText())
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
		nil,
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

func TestBudgetCumulativeOverflowRejectsNextIntent(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: math.MaxInt - 1}
	second := final("done")
	second.Usage = &llm.Usage{OutputTokens: 2}
	client := newPricedClient(first, second)
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxTotalTokens: math.MaxInt},
		llm.Settings{MaxTokens: 100},
		tool.List{budgetLookup(nil)},
		nil,
	)
	if out.Persistence != PersistenceRejected {
		t.Fatalf("overflowing intent persistence=%q", out.Persistence)
	}
	if out.Unacknowledged == nil ||
		out.Unacknowledged.Kind != StepModelIntent {
		t.Fatalf("overflowing intent was acknowledged: %+v", out.Unacknowledged)
	}
	if client.sends != 1 {
		t.Fatalf("overflowing intent dispatched a call: sends=%d", client.sends)
	}
	view, err := Replay(out.Turn)
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
