package dashi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestKnownProviderRejectionCanContinueWithNewModelCall(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}, final("done")},
		errors: []error{
			&llm.Error{
				Provider:   "test",
				Category:   llm.ErrorRateLimited,
				Status:     429,
				SendStatus: llm.SendStatusProviderRejected,
			},
		},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopFailed || len(first.Pending) != 0 || client.sends != 1 {
		t.Fatalf("first=%+v err=%v sends=%d", first, err, client.sends)
	}
	finished := first.Turn.Steps[len(first.Turn.Steps)-2]
	if finished.Kind != StepModelCallFinished || finished.Model.SendStatus != llm.SendStatusProviderRejected || finished.Model.ResultUnknown {
		t.Fatalf("finish=%+v", finished)
	}
	second, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), first.Turn, Continue{CommandID: "retry"})
	if err != nil || second.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("second=%+v err=%v sends=%d", second, err, client.sends)
	}
}

func TestUnknownProviderFailureRequiresEvidenceBeforeRetry(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}, final("done")},
		errors:  []error{errors.New("connection lost")},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopResultUnknown || client.sends != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "unsafe-retry"},
	); err == nil || client.sends != 1 {
		t.Fatalf("result unknown attempt retried: err=%v sends=%d", err, client.sends)
	}
	proved, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Resolve{
			CommandID: "proof",
			CallID:    "t1:model:1",
			ModelFailure: &ModelFailure{
				SendStatus: llm.SendStatusNotSent,
				Reason:     "no dispatch",
				Evidence:   "host dispatch log 42",
			},
		},
	)
	if err != nil || !proved.ReadyToContinue || len(proved.Pending) != 0 ||
		client.sends != 1 {
		t.Fatalf("proof=%+v err=%v sends=%d", proved, err, client.sends)
	}
	retried, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), proved.Turn, Continue{CommandID: "retry"})
	if err != nil || retried.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("retry=%+v err=%v sends=%d", retried, err, client.sends)
	}
}

func TestPartialProviderFailureStaysResultUnknownDespiteRejectionLabel(
	t *testing.T,
) {
	client := &scriptedClient{
		results: []llm.Result{
			{
				Message: llm.AssistantMessage(llm.Text("partial")),
				Usage:   &llm.Usage{InputTokens: 4},
			},
		},
		errors: []error{
			&llm.Error{
				Provider:   "test",
				Category:   llm.ErrorInvalidRequest,
				Status:     400,
				SendStatus: llm.SendStatusProviderRejected,
			},
		},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	view, err := first.Turn.View()
	if err != nil || view.Spend.Usage.InputTokens != 4 {
		t.Fatalf("usage=%+v err=%v", view.Spend.Usage, err)
	}
	falseProof, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Resolve{
			CommandID: "false-proof",
			CallID:    "t1:model:1",
			ModelFailure: &ModelFailure{
				SendStatus: llm.SendStatusNotSent,
				Reason:     "not sent",
				Evidence:   "claim",
			},
		},
	)
	if !isRefusal(err) {
		t.Fatalf(
			"partial response accepted as non-dispatch: out=%+v err=%v",
			falseProof,
			err,
		)
	}
}

func TestProviderContextLimitRecordsTypedLimitAndEnds(t *testing.T) {
	result := final("cut")
	result.Stop.Kind = llm.StopContextLimit
	client := &scriptedClient{results: []llm.Result{result}}
	out, err := (Runner{Client: client}).Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != LimitContextWindow || !out.Stop.Ended ||
		out.Next() != NextNone {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

// A reply cut off at its max_tokens setting stops limited with
// StopReasonMaxTokens and no Limit, so a host cannot mistake it for the
// turn's output-token budget. AddMessages then Continue sends a request that
// ends with the host's user message, not the cut-off reply (a prefill many
// models reject), and a BeforeModel hook can raise max_tokens for it.
func TestMaxTokensReplyStopsLimited(t *testing.T) {
	cut := final("partial")
	cut.Stop = llm.Stop{Kind: llm.StopOutputLimit, Raw: "max_tokens"}
	client := &scriptedClient{results: []llm.Result{cut, final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopLimited ||
		first.Stop.Reason != StopReasonMaxTokens || first.Stop.Limit != nil ||
		first.Stop.Ended || !first.ReadyToContinue || first.Next() != NextFix {
		t.Fatalf("first: stop=%+v ready=%t next=%s err=%v",
			first.Stop, first.ReadyToContinue, first.Next(), err)
	}
	added, err := runner.Run(t.Context(), first.Turn, AddMessages{
		CommandID: "add",
		Items:     []ContextItem{{Message: llm.UserMessage(llm.Text("go on, briefly"))}},
	})
	if err != nil || client.sends != 1 {
		t.Fatalf("add: stop=%+v err=%v sends=%d", added.Stop, err, client.sends)
	}
	raise := ModelHook{Name: "raise", Func: func(_ context.Context, plan ModelPlan) (ModelDecision, error) {
		settings := plan.Input.Settings
		settings.MaxTokens = 4096
		return ModelDecision{Settings: &settings}, nil
	}}
	second, err := runner.Run(
		t.Context(), added.Turn, Continue{CommandID: "again"}, WithHooks(raise.Hooks()),
	)
	if err != nil || second.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("second: stop=%+v err=%v sends=%d", second.Stop, err, client.sends)
	}
	in := client.inputs[1]
	n := len(in.Messages)
	prev, last := in.Messages[n-2], in.Messages[n-1]
	if in.Settings.MaxTokens != 4096 ||
		prev.Role != llm.RoleAssistant || prev.Text() != "partial" ||
		last.Role != llm.RoleUser || !strings.Contains(last.Text(), "go on, briefly") {
		t.Fatalf("second input: max_tokens=%d prev=%s %q last=%s %q",
			in.Settings.MaxTokens, prev.Role, prev.Text(), last.Role, last.Text())
	}
}
