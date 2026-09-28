package dashi

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestKnownProviderRejectionCanContinueWithNewAttempt(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}, final("done")},
		errors: []error{
			&llm.Error{
				Provider:    "test",
				Category:    llm.ErrorRateLimited,
				Status:      429,
				Disposition: llm.FailureRejected,
			},
		},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopFailed || len(first.Pending) != 0 || client.sends != 1 {
		t.Fatalf("first=%+v err=%v sends=%d", first, err, client.sends)
	}
	settled := first.Turn.Steps[len(first.Turn.Steps)-2]
	if settled.Kind != StepModelSettled || settled.Model.Failure != llm.FailureRejected || settled.Model.Uncertain {
		t.Fatalf("settlement=%+v", settled)
	}
	second, err := (Runner{
		Agent:    testAgent(),
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
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopUncertain || client.sends != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "unsafe-retry"},
	); err == nil || client.sends != 1 {
		t.Fatalf("uncertain attempt retried: err=%v sends=%d", err, client.sends)
	}
	proved, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Settle{
			CommandID: "proof",
			CallID:    "t1:model:1",
			ModelFailure: &ModelFailure{
				Disposition: llm.FailureNotDispatched,
				Reason:      "no dispatch",
				Evidence:    "host dispatch log 42",
			},
		},
	)
	if err != nil || !proved.ReadyToContinue || len(proved.Pending) != 0 ||
		client.sends != 1 {
		t.Fatalf("proof=%+v err=%v sends=%d", proved, err, client.sends)
	}
	retried, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), proved.Turn, Continue{CommandID: "retry"})
	if err != nil || retried.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("retry=%+v err=%v sends=%d", retried, err, client.sends)
	}
}

func TestPartialProviderFailureStaysUncertainDespiteRejectionLabel(
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
				Provider:    "test",
				Category:    llm.ErrorInvalidRequest,
				Status:      400,
				Disposition: llm.FailureRejected,
			},
		},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopUncertain {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	view, err := Replay(first.Turn)
	if err != nil || view.Spend.Usage.InputTokens != 4 {
		t.Fatalf("usage=%+v err=%v", view.Spend.Usage, err)
	}
	falseProof, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Settle{
			CommandID: "false-proof",
			CallID:    "t1:model:1",
			ModelFailure: &ModelFailure{
				Disposition: llm.FailureNotDispatched,
				Reason:      "not sent",
				Evidence:    "claim",
			},
		},
	)
	if err != nil || falseProof.Persistence != PersistenceRejected {
		t.Fatalf(
			"partial response accepted as non-dispatch: out=%+v err=%v",
			falseProof,
			err,
		)
	}
}
