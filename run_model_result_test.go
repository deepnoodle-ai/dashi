package dashi

import (
	"errors"
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
	if !nothingWritten(err) {
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
