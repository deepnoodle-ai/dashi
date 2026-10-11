package compaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func replacementStart(t *testing.T) (dashi.Start, []llm.Message) {
	t.Helper()
	operator := llm.Message{
		Role: llm.RoleSystem, Content: []llm.Content{llm.Text("Keep logs structured.")},
		Context: &llm.MessageContext{Origin: "operator", Name: "rules"},
	}
	source := []llm.Message{
		operator,
		llm.UserMessage(llm.Text("Project Heron.")),
		llm.AssistantMessage(llm.Text("Noted.")),
	}
	start, err := (&compaction.Compactor{
		TailFraction: -1,
		Model:        dashitest.Model,
	}).Start(
		t.Context(),
		source,
	)
	if err != nil {
		t.Fatal(err)
	}
	start.CommandID = "summarize:heron"
	return start, source
}

func summaryRun(
	t *testing.T,
	runner dashi.Runner,
	turn dashi.Turn,
	cmd dashi.Command,
	opts ...dashi.RunOption,
) dashi.Turn {
	t.Helper()
	out, err := runner.Run(t.Context(), turn, cmd, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return out.Turn
}

func summaryPause() dashi.RunOption {
	pause := make(chan struct{})
	close(pause)
	return dashi.WithPause(pause)
}

func roundTrip[T any](t *testing.T, value T) T {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestValidateReplacementRejectsChangedFrozenMaterial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*dashi.Start, *[]llm.Message, *dashi.Turn)
	}{
		{"source", func(_ *dashi.Start, ms *[]llm.Message, _ *dashi.Turn) {
			(*ms)[1].Content[0].Block.Text.Text = "changed"
		}},
		{
			"source role",
			func(
				_ *dashi.Start,
				ms *[]llm.Message,
				_ *dashi.Turn,
			) {
				(*ms)[1].Role = "invalid"
			},
		},
		{
			"missing hash",
			func(
				s *dashi.Start,
				_ *[]llm.Message,
				_ *dashi.Turn,
			) {
				s.Compaction.Hash = ""
			},
		},
		{
			"missing metadata",
			func(s *dashi.Start, _ *[]llm.Message, _ *dashi.Turn) { s.Compaction = nil },
		},
		{
			"frozen origin",
			func(
				s *dashi.Start,
				_ *[]llm.Message,
				_ *dashi.Turn,
			) {
				s.Origin = dashi.StartUser
			},
		},
		{
			"record hash",
			func(
				_ *dashi.Start,
				_ *[]llm.Message,
				tr *dashi.Turn,
			) {
				tr.Steps[0].Started.Compaction.Hash = "wrong"
			},
		},
		{"retained text", func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) {
			tr.Steps[0].Started.Compaction.Keep[0].Content[0].Block.Text.Text = "changed"
		}},
		{"retained identity", func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) {
			tr.Steps[0].Started.Compaction.Keep[0].Context.Name = "other"
		}},
		{
			"placement",
			func(
				_ *dashi.Start,
				_ *[]llm.Message,
				tr *dashi.Turn,
			) {
				tr.Steps[0].Started.Compaction.SummaryAt = 0
			},
		},
		{"frame", func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) {
			tr.Steps[0].Started.Compaction.Frame = "Other frame"
		}},
		{
			"coverage",
			func(
				_ *dashi.Start,
				_ *[]llm.Message,
				tr *dashi.Turn,
			) {
				tr.Steps[0].Started.Compaction.Through++
			},
		},
		{"initial input", func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) {
			tr.Steps[0].Started.Input[0].Content[0].Block.Text.Text = "changed"
		}},
		{"prior history", func(s *dashi.Start, _ *[]llm.Message, _ *dashi.Turn) {
			s.History = []llm.Message{llm.UserMessage(llm.Text("extra"))}
		}},
		{
			"command ID",
			func(
				s *dashi.Start,
				_ *[]llm.Message,
				_ *dashi.Turn,
			) {
				s.CommandID = "different"
			},
		},
		{"record origin", func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) {
			tr.Steps[0].Started.Origin = dashi.StartUser
			tr.Steps[0].Started.Compaction = nil
		}},
		{
			"invalid record",
			func(_ *dashi.Start, _ *[]llm.Message, tr *dashi.Turn) { tr.Steps[0].Seq++ },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, source := replacementStart(t)
			turn := summaryRun(
				t,
				dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("<summary>Heron.</summary>"))},
				dashi.NewTurn("summary-heron"),
				start,
			)
			tc.change(&start, &source, &turn)
			got, err := compaction.ValidateReplacement(start, source, turn)
			if err == nil || !reflect.DeepEqual(got, compaction.Replacement{}) {
				t.Fatalf("changed material returned replacement: %+v, %v", got, err)
			}
			if !strings.Contains(err.Error(), "summary-heron") {
				t.Fatalf("error lost summary identity: %v", err)
			}
			if tc.name == "invalid record" && !errors.Is(err, dashi.ErrInvalidTurn) {
				t.Fatalf("replay error cause lost: %v", err)
			}
		})
	}
}

func TestValidateReplacementRejectsUnfinishedAndEmptySummaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply llm.Result
	}{
		{"empty", dashitest.Reply("<summary> </summary>")},
		{
			"limited",
			llm.Result{
				Message: llm.AssistantMessage(llm.Text("partial")),
				Stop:    llm.Stop{Kind: llm.StopOutputLimit},
			},
		},
		{"failed", llm.Result{Stop: llm.Stop{Kind: llm.StopIncomplete}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, source := replacementStart(t)
			turn := summaryRun(
				t,
				dashi.Runner{Client: dashitest.NewClient(tc.reply)},
				dashi.NewTurn("summary"),
				start,
			)
			if _, err := compaction.ValidateReplacement(
				start,
				source,
				turn,
			); err == nil {
				t.Fatal("unfinished or empty summary was accepted")
			}
		})
	}
}

func TestValidateReplacementRejectsLaterApplicationInput(t *testing.T) {
	for _, origin := range []dashi.ContextOrigin{
		dashi.ContextUser,
		dashi.ContextRuntime,
	} {
		t.Run(string(origin), func(t *testing.T) {
			start, source := replacementStart(t)
			runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("Heron."))}
			turn := summaryRun(
				t,
				runner,
				dashi.NewTurn("summary"),
				start,
				summaryPause(),
			)
			turn = summaryRun(
				t,
				runner,
				turn,
				dashi.AddMessages{
					CommandID: "extra",
					Items: []dashi.ContextItem{{
						Origin:  origin,
						Message: llm.UserMessage(llm.Text("Changed summary instructions.")),
					}},
				},
			)
			turn = summaryRun(t, runner, turn, dashi.Continue{CommandID: "continue"})
			if _, err := compaction.ValidateReplacement(
				start,
				source,
				turn,
			); err == nil {
				t.Fatal("later application input was accepted")
			}
		})
	}
}

func TestValidateReplacementRejectsHistoryMismatchAfterRestoration(
	t *testing.T,
) {
	start, source := replacementStart(t)
	runner := dashi.Runner{Client: dashitest.NewClient(
		llm.Result{
			Message: llm.AssistantMessage(llm.Text("partial")),
			Stop:    llm.Stop{Kind: llm.StopOutputLimit},
		},
		dashitest.Reply("Heron."),
	)}
	turn := summaryRun(t, runner, dashi.NewTurn("summary"), start, summaryPause())
	turn = summaryRun(
		t,
		runner,
		turn,
		dashi.Continue{
			CommandID: "changed",
			History:   []llm.Message{llm.UserMessage(llm.Text("other source"))},
		},
	)
	turn = summaryRun(t, runner, turn, dashi.Continue{CommandID: "restored"})
	if _, err := compaction.ValidateReplacement(start, source, turn); err == nil {
		t.Fatal("restoration concealed an earlier history mismatch")
	}
}

func TestValidateReplacementAllowsHookContextAndRecovery(t *testing.T) {
	start, source := replacementStart(t)
	hooks := dashi.SeedContext(dashi.ContextItem{
		Origin:  dashi.ContextRuntime,
		Name:    "policy",
		Message: llm.UserMessage(llm.Text("Keep the summary concise.")),
	})
	runner := dashi.Runner{
		Client: dashitest.NewClient(dashitest.Reply("<summary>Heron.</summary>")),
		Hooks:  dashi.JoinHooks(hooks),
	}
	turn := summaryRun(t, runner, dashi.NewTurn("summary"), start, summaryPause())
	turn = summaryRun(t, runner, turn, dashi.Continue{CommandID: "resume"})
	start, source, turn = roundTrip(
		t,
		start,
	), roundTrip(t, source), roundTrip(t, turn)
	before, _ := json.Marshal([]any{start, source, turn})
	got, err := compaction.ValidateReplacement(start, source, turn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Heron." || got.SourceMessages != len(source) || got.TurnID != turn.ID {
		t.Fatalf("replacement metadata differs: %+v", got)
	}
	again, err := compaction.ValidateReplacement(start, source, turn)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("repeated validation differs: %v", err)
	}
	if !reflect.DeepEqual(got, roundTrip(t, got)) {
		t.Fatal("replacement changed in JSON storage")
	}
	clone := got.Clone()
	clone.History[0].Content[0].Block.Text.Text = "clone mutation"
	if got.History[0].Content[0].Block.Text.Text != "Keep logs structured." {
		t.Fatal("clone changed the original replacement")
	}
	got.History[0].Context.Name = "replacement mutation"
	after, _ := json.Marshal([]any{start, source, turn})
	if string(before) != string(after) {
		t.Fatal("validation or replacement mutation changed its inputs")
	}
	if again.History[0].Content[0].Block.Text.Text != "Keep logs structured." {
		t.Fatal("returned text content was shared")
	}
	if again.History[0].Context.Name != "rules" {
		t.Fatal("returned context identity was shared")
	}
	if len(runner.Client.(*dashitest.Client).Inputs()) != 1 {
		t.Fatal("validation executed another model call")
	}
}

// Resolving a recorded unknown model call does not need another send.
func TestValidateReplacementAllowsAuthoritativeModelResolution(t *testing.T) {
	start, source := replacementStart(t)
	runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("Heron."))}
	turn := summaryRun(t, runner, dashi.NewTurn("summary"), start)
	var callID string
	for i, step := range turn.Steps {
		if step.Kind == dashi.StepModelCallStarted {
			callID = step.Model.ModelCallID
			turn.Steps = turn.Steps[:i+1]
			break
		}
	}
	result := dashitest.Reply("Heron.")
	result.Requested = llm.ModelRef{
		Provider: dashitest.Provider,
		Model:    dashitest.Model,
	}
	turn = summaryRun(
		t,
		dashi.Runner{},
		roundTrip(t, turn),
		dashi.Resolve{CommandID: "resolve", CallID: callID, ModelResult: &result},
	)
	turn = summaryRun(
		t,
		runner,
		turn,
		dashi.Continue{CommandID: "finish-resolution"},
	)
	if _, err := compaction.ValidateReplacement(start, source, turn); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReplacementRejectsSteeringContext(t *testing.T) {
	start, source := replacementStart(t)
	inbox := dashi.NewInbox()
	if _, err := inbox.Send(dashi.Steer{Item: dashi.ContextItem{
		ID: "later", Origin: dashi.ContextRuntime,
		Message: llm.UserMessage(llm.Text("Use different source material.")),
	}}); err != nil {
		t.Fatal(err)
	}
	turn := summaryRun(t,
		dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("Heron."))},
		dashi.NewTurn("summary"), start, dashi.WithSteering(inbox),
	)
	if _, err := compaction.ValidateReplacement(start, source, turn); err == nil {
		t.Fatal("steering context was accepted")
	}
}

func TestValidateReplacementAllowsFinishHookFeedback(t *testing.T) {
	start, source := replacementStart(t)
	hook := dashi.FinishHook{
		Name: "refine",
		Func: func(
			_ context.Context,
			p dashi.FinishPlan,
		) (dashi.FinishDecision, error) {
			if p.Turn.HookActed(dashi.HookBeforeFinish, "refine") {
				return dashi.FinishDecision{}, nil
			}
			return dashi.FinishDecision{AddMessages: []dashi.ContextItem{{
				Origin:  dashi.ContextUser,
				Message: llm.UserMessage(llm.Text("Include the project name.")),
			}}}, nil
		},
	}
	turn := summaryRun(t, dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.Reply("Notes."),
			dashitest.Reply("Heron."),
		),
		Hooks: dashi.JoinHooks(hook),
	}, dashi.NewTurn("summary"), start)
	if _, err := compaction.ValidateReplacement(start, source, turn); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReplacementRequiredFailureKeepsWorkUnadmitted(t *testing.T) {
	start, source := replacementStart(t)
	client := dashitest.NewClient(llm.Result{
		Message: llm.AssistantMessage(llm.Text("partial")),
		Stop:    llm.Stop{Kind: llm.StopOutputLimit},
	})
	turn := summaryRun(
		t,
		dashi.Runner{Client: client},
		dashi.NewTurn("summary"),
		start,
	)
	active := roundTrip(t, source)
	if replacement, err := compaction.ValidateReplacement(
		start,
		source,
		turn,
	); err == nil {
		active = replacement.History
		t.Fatal("failed required summary would admit waiting work")
	}
	if !reflect.DeepEqual(active, source) || len(client.Inputs()) != 1 {
		t.Fatal("failed summary changed active history or dispatched waiting work")
	}
}

func TestValidateReplacementAllowsVerifiedModelFailure(t *testing.T) {
	start, source := replacementStart(t)
	client := dashitest.NewClient(dashitest.Reply("Heron."))
	runner := dashi.Runner{Client: client}
	turn := summaryRun(t, runner, dashi.NewTurn("summary"), start)
	var callID string
	for i, step := range turn.Steps {
		if step.Kind == dashi.StepModelCallStarted {
			callID = step.Model.ModelCallID
			turn.Steps = turn.Steps[:i+1]
			break
		}
	}
	turn = summaryRun(t, dashi.Runner{}, roundTrip(t, turn), dashi.Resolve{
		CommandID: "verify-no-send",
		CallID:    callID,
		ModelFailure: &dashi.ModelFailure{
			SendStatus: llm.SendStatusNotSent,
			Reason:     "worker ended before dispatch",
			Evidence:   "application verified dispatch did not occur",
		},
	})
	client.Add(dashitest.Reply("Heron."))
	turn = summaryRun(t, runner, turn, dashi.Continue{CommandID: "resume"})
	if _, err := compaction.ValidateReplacement(start, source, turn); err != nil {
		t.Fatal(err)
	}
}
