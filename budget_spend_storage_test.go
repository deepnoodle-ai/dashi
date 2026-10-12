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
		ID: "agent", Version: "r1",
		Provider: dashitest.Provider,
		API:      dashitest.API,
		Model:    dashitest.Model,
		Limits:   dashi.Limits{ModelCalls: 4},
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
					replies[i].Usage = &llm.Usage{RequestsComplete: true}
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
						Kind: tool.KindProviderTool,
						ProviderTool: &tool.ProviderToolSpec{
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
				view, err := saved.View()
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
					t.Fatal("actual monetary overflow lost its unknown result")
				}
				if scenario == "usage" &&
					view.Spend.String() != "model: 0.75 USD; tools: 5 USD; usage incomplete" {
					t.Fatal("stored usage overflow lost exact model or tool totals")
				}
				if scenario == "derived" &&
					view.Spend.Usage.Requests[llm.MeterUnreported] != math.MaxInt {
					t.Fatal("failed enrichment changed the recorded request count")
				}
				if scenario == "token" && (!view.Stop.Ended ||
					view.Stop.Limit.Kind != dashi.LimitOutputTokens ||
					view.Next() != dashi.NextNone || view.ReadyToContinue) {
					t.Fatal("reopening storage revived a ending token stop")
				}
			})
		}
	}
}

// A host may record an open cost stop while external work remains pending.
// Its late cost cannot silently change whether that recorded stop ended the
// turn.
func TestLateToolOverflowCannotPoisonOpenCostStop(t *testing.T) {
	for _, kind := range []string{"memory", "file", "custom"} {
		t.Run(kind, func(t *testing.T) {
			reply := dashitest.CallTool("call-1", "lookup", `{}`)
			reply.Message.Content = append(reply.Message.Content,
				dashitest.CallTool("call-2", "lookup", `{}`).Message.Content...)
			reply.Usage = &llm.Usage{RequestsComplete: true}
			reply.Cost = storageCharge(money.MustParseAmount("1"))
			lookup, err := tool.Func[struct{}]("lookup", "Fixture",
				func(_ context.Context, call tool.Call, _ struct{}) (tool.Outcome, error) {
					if call.ID == "call-2" {
						return tool.Waiting("external result"), nil
					}
					out := tool.Succeeded("done")
					out.Cost = &tool.Cost{Amount: money.Amount(math.MaxInt64), Currency: "USD"}
					return out, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			runner := dashi.Runner{Client: dashitest.NewClient(reply), Tools: tool.List{lookup}}
			out, err := runner.Run(t.Context(), dashi.Turn{ID: "late-tool"}, dashi.Start{
				CommandID: "start", Agent: storageAgent(),
				Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
			})
			if err != nil {
				t.Fatal(err)
			}
			turn := out.Turn
			turn.Steps[0].Started.Budget = &dashi.Budget{MaxCost: &dashi.Money{
				Amount: money.MustParseAmount("1"), Currency: "USD",
			}}
			turn.Steps[len(turn.Steps)-1].Stop = &dashi.Stop{
				Kind: dashi.StopLimited, Reason: "cost", Limit: &dashi.LimitDetail{
					Kind: dashi.LimitCost, Currency: "USD",
					CostLimit: money.MustParseAmount("1"), CostUsed: money.MustParseAmount("1"),
				},
			}
			before, err := turn.View()
			if err != nil || before.Stop.Ended || before.Spend.CostUnknown != 0 {
				t.Fatalf("invalid fixture: view=%+v err=%v", before, err)
			}
			late := tool.Succeeded("done")
			late.Cost = &tool.Cost{Amount: 1, Currency: "USD"}
			bad := dashi.Step{
				Version: 1, TurnID: turn.ID, Seq: int64(len(turn.Steps) + 1),
				Kind: dashi.StepToolFinished,
				Tool: &dashi.ToolFact{Call: before.Tools["call-2"].Call, Outcome: &late},
			}
			candidate := turn
			candidate.Steps = append(append([]dashi.Step(nil), turn.Steps...), bad)
			if _, err := candidate.View(); !errors.Is(err, dashi.ErrInvalidTurn) {
				t.Fatalf("late overflow changed an open stop: %v", err)
			}

			var recorder dashi.Recorder
			var load func() dashi.Loader
			var custom *uncheckedLateRecorder
			if kind == "custom" {
				custom = &uncheckedLateRecorder{turn: turn}
				recorder, load = custom, func() dashi.Loader { return custom }
			} else {
				recorder, load = accountingStorage(t, kind == "file")
				for _, step := range turn.Steps {
					if err := recorder.Commit(t.Context(), dashi.CommitRequest{
						Step: step, ExpectedSeq: step.Seq - 1,
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			runner.Recorder = recorder
			if _, err := runner.Run(t.Context(), turn, dashi.Resolve{
				CommandID: "late", CallID: "call-2", Outcome: &late,
			}); !errors.Is(err, dashi.ErrInvalidTurn) {
				t.Fatalf("unsafe late charge was not rejected: %v", err)
			}
			if custom != nil && custom.commits != 0 {
				t.Fatal("runner sent the invalid transition to the host recorder")
			}
			saved, err := load().Load(t.Context(), turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			view, err := saved.View()
			if err != nil || len(saved.Steps) != len(turn.Steps) ||
				view.Tools["call-2"].Status != dashi.ToolWaiting ||
				view.Spend.CostUnknown != 0 || view.Stop.Ended {
				t.Fatalf("rejection damaged the recorded turn: view=%+v err=%v", view, err)
			}
		})
	}
}

// This host deliberately does not validate its appended facts. The runner
// must reject the bad transition before calling Commit.
type uncheckedLateRecorder struct {
	turn    dashi.Turn
	commits int
}

func (r *uncheckedLateRecorder) Commit(_ context.Context, req dashi.CommitRequest) error {
	r.commits++
	r.turn.Steps = append(r.turn.Steps, req.Step)
	return nil
}

func (r *uncheckedLateRecorder) Load(context.Context, string) (dashi.Turn, error) {
	return r.turn, nil
}
