//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package dashi_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func accountingStorage(t *testing.T, file bool) (dashi.Recorder, func() dashi.Loader) {
	t.Helper()
	if !file {
		r := dashi.NewMemoryRecorder()
		return r, func() dashi.Loader { return r }
	}
	dir := t.TempDir()
	open := func() *filerecorder.Recorder {
		t.Helper()
		r, err := filerecorder.New(filerecorder.Config{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return open(), func() dashi.Loader { return open() }
}

func storageAgent() dashi.Agent {
	return dashi.Agent{
		ID: "agent", Revision: "r1",
		Provider: dashitest.Provider,
		API:      dashitest.API,
		Model:    dashitest.Model,
		Limits:   dashi.Limits{ModelAttempts: 4},
	}
}

func storageCharge(total money.Amount) *llm.Cost {
	return &llm.Cost{
		Total: total, Currency: "USD", Source: llm.CostSourceProviderReported,
	}
}

func TestRecordersReopenSeparateUsageAndMonetaryOverflow(t *testing.T) {
	for _, file := range []bool{false, true} {
		name := "memory"
		if file {
			name = "file"
		}
		for _, scenario := range []string{"usage", "model", "tool", "derived", "token"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				recorder, reopen := accountingStorage(t, file)
				replies := []llm.Result{
					dashitest.CallTool("call-1", "lookup", `{}`),
					dashitest.CallTool("call-2", "lookup", `{}`),
					dashitest.Reply("done"),
				}
				for i := range replies {
					replies[i].Usage = &llm.Usage{RequestsFinal: true}
				}
				modelCosts := []money.Amount{
					money.MustParseAmount("0.5"), money.MustParseAmount("0.25"), 0,
				}
				toolCosts := []money.Amount{
					money.MustParseAmount("2"), money.MustParseAmount("3"),
				}
				if scenario == "usage" {
					replies[0].Usage.InputTokens = math.MaxInt - 1
					replies[1].Usage.OutputTokens = 2
				}
				var budget *dashi.Budget
				if scenario == "token" {
					budget = &dashi.Budget{MaxOutputTokens: 100}
					replies[0].Usage.Requests = map[string]int{
						llm.MeterWebSearch: math.MaxInt,
					}
					replies[1].Usage.Requests = map[string]int{
						llm.MeterWebSearch: 1,
					}
				}
				if scenario == "model" {
					modelCosts[0], modelCosts[1] = money.Amount(math.MaxInt64), 1
				}
				if scenario == "tool" {
					toolCosts[0], toolCosts[1] = money.Amount(math.MaxInt64), 1
				}
				for i := range replies {
					replies[i].Cost = storageCharge(modelCosts[i])
				}
				executions := 0
				lookup, err := tool.Func[struct{}]("lookup", "Lookup fixture data",
					func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
						out := tool.Succeeded("done")
						out.Cost = &tool.Cost{
							Amount: toolCosts[executions], Currency: "USD",
						}
						executions++
						return out, nil
					})
				if err != nil {
					t.Fatal(err)
				}
				tools := tool.List{lookup}
				if scenario == "derived" {
					replies = []llm.Result{dashitest.Reply("done")}
					replies[0].Usage = &llm.Usage{
						Requests: map[string]int{llm.MeterUnreported: math.MaxInt},
					}
					replies[0].Cost = storageCharge(modelCosts[0])
					tools = tool.List{{Definition: tool.Definition{
						Name: "search",
						Kind: tool.KindProviderHosted,
						Hosted: &tool.HostedSpec{
							Provider: dashitest.Provider,
							API:      dashitest.API,
							Type:     "web_search",
						},
					}}}
				}
				out, err := (dashi.Runner{
					Client:   dashitest.NewClient(replies...),
					Recorder: recorder,
					Tools:    tools,
				}).Run(t.Context(), dashi.Turn{ID: "recorded"}, dashi.Start{
					CommandID: "start", Agent: storageAgent(),
					Input:  []llm.Message{llm.UserMessage(llm.Text("go"))},
					Budget: budget,
				})
				if err != nil {
					t.Fatal(err)
				}
				saved, err := reopen().Load(t.Context(), out.Turn.ID)
				if err != nil {
					t.Fatal(err)
				}
				view, err := dashi.Replay(saved)
				if err != nil {
					t.Fatal(err)
				}
				if view.Spend.String() != out.Spend.String() ||
					view.Spend.UsageIncomplete != out.Spend.UsageIncomplete {
					t.Fatal("reopening storage changed accounting meanings")
				}
				usageFailed := scenario == "usage" || scenario == "derived" ||
					scenario == "token"
				if view.Spend.UsageIncomplete != usageFailed {
					t.Fatal("usage and monetary failures became indistinguishable")
				}
				if usageFailed && view.Spend.CostUnknown != 0 {
					t.Fatal("usage-only failure changed exact monetary certainty")
				}
				if !usageFailed && view.Spend.CostUnknown != 1 {
					t.Fatal("actual monetary overflow lost its uncertainty")
				}
				if scenario == "usage" &&
					view.Spend.String() != "model: 0.75 USD; tools: 5 USD; usage incomplete" {
					t.Fatal("stored usage overflow lost exact model or tool totals")
				}
				if scenario == "derived" &&
					view.Spend.Usage.Requests[llm.MeterUnreported] != math.MaxInt {
					t.Fatal("failed enrichment changed the recorded request count")
				}
				if scenario == "token" && (!view.Stop.Terminal ||
					view.Stop.Limit.Kind != dashi.LimitOutputTokens ||
					view.Next() != dashi.NextNone || view.ReadyToContinue) {
					t.Fatal("reopening storage revived a terminal token stop")
				}
			})
		}
	}
}

func TestRecordersRejectUnsafeTokenIntentBeforeStorage(t *testing.T) {
	for _, file := range []bool{false, true} {
		recorder, reopen := accountingStorage(t, file)
		first := dashitest.CallTool("call-1", "lookup", `{}`)
		first.Usage = &llm.Usage{InputTokens: math.MaxInt - 1}
		first.Cost = storageCharge(0)
		lookup, err := tool.Func[struct{}]("lookup", "Lookup fixture data",
			func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				return tool.Succeeded("done"), nil
			})
		if err != nil {
			t.Fatal(err)
		}
		client := dashitest.NewClient(first, dashitest.Reply("never sent"))
		out, err := (dashi.Runner{
			Client: client, Recorder: recorder, Tools: tool.List{lookup},
		}).Run(t.Context(), dashi.Turn{ID: "guarded"}, dashi.Start{
			CommandID: "start", Agent: storageAgent(),
			Input:  []llm.Message{llm.UserMessage(llm.Text("go"))},
			Budget: &dashi.Budget{MaxTotalTokens: math.MaxInt},
		})
		var rejected *dashi.CommitError
		if !errors.As(err, &rejected) ||
			rejected.Persistence != dashi.PersistenceRejected ||
			rejected.Step.Kind != dashi.StepModelIntent {
			t.Fatalf("unsafe intent was not rejected before admission: %v", err)
		}
		if len(client.Inputs()) != 1 {
			t.Fatal("unsafe prospective intent reached model dispatch")
		}
		saved, err := reopen().Load(t.Context(), out.Turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved.Steps) != len(out.Turn.Steps) {
			t.Fatal("rejected intent reached storage")
		}
		if saved.Steps[len(saved.Steps)-1].Kind == dashi.StepModelIntent {
			t.Fatal("rejected model intent is the stored last step")
		}
	}
}
