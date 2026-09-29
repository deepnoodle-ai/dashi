package dashi_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A turn continued by a fresh runner, as in another process, reads the
// helpers' earlier answers from the record: the note is not seeded again and
// the nudge is not repeated.
func TestHookHelpersHoldAcrossResume(t *testing.T) {
	ctx := context.Background()
	nudges := 0
	nudge := dashi.FinishHook{Name: "nudge", Func: func(
		_ context.Context, p dashi.FinishPlan) (dashi.FinishDecision, error) {
		if p.Turn.HookActed(dashi.HookBeforeFinish, "nudge") {
			return dashi.FinishDecision{}, nil
		}
		nudges++
		return dashi.FinishDecision{Deliver: []dashi.ContextItem{{
			Origin:  dashi.ContextRuntime,
			Message: llm.UserMessage(llm.Text("Say more.")),
		}}}, nil
	}}
	broken := dashi.FinishHook{Name: "broken", FailOpen: true, Func: func(
		context.Context, dashi.FinishPlan) (dashi.FinishDecision, error) {
		return dashi.FinishDecision{}, errors.New("broken")
	}}
	seed := dashi.SeedContext(dashi.ContextItem{
		Name:    "note",
		Origin:  dashi.ContextRuntime,
		Message: llm.UserMessage(llm.Text("A note.")),
	})
	hooks := dashi.JoinHooks(seed, nudge, broken)
	start := dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Hi."))},
	}

	// The first process nudges and completes. Cut its record just after the
	// nudge, as if it crashed there.
	first := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.Reply("Short."),
			dashitest.Reply("Longer."),
		),
		Hooks: hooks,
	}
	out, err := first.Run(ctx, dashi.NewTurn("t1"), start)
	if err != nil {
		t.Fatal(err)
	}
	turn := out.Turn
	cut := slices.IndexFunc(turn.Steps, func(s dashi.Step) bool {
		return s.Kind == dashi.StepLoopContinued
	})
	if cut < 0 {
		t.Fatal("no nudge recorded")
	}
	turn.Steps = turn.Steps[:cut+1]
	view, err := dashi.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !view.HookActed(dashi.HookBeforeFinish, "nudge") {
		t.Error("nudge answered but HookActed is false")
	}
	if !view.HookActed(dashi.HookBeforeModel, "note") {
		t.Error("note seeded but HookActed is false")
	}
	if view.HookActed(dashi.HookBeforeFinish, "broken") {
		t.Error("a failed hook counts as acted")
	}

	// A second process continues the turn with a fresh runner.
	second := dashi.Runner{
		Client: dashitest.NewClient(dashitest.Reply("Longer.")),
		Hooks:  hooks,
	}
	out, err = second.Run(ctx, turn, dashi.Continue{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted || out.Answer != "Longer." {
		t.Fatalf("stop %s, answer %q; want completed, Longer.",
			out.Stop.Kind, out.Answer)
	}
	if nudges != 1 {
		t.Errorf("nudged %d times; want 1", nudges)
	}
	seeded := 0
	for _, step := range out.Turn.Steps {
		if step.Context == nil {
			continue
		}
		for _, item := range step.Context.Items {
			if item.Name == "note" {
				seeded++
			}
		}
	}
	if seeded != 1 {
		t.Errorf("note delivered %d times; want 1", seeded)
	}
}
