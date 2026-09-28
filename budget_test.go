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
		Start{ID: "start", Budget: budget,
			Input: []llm.Message{llm.UserMessage(llm.Text("answer"))}},
		Bindings{Client: client, Tools: tools, Policy: policy},
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBudgetStopsUnpricedModelBeforeDispatch(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	out := runBudgetTurn(t, client,
		&Budget{MaxCost: &Money{Amount: 0.5, Currency: "USD"}},
		llm.Settings{MaxTokens: 100}, nil, nil)
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitCost ||
		out.Stop.Limit.UnpricedModel != "fixture" || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	if !strings.Contains(out.Stop.Reason, "supply this model's prices") {
		t.Fatalf("reason=%q", out.Stop.Reason)
	}
}

func TestBudgetCostShortfallDoesNotShrinkRequest(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(t, client,
		&Budget{MaxCost: &Money{Amount: 0.0015, Currency: "USD"}},
		llm.Settings{MaxTokens: 1000}, nil, nil)
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitCost || out.Stop.Limit.FitTokens <= 0 ||
		out.Stop.Limit.FitTokens >= 1000 || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	if client.inputs[0].Settings.MaxTokens != 1000 {
		t.Fatalf("max tokens was silently reduced: %+v", client.inputs[0].Settings)
	}
}

func TestBudgetClampsOutputAndRecordsCharge(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 40}
	client := newPricedClient(answer)
	out := runBudgetTurn(t, client,
		&Budget{MaxOutputTokens: 50,
			MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
		llm.Settings{MaxTokens: 1000}, nil, nil)
	if out.Stop.Kind != StopCompleted || client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("stop=%+v input=%+v", out.Stop, client.inputs)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(view.Spend.Cost["USD"]-0.00014) > 1e-12 ||
		view.Spend.Usage.OutputTokens != 40 || view.Spend.Estimated != 0 {
		t.Fatalf("spend=%+v", view.Spend)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningBudgetClamp {
		t.Fatalf("warnings=%+v", view.Warnings)
	}
	intent := out.Turn.Steps[1].Model
	settled := out.Turn.Steps[2].Model
	if intent.AtRisk == nil || intent.AtRisk.Total <= settled.Charge.Total ||
		settled.Charge == nil || settled.EstimatedUsage {
		t.Fatalf("intent=%+v settled=%+v", intent, settled)
	}
}

func TestBudgetRaiseResumesAndKeepsAudit(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	second := final("done")
	second.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	client := newPricedClient(first, second)
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name: "lookup", Schema: []byte(`{"type":"object"}`),
			Kind: tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded,
				Output: ptr(tool.TextOutput("ok"))}, nil
		}),
	}
	initial := &Budget{MaxCost: &Money{Amount: 0.0016, Currency: "USD"}}
	def := definition()
	def.Settings.MaxTokens = 500
	rec := NewMemoryRecorder()
	b := Bindings{Client: client, Tools: fixedTools{lookup},
		Recorder: rec, Policy: AllowLocalPolicy{}}
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "raise"},
		def, Start{ID: "start", Budget: initial,
			Input: []llm.Message{llm.UserMessage(llm.Text("lookup"))}}, b)
	if err != nil || out.Stop.Kind != StopLimited || out.ReadyToContinue ||
		client.sends != 1 {
		t.Fatalf("first stop=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
	raised := &Budget{MaxCost: &Money{Amount: 0.003, Currency: "USD"}}
	out, err = (Engine{}).Apply(context.Background(), out.Turn, def,
		Continue{ID: "raise", Budget: raised}, b)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("continued stop=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Raises) != 1 || view.Raises[0].At.IsZero() ||
		view.Raises[0].Budget.MaxCost.Amount != 0.003 {
		t.Fatalf("raises=%+v", view.Raises)
	}
}

func TestBudgetMissingUsageUsesRecordedBestGuess(t *testing.T) {
	client := newPricedClient(final("partial text"))
	out := runBudgetTurn(t, client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
		llm.Settings{MaxTokens: 100}, nil, nil)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	intent := out.Turn.Steps[1].Model
	settled := out.Turn.Steps[2].Model
	if view.Spend.Estimated != 1 ||
		view.Spend.Usage.InputTokens != intent.EstimatedInputTokens ||
		view.Spend.Usage.OutputTokens != 3 ||
		settled.Charge == nil || !settled.EstimatedUsage ||
		settled.Charge.Total >= intent.AtRisk.Total {
		t.Fatalf("spend=%+v intent=%+v settled=%+v", view.Spend, intent, settled)
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
	return ModelAuthorization{Reason: "insufficient credits",
		Limit: &LimitDetail{Kind: LimitHost,
			Code: "insufficient_credits"}}, nil
}

func TestBudgetHostDenialIsResumableLimit(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(t, client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
		llm.Settings{MaxTokens: 100}, nil, hostBudgetPolicy{})
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitHost ||
		out.Stop.Limit.Code != "insufficient_credits" ||
		!out.ReadyToContinue || client.sends != 0 {
		t.Fatalf("stop=%+v ready=%t sends=%d",
			out.Stop, out.ReadyToContinue, client.sends)
	}
}

func TestBudgetToolCostInOtherCurrencyStopsBeforeNextCall(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	client := newPricedClient(first, final("unused"))
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("done")),
				Cost:   &tool.Cost{Amount: 0.5, Currency: "EUR"},
			}, nil
		}),
	}
	out := runBudgetTurn(t, client,
		&Budget{MaxCost: &Money{Amount: 0.01, Currency: "USD"}},
		llm.Settings{MaxTokens: 100}, fixedTools{lookup}, nil)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitCost || client.sends != 1 ||
		view.Spend.Cost["EUR"] != 0.5 || view.Spend.CostUnknown != 1 {
		t.Fatalf("stop=%+v spend=%+v sends=%d",
			out.Stop, view.Spend, client.sends)
	}
}

func TestBudgetOutputCapRaiseResumesWithRemainingAllowance(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 50}
	second := final("done")
	second.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 20}
	client := newPricedClient(first, second)
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("done")),
			}, nil
		}),
	}
	def := definition()
	def.Settings.MaxTokens = 100
	b := Bindings{Client: client, Tools: fixedTools{lookup},
		Policy: AllowLocalPolicy{}}
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "tokens"},
		def, Start{ID: "start", Budget: &Budget{MaxOutputTokens: 50},
			Input: []llm.Message{llm.UserMessage(llm.Text("lookup"))}}, b)
	if err != nil || out.Stop.Kind != StopLimited ||
		out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitOutputTokens ||
		client.sends != 1 || client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("stop=%+v err=%v inputs=%+v", out.Stop, err, client.inputs)
	}
	out, err = (Engine{}).Apply(context.Background(), out.Turn, def,
		Continue{ID: "raise", Budget: &Budget{MaxOutputTokens: 100}}, b)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 2 ||
		client.inputs[1].Settings.MaxTokens != 50 {
		t.Fatalf("stop=%+v err=%v inputs=%+v", out.Stop, err, client.inputs)
	}
}

func TestBudgetRejectsDecreaseWithoutRecording(t *testing.T) {
	client := newPricedClient(final("unused"))
	out := runBudgetTurn(t, client,
		&Budget{MaxOutputTokens: 50,
			MaxCost: &Money{Amount: 0.0001, Currency: "USD"}},
		llm.Settings{MaxTokens: 100}, nil, nil)
	if out.Stop.Kind != StopLimited {
		t.Fatalf("stop=%+v", out.Stop)
	}
	def := definition()
	def.Settings.MaxTokens = 100
	_, err := (Engine{}).Apply(context.Background(), out.Turn, def,
		Continue{ID: "lower", Budget: &Budget{
			MaxOutputTokens: 40,
			MaxCost:         &Money{Amount: 0.001, Currency: "USD"},
		}}, Bindings{Client: client, Policy: AllowLocalPolicy{}})
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
	result.Message.Content = append([]llm.Content{{
		Kind: llm.ContentHostedUse,
		HostedUse: &llm.HostedUse{
			ID: "search-1", Name: "web_search", Type: "web_search_call",
			Origin: content.Origin{Provider: "test", API: "test.chat",
				Model: "fixture", Scope: "fixture"},
		},
	}}, result.Message.Content...)
	client.results = []llm.Result{result}
	out := runBudgetTurn(t, client, nil,
		llm.Settings{MaxTokens: 100}, nil, nil)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.Requests["web_search"] != 1 ||
		math.Abs(view.Spend.Cost["USD"]-0.01002) > 1e-12 {
		t.Fatalf("spend=%+v", view.Spend)
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
	out, err := (Engine{}).Apply(ctx, Turn{ID: "partial"}, def,
		Start{ID: "start", Budget: &Budget{
			MaxCost: &Money{Amount: 0.01, Currency: "USD"},
		}, Input: []llm.Message{llm.UserMessage(llm.Text("answer"))}},
		Bindings{Client: client, Policy: AllowLocalPolicy{}})
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Estimated != 1 || view.Spend.Usage.OutputTokens != 2 ||
		view.Spend.Cost["USD"] <= 0 {
		t.Fatalf("spend=%+v", view.Spend)
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
	out := runBudgetTurn(t, client,
		&Budget{MaxOutputTokens: 50},
		llm.Settings{MaxTokens: 100}, nil, nil)
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitOutputTokens ||
		out.Turn.AnswerText() != "answer" {
		t.Fatalf("stop=%+v answer=%q", out.Stop, out.Turn.AnswerText())
	}
}

func TestBudgetTotalTokensStopsBetweenCalls(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 40, OutputTokens: 20}
	client := newPricedClient(first, final("unused"))
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("done")),
			}, nil
		}),
	}
	out := runBudgetTurn(t, client,
		&Budget{MaxTotalTokens: 60},
		llm.Settings{MaxTokens: 100}, fixedTools{lookup}, nil)
	if out.Stop.Kind != StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitTotalTokens ||
		out.Stop.Limit.Used != 60 || client.sends != 1 ||
		out.ReadyToContinue {
		t.Fatalf("stop=%+v ready=%t sends=%d",
			out.Stop, out.ReadyToContinue, client.sends)
	}
}
