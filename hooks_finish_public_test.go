package dashi_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestNamedFinishContextSurvivesFreshRunnerResume(t *testing.T) {
	item := dashi.ContextItem{
		ID:      "next-step-delivery",
		Name:    "next-step",
		Origin:  dashi.ContextRuntime,
		Message: llm.UserMessage(llm.Text("End with a concrete next step.")),
	}
	recorder := dashi.NewMemoryRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstClient := dashitest.NewClient(dashitest.Reply("draft"))
	firstMessageItems := 0
	firstRunner := dashi.Runner{
		Client:   firstClient,
		Recorder: recorder,
		Hooks: dashi.Hooks{
			BeforeFinish: []dashi.FinishHook{{
				Name: "next-step",
				Func: func(_ context.Context, plan dashi.FinishPlan) (dashi.FinishDecision, error) {
					if _, ok := plan.Turn.Context(item.Origin, item.Name); ok {
						t.Fatal("first finish already contains the named context")
					}
					firstMessageItems++
					return dashi.FinishDecision{AddMessages: []dashi.ContextItem{item}}, nil
				},
			}},
			BeforeModel: []dashi.ModelHook{{
				Name: "interrupt-after-delivery",
				Func: func(ctx context.Context, plan dashi.ModelPlan) (dashi.ModelDecision, error) {
					if _, ok := plan.Turn.Context(item.Origin, item.Name); ok {
						// The finish hook's added item is committed before
						// this hook runs. Interrupt before another model
						// request can start.
						cancel()
						return dashi.ModelDecision{}, ctx.Err()
					}
					return dashi.ModelDecision{}, nil
				},
			}},
		},
	}
	first, err := firstRunner.Run(ctx, dashi.Turn{ID: "finish-context-resume"}, dashi.Start{
		CommandID: "start",
		Agent:     dashi.Agent{Model: dashitest.Model},
		Input:     []llm.Message{llm.UserMessage(llm.Text("Check the disk."))},
	})
	if err != nil || first.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("first stop=%+v err=%v", first.Stop, err)
	}
	if firstMessageItems != 1 || len(firstClient.Inputs()) != 1 {
		t.Fatalf("first deliveries=%d model calls=%d", firstMessageItems, len(firstClient.Inputs()))
	}

	// Load the durable record. The new runner, client, and hook do not use
	// the first result or the first hook's addition count.
	saved, err := recorder.Load(t.Context(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	freshClient := dashitest.NewClient(dashitest.Reply("Next step: remove unused files."))
	freshChecks := 0
	freshRunner := dashi.Runner{
		Client:   freshClient,
		Recorder: recorder,
		Hooks: dashi.FinishHook{
			Name: "next-step",
			Func: func(_ context.Context, plan dashi.FinishPlan) (dashi.FinishDecision, error) {
				got, ok := plan.Turn.Context(dashi.ContextRuntime, "next-step")
				if !ok || !reflect.DeepEqual(got, item) {
					t.Fatalf("resumed finish context=%+v present=%v, want %+v", got, ok, item)
				}
				freshChecks++
				return dashi.FinishDecision{}, nil
			},
		}.Hooks(),
	}
	resumed, err := freshRunner.Run(t.Context(), saved, dashi.Continue{CommandID: "resume"})
	if err != nil || resumed.Stop.Kind != dashi.StopCompleted ||
		resumed.Reply != "Next step: remove unused files." || freshChecks != 1 {
		t.Fatalf("resumed stop=%+v answer=%q checks=%d err=%v",
			resumed.Stop, resumed.Reply, freshChecks, err)
	}
	if len(freshClient.Inputs()) != 1 {
		t.Fatalf("resumed model calls=%d, want 1", len(freshClient.Inputs()))
	}

	completed, err := recorder.Load(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for _, step := range completed.Steps {
		for _, got := range step.Items() {
			if got.Origin == item.Origin && got.Name == item.Name {
				if !reflect.DeepEqual(got, item) {
					t.Fatalf("recorded context=%+v, want %+v", got, item)
				}
				deliveries++
			}
		}
	}
	if deliveries != 1 {
		t.Fatalf("recorded finish deliveries=%d, want 1", deliveries)
	}
}
