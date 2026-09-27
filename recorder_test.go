package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestMemoryRecorderFencesOwnerAndRevision(t *testing.T) {
	rec := NewMemoryRecorder()
	rec.Claim("t1", "worker-1")
	start := Step{Version: 1, TurnID: "t1", Seq: 1, Kind: StepStarted, Started: &StartFact{Definition: definition(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}}
	if err := rec.Commit(context.Background(), CommitRequest{Step: start, ExpectedSeq: 0, Owner: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Commit(context.Background(), CommitRequest{Step: start, ExpectedSeq: 0, Owner: "worker-1"}); err != nil {
		t.Fatalf("identical commit was not idempotent: %v", err)
	}
	deliver := Step{Version: 1, TurnID: "t1", Seq: 2, Kind: StepDelivered, Context: &ContextFact{Items: []ContextItem{{Origin: "user", Message: llm.UserMessage(llm.Text("more"))}}}}
	for _, req := range []CommitRequest{
		{Step: deliver, ExpectedSeq: 1, Owner: "worker-2"},
		{Step: deliver, ExpectedSeq: 0, Owner: "worker-1"},
	} {
		err := rec.Commit(context.Background(), req)
		var commitErr *CommitError
		if !errors.As(err, &commitErr) || commitErr.Persistence != PersistenceRejected || !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("expected rejected revision conflict, got %v", err)
		}
	}
	saved, err := rec.Load("t1")
	if err != nil || len(saved.Steps) != 1 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}
