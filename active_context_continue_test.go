package dashi_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type stopAfterActiveApplication struct {
	dashi.Recorder
	cancel  context.CancelFunc
	loseAck bool
	fired   bool
}

func (r *stopAfterActiveApplication) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if !r.fired && req.Step.Kind == dashi.StepContextCompacted {
		r.fired = true
		if r.loseAck {
			return errors.New("lost application acknowledgement")
		}
		r.cancel()
	}
	return nil
}

func TestActiveCompactionContinueAfterApplication(t *testing.T) {
	for _, loseAck := range []bool{false, true} {
		for _, attempts := range []int{3, 5} {
			name := "interrupt"
			if loseAck {
				name = "lost acknowledgement"
			}
			if attempts == 3 {
				name += "/one attempt left"
			} else {
				name += "/spare attempts"
			}
			t.Run(name, func(t *testing.T) {
				effects := 0
				read := tool.MustFunc("read", "Read evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
					effects++
					return tool.Succeeded(strings.Repeat("evidence ", 1000)), nil
				})
				replies := []llm.Result{
					dashitest.CallTool("read-1", "read", "{}"),
					dashitest.Reply("<summary>Original objective retained. Evidence read finished. " + strings.Repeat("settled evidence ", 300) + "</summary>"),
					dashitest.Reply("Fixed and verified."),
				}
				for i := range replies {
					replies[i].Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
				}
				client := activeExampleClient{dashitest.NewClient(replies...)}
				recorder := dashi.NewMemoryRecorder()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fault := &stopAfterActiveApplication{Recorder: recorder, cancel: cancel, loseAck: loseAck}
				first, err := (dashi.Runner{Client: client, Tools: tool.List{read}, Recorder: fault}).Run(ctx, dashi.Turn{ID: "application-recovery"}, dashi.Start{
					Agent: dashi.Agent{
						Model: dashitest.Model, Settings: llm.Settings{MaxTokens: 256},
						Limits: dashi.Limits{ModelAttempts: attempts, NoReservedFinalAnswer: true},
						// The applied replacement remains above this soft threshold.
						ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, Fraction: .3, MaxTokens: 256},
					},
					Input: []llm.Message{llm.UserMessage(llm.Text("Investigate, fix, and verify the original objective."))},
				})
				if loseAck {
					if _, ok := errors.AsType[*dashi.CommitError](err); !ok {
						t.Fatalf("expected unknown application commit, got %v", err)
					}
				} else if err != nil || first.Stop.Kind != dashi.StopInterrupted {
					t.Fatalf("interrupt: stop=%+v err=%v", first.Stop, err)
				}
				if !fault.fired || len(client.Inputs()) != 2 || effects != 1 {
					t.Fatalf("application boundary: fired=%v sends=%d effects=%d", fault.fired, len(client.Inputs()), effects)
				}
				loaded, err := recorder.Load(t.Context(), "application-recovery")
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(loaded)
				if err != nil {
					t.Fatal(err)
				}
				var restored dashi.Turn
				if err := json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				view, err := dashi.Replay(restored)
				if err != nil || view.ModelAttempts != 2 || len(view.ContextCompactions) != 1 {
					t.Fatalf("reload: attempts=%d summaries=%d err=%v", view.ModelAttempts, len(view.ContextCompactions), err)
				}
				input := client.Inputs()[0]
				input.Messages, err = dashi.ProjectContext(restored)
				if err != nil {
					t.Fatal(err)
				}
				req, err := client.Prepare(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				estimate := llm.EstimatedInputTokens(req)
				allowance := 16000 - req.Snapshot().Effective.MaxTokens
				if float64(estimate) < .3*float64(allowance) || estimate > allowance {
					t.Fatalf("replacement must exceed soft threshold and fit hard limit: estimate=%d allowance=%d", estimate, allowance)
				}
				out, err := (dashi.Runner{Client: client, Tools: tool.List{read}, Recorder: recorder}).Run(t.Context(), restored, dashi.Continue{CommandID: "resume"})
				if err != nil || out.Stop.Kind != dashi.StopCompleted || out.Answer != "Fixed and verified." {
					t.Fatalf("resume: stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
				}
				if effects != 1 || len(client.Inputs()) != 3 || len(out.ContextCompactions) != 1 || out.ModelAttempts != 3 {
					t.Fatalf("resume repeated work: effects=%d sends=%d summaries=%d attempts=%d", effects, len(client.Inputs()), len(out.ContextCompactions), out.ModelAttempts)
				}
				if out.Spend.Usage.InputTokens != 300 || out.Spend.Usage.OutputTokens != 6 {
					t.Fatalf("resume accounting: %+v", out.Spend)
				}
				summaryIntents := 0
				for _, step := range out.Turn.Steps {
					if step.Kind == dashi.StepModelIntent && step.Model.Purpose == dashi.ModelPurposeCompaction {
						summaryIntents++
					}
				}
				if summaryIntents != 1 {
					t.Fatalf("summary dispatches=%d", summaryIntents)
				}
			})
		}
	}
}
