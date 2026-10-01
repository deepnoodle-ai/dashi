package dashi

import (
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestMemoryRecorderFencesOwnerAndRevision(t *testing.T) {
	rec := NewMemoryRecorder()
	rec.Claim(t.Context(), "t1", "worker-1")
	start := Step{
		Version:       1,
		TurnID:        "t1",
		Seq:           1,
		Kind:          StepTurnStarted,
		CommandID:     "start",
		CommandDigest: "digest",
		Started: &StartFact{
			Agent: testAgent(),
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	}
	if err := rec.Commit(
		t.Context(),
		CommitRequest{Step: start, ExpectedSeq: 0, Owner: "worker-1"},
	); err != nil {
		t.Fatal(err)
	}
	retried := start
	retried.At = time.Now().UTC()
	if err := rec.Commit(
		t.Context(),
		CommitRequest{Step: retried, ExpectedSeq: 0, Owner: "worker-1"},
	); err != nil {
		t.Fatalf("stable command was not idempotent: %v", err)
	}
	deliver := Step{
		Version: 1,
		TurnID:  "t1",
		Seq:     2,
		Kind:    StepMessagesAdded,
		Context: &ContextFact{
			Items: []ContextItem{
				{Origin: "user", Message: llm.UserMessage(llm.Text("more"))},
			},
		},
	}
	for _, req := range []CommitRequest{
		{Step: deliver, ExpectedSeq: 1, Owner: "worker-2"},
		{Step: deliver, ExpectedSeq: 0, Owner: "worker-1"},
		{
			Step: Step{
				Version:       1,
				TurnID:        "t1",
				Seq:           1,
				Kind:          StepTurnStarted,
				CommandID:     "start",
				CommandDigest: "different",
				Started:       start.Started,
			},
			ExpectedSeq: 0,
			Owner:       "worker-1",
		},
	} {
		err := rec.Commit(t.Context(), req)
		commitErr, ok := errors.AsType[*CommitError](err)
		if !ok || commitErr.Persistence != PersistenceRejected || (!errors.Is(
			err,
			ErrRevisionConflict,
		) && !errors.Is(err, ErrCommandConflict)) {
			t.Fatalf("expected rejected write conflict, got %v", err)
		}
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(saved.Steps) != 1 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}
