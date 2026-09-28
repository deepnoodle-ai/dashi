package kernel

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

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
				Input:      1,
				Output:     1,
				CacheRead:  1,
				CacheWrite: 1,
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
	def := definition()
	def.Settings = settings
	if policy == nil {
		policy = AllowLocalPolicy{}
	}
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "budget"},
		def,
		Start{
			ID:     "start",
			Budget: budget,
			Input:  []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
		Bindings{Client: client, Tools: tools, Policy: policy},
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
				Output: ptr(tool.TextOutput("done")),
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
	budget := Budget{MaxCost: &Money{Amount: 1, Currency: "USD"}}
	clone := budget.Clone()
	clone.MaxCost.Amount = 2
	if budget.MaxCost.Amount != 1 {
		t.Fatal("budget clone shared its cost cap")
	}
}

func TestSpendCloneCopiesUsageAndCosts(t *testing.T) {
	spend := Spend{
		Usage: llm.Usage{Requests: map[string]int{"web_search": 1}},
		Cost:  map[string]float64{"USD": 1},
	}
	clone := spend.Clone()
	clone.Usage.Requests["web_search"] = 2
	clone.Cost["USD"] = 2
	if spend.Usage.Requests["web_search"] != 1 {
		t.Fatal("spend clone shared hosted request counts")
	}
	if spend.Cost["USD"] != 1 {
		t.Fatal("spend clone shared currency totals")
	}
}

func TestSpendAddCostRejectsOverflow(t *testing.T) {
	spend := Spend{Cost: map[string]float64{"USD": math.MaxFloat64}}
	if err := spend.addCost("USD", math.MaxFloat64); err == nil {
		t.Fatal("overflowing cumulative cost was accepted")
	}
	if spend.Cost["USD"] != math.MaxFloat64 {
		t.Fatal("failed cost addition changed spend")
	}
}

func TestBudgetStopsUnpricedModelBeforeDispatch(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: 0.5, Currency: "USD"}},
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

func TestBudgetCostShortfallDoesNotShrinkRequest(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: 0.0015, Currency: "USD"}},
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
			MaxCost:         &Money{Amount: 0.01, Currency: "USD"},
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
	if math.Abs(view.Spend.Cost["USD"]-0.00014) > 1e-12 {
		t.Fatalf("cost=%g", view.Spend.Cost["USD"])
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
		t.Fatalf("risk=%g charge=%g", intent.AtRisk.Total, settled.Charge.Total)
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
	initial := &Budget{MaxCost: &Money{Amount: 0.0016, Currency: "USD"}}
	def := definition()
	def.Settings.MaxTokens = 500
	rec := NewMemoryRecorder()
	b := Bindings{
		Client:   client,
		Tools:    fixedTools{lookup},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "raise"},
		def,
		Start{
			ID:     "start",
			Budget: initial,
			Input:  []llm.Message{llm.UserMessage(llm.Text("lookup"))},
		},
		b,
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
	raised := &Budget{MaxCost: &Money{Amount: 0.003, Currency: "USD"}}
	out, err = (Engine{}).Apply(
		context.Background(),
		out.Turn,
		def,
		Continue{ID: "raise", Budget: raised},
		b,
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
	if view.Raises[0].Budget.MaxCost.Amount != 0.003 {
		t.Fatalf("raised cap=%g", view.Raises[0].Budget.MaxCost.Amount)
	}
}

func TestBudgetMissingUsageUsesRecordedBestGuess(t *testing.T) {
	client := newPricedClient(final("partial text"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
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
		t.Fatalf("estimated input=%d, recorded=%d",
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
		t.Fatalf("charge=%g risk=%g", settled.Charge.Total, intent.AtRisk.Total)
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
		Limit: &LimitDetail{
			Kind: LimitHost,
			Code: "insufficient_credits",
		},
	}, nil
}

func TestBudgetHostDenialIsResumableLimit(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
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

func TestBudgetToolCostInOtherCurrencyStopsBeforeNextCall(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	client := newPricedClient(first, final("unused"))
	lookup := budgetLookup(&tool.Cost{Amount: 0.5, Currency: "EUR"})
	out := runBudgetTurn(
		t,
		client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		fixedTools{lookup},
		nil,
	)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	requireBudgetLimit(t, out, LimitCost)
	if client.sends != 1 {
		t.Fatalf("sends=%d", client.sends)
	}
	if view.Spend.Cost["EUR"] != 0.5 {
		t.Fatalf("EUR cost=%g", view.Spend.Cost["EUR"])
	}
	if view.Spend.CostUnknown != 1 {
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
	def := definition()
	def.Settings.MaxTokens = 100
	b := Bindings{
		Client: client,
		Tools:  fixedTools{lookup},
		Policy: AllowLocalPolicy{},
	}
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "tokens"},
		def,
		Start{
			ID:     "start",
			Budget: &Budget{MaxOutputTokens: 50},
			Input:  []llm.Message{llm.UserMessage(llm.Text("lookup"))},
		},
		b,
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
	out, err = (Engine{}).Apply(
		context.Background(),
		out.Turn,
		def,
		Continue{ID: "raise", Budget: &Budget{MaxOutputTokens: 100}},
		b,
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
			MaxCost:         &Money{Amount: 0.0001, Currency: "USD"},
		},
		llm.Settings{MaxTokens: 100},
		nil,
		nil,
	)
	if out.Stop.Kind != StopLimited {
		t.Fatalf("stop=%+v", out.Stop)
	}
	def := definition()
	def.Settings.MaxTokens = 100
	_, err := (Engine{}).Apply(
		context.Background(),
		out.Turn,
		def,
		Continue{
			ID: "lower",
			Budget: &Budget{
				MaxOutputTokens: 40,
				MaxCost:         &Money{Amount: 0.001, Currency: "USD"},
			},
		},
		Bindings{Client: client, Policy: AllowLocalPolicy{}},
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
	client.pricing.Requests = map[string]float64{"web_search": 0.01}
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
	out := runBudgetTurn(
		t,
		client,
		nil,
		llm.Settings{MaxTokens: 100},
		nil,
		nil,
	)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.Requests["web_search"] != 1 {
		t.Fatalf("web searches=%d", view.Spend.Usage.Requests["web_search"])
	}
	if math.Abs(view.Spend.Cost["USD"]-0.01002) > 1e-12 {
		t.Fatalf("cost=%g", view.Spend.Cost["USD"])
	}
}

type cancelingPricedClient struct {
	*pricedClient
	cancel context.CancelFunc
}

func (c *cancelingPricedClient) Send(
	ctx context.Context,
	_ llm.Request,
	sink llm.Sink,
) (llm.Result, error) {
	sink(llm.Delta{Kind: llm.DeltaText, Text: "partial"})
	c.cancel()
	return llm.Result{}, ctx.Err()
}

func TestBudgetInterruptedCallCountsPartialBestGuess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &cancelingPricedClient{
		pricedClient: newPricedClient(),
		cancel:       cancel,
	}
	def := definition()
	def.Settings.MaxTokens = 100
	out, err := (Engine{}).Apply(
		ctx,
		Turn{ID: "partial"},
		def,
		Start{
			ID:     "start",
			Budget: &Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
			Input:  []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
		Bindings{Client: client, Policy: AllowLocalPolicy{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v", out.Stop)
	}
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
		t.Fatalf("cost=%g", view.Spend.Cost["USD"])
	}
	if out.Turn.Steps[2].Model.Charge == nil ||
		!out.Turn.Steps[2].Model.EstimatedUsage {
		t.Fatalf("settlement=%+v", out.Turn.Steps[2].Model)
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
		fixedTools{lookup},
		nil,
	)
	limit := requireBudgetLimit(t, out, LimitTotalTokens)
	if limit.Used != 60 {
		t.Fatalf("used tokens=%g", limit.Used)
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
		fixedTools{budgetLookup(nil)},
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
