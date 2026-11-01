package dashi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// capturingRecorder keeps every commit request it accepts.
type capturingRecorder struct {
	*MemoryRecorder
	mu       sync.Mutex
	requests []CommitRequest
	// refuseStart, when set, refuses each model_call_started step with it.
	refuseStart error
}

func (r *capturingRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelCallStarted && r.refuseStart != nil {
		return &CommitError{NothingWritten: true, Err: r.refuseStart}
	}
	if err := r.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return nil
}

func (r *capturingRecorder) deliveredFor(kind StepKind) []DeliveredCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []DeliveredCommand
	for _, req := range r.requests {
		if req.Step.Kind == kind {
			out = append(out, req.Delivered...)
		}
	}
	return out
}

func TestCommitRequestCarriesModelCall(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := &capturingRecorder{MemoryRecorder: NewMemoryRecorder()}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished *ModelCallCost
	for _, req := range rec.requests {
		switch req.Step.Kind {
		case StepModelCallStarted:
			started = req.ModelCall
		case StepModelCallFinished:
			finished = req.ModelCall
		default:
			if req.ModelCall != nil {
				t.Fatalf("%s carries a model call", req.Step.Kind)
			}
		}
	}
	if started == nil || started.Finished || finished == nil || !finished.Finished {
		t.Fatalf("started=%+v finished=%+v", started, finished)
	}
	if !canonjson.SameValue(*finished, out.ModelCallCosts[0]) {
		t.Fatalf("finish %+v differs from the view's %+v", *finished, out.ModelCallCosts[0])
	}
}

func TestCommitRequestCarriesDeliveredSteers(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := &capturingRecorder{MemoryRecorder: NewMemoryRecorder()}
	inbox := NewMemoryInbox()
	id := mustSend(t, inbox, "use staging")
	if _, err := (Runner{Client: client, Recorder: rec, Inbox: inbox}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(),
	); err != nil {
		t.Fatal(err)
	}
	if got := rec.deliveredFor(StepMessagesAdded); len(got) != 1 ||
		got[0] != (DeliveredCommand{ID: id, Kind: CommandSteer}) {
		t.Fatalf("delivered = %v", got)
	}
}

func TestRecorderLimitErrorRecordsALimitedStop(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unneeded")}}
	limit := LimitDetail{Kind: LimitHost, Code: "balance", Ends: true}
	rec := &capturingRecorder{
		MemoryRecorder: NewMemoryRecorder(),
		refuseStart:    &LimitError{Reason: "out of credits", Limit: limit},
	}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(),
	)
	if err != nil || out.Stop.Kind != StopLimited || out.Stop.Reason != "out of credits" ||
		out.Stop.Limit == nil || out.Stop.Limit.Code != "balance" ||
		!out.Stop.Ended || out.Next() != NextNone || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
}

func TestMemoryRecorderFencesOwnerAndSeq(t *testing.T) {
	rec := NewMemoryRecorder()
	rec.Claim(t.Context(), "t1", "worker-1")
	start := Step{
		Version:     1,
		TurnID:      "t1",
		Seq:         1,
		Kind:        StepTurnStarted,
		CommandID:   "start",
		CommandHash: "hash",
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
				Version:     1,
				TurnID:      "t1",
				Seq:         1,
				Kind:        StepTurnStarted,
				CommandID:   "start",
				CommandHash: "different",
				Started:     start.Started,
			},
			ExpectedSeq: 0,
			Owner:       "worker-1",
		},
	} {
		err := rec.Commit(t.Context(), req)
		commitErr, ok := errors.AsType[*CommitError](err)
		if !ok || !commitErr.NothingWritten || (!errors.Is(
			err,
			ErrStaleTurn,
		) && !errors.Is(err, ErrCommandIDReused)) {
			t.Fatalf("expected rejected write conflict, got %v", err)
		}
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(saved.Steps) != 1 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

// waitingTurn runs a turn on its own recorder until it waits on an
// external tool, as a host that saves Turns itself would hold it.
func waitingTurn(t *testing.T) RunResult {
	t.Helper()
	runner := Runner{
		Client: &scriptedClient{results: []llm.Result{
			toolUse(toolCall("ext-1", "mail", `{}`)),
		}},
		Tools: tool.List{externalTool("mail")},
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil || out.Next() != NextResolvePending {
		t.Fatalf("next=%s err=%v", out.Next(), err)
	}
	return out
}

func TestNewMemoryRecorderSeedsCopies(t *testing.T) {
	turn := waitingTurn(t).Turn
	want := turn.Clone()
	older := Turn{ID: "t1", Steps: want.Steps[:1]}
	rec := NewMemoryRecorder(older, turn, Turn{ID: "empty"})
	// The caller's later edits do not reach the recorder.
	turn.Steps[0].Started.Input[0].Content[0] = llm.Text("changed")
	turn.Steps = turn.Steps[:1]
	got, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !canonjson.SameValue(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	// Nor do edits to a loaded copy.
	got.Steps[0].Started.Input[0].Content[0] = llm.Text("changed")
	again, _ := rec.Load(t.Context(), "t1")
	if !canonjson.SameValue(again, want) {
		t.Fatal("a loaded copy shares memory with the recorder")
	}
	if _, err := rec.Load(t.Context(), "empty"); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("empty turn: err=%v", err)
	}
}

func TestNewMemoryRecorderSeedRunsAdvance(t *testing.T) {
	waiting := waitingTurn(t)
	turn := waiting.Turn
	inbox := NewMemoryInbox()
	runner := Runner{
		Client:   &scriptedClient{results: []llm.Result{final("sent")}},
		Recorder: NewMemoryRecorder(turn),
		Inbox:    inbox,
		Tools:    tool.List{externalTool("mail")},
	}
	mustPut(t, inbox, turn.ID, waiting.Pending[0].Result(tool.Succeeded("ok")))
	out, err := runner.Advance(t.Context(), turn.ID)
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "sent" {
		t.Fatalf("stop=%+v reply=%q err=%v", out.Stop, out.Reply, err)
	}
	if len(out.Turn.Steps) <= len(turn.Steps) {
		t.Fatalf("Advance recorded nothing: %d steps", len(out.Turn.Steps))
	}
}

func TestNewMemoryRecorderRejectsAnInvalidSeed(t *testing.T) {
	turn := waitingTurn(t).Turn
	// Without its start step the record is not a turn.
	bad := Turn{ID: turn.ID, Steps: turn.Steps[1:]}
	runner := Runner{
		Client:   &scriptedClient{results: []llm.Result{final("done")}},
		Recorder: NewMemoryRecorder(bad),
		Tools:    tool.List{externalTool("mail")},
	}
	if _, err := runner.Advance(t.Context(), bad.ID); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("Advance: err=%v", err)
	}
	if _, err := runner.Drive(t.Context(), bad.ID); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("Drive: err=%v", err)
	}
	_, err := runner.Run(t.Context(), bad, Continue{CommandID: "go"})
	if !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("Run: err=%v", err)
	}
}
