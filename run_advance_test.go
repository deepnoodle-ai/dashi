package dashi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// savedTurn records startInput without running it, as admission does.
func savedTurn(t *testing.T, runner Runner) string {
	t.Helper()
	out, err := runner.Record(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil {
		t.Fatal(err)
	}
	return out.Turn.ID
}

// refusals records what an InboxRefuser hears.
type refusingInbox struct {
	*MemoryInbox
	mu      sync.Mutex
	refused map[string]error
}

func newRefusingInbox() *refusingInbox {
	return &refusingInbox{MemoryInbox: NewMemoryInbox(), refused: map[string]error{}}
}

func (in *refusingInbox) Refused(_ context.Context, _ string, id string, reason error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.refused[id] = reason
}

func (in *refusingInbox) reason(id string) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.refused[id]
}

func mustPut(t *testing.T, in *MemoryInbox, turnID string, c Command) {
	t.Helper()
	if err := in.Put(turnID, c); err != nil {
		t.Fatal(err)
	}
}

func lastKind(turn Turn) StepKind {
	if s := lastStep(turn); s != nil {
		return s.Kind
	}
	return ""
}

func TestAdvanceRunsSavedStartWithDerivedContinue(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder()}
	id := savedTurn(t, runner)
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted || out.Next() != NextNone {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	continued := stepsOf(out.Turn, StepTurnContinued)
	if len(continued) != 1 || continued[0].CommandID != "continue:1" {
		t.Fatalf("continued=%+v", continued)
	}
	// An ended turn returns at once.
	again, err := runner.Advance(t.Context(), id)
	if err != nil || again.LastSeq != out.LastSeq || client.sends != 1 {
		t.Fatalf("again: seq=%d sends=%d err=%v", again.LastSeq, client.sends, err)
	}
}

func TestAdvanceAppliesToolResultFromInbox(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
		final("sent"),
	}}
	inbox := NewMemoryInbox()
	rec := &capturingRecorder{MemoryRecorder: NewMemoryRecorder()}
	runner := Runner{
		Client: client, Recorder: rec, Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Next() != NextResolvePending ||
		lastKind(out.Turn) != StepTurnStopped {
		t.Fatalf("next=%s stop=%+v err=%v", out.Next(), out.Stop, err)
	}
	result := out.Pending[0].Result(tool.Succeeded("ok"))
	mustPut(t, inbox, id, result)
	out, err = runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "sent" {
		t.Fatalf("stop=%+v reply=%q err=%v", out.Stop, out.Reply, err)
	}
	if inbox.Has(result.CommandID) {
		t.Fatal("the applied result was not acknowledged")
	}
	if got := rec.deliveredFor(StepToolFinished); len(got) != 1 ||
		got[0] != result.CommandID {
		t.Fatalf("CommitRequest.Delivered = %v", got)
	}
}

func TestAdvanceRecordsWaitingStopAfterPartialResults(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`), toolCall("ext-2", "mail", `{}`)),
	}}
	inbox := NewMemoryInbox()
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	out, err := runner.Advance(t.Context(), id)
	if err != nil || len(out.Pending) != 2 {
		t.Fatalf("pending=%v err=%v", out.Pending, err)
	}
	mustPut(t, inbox, id, out.Pending[0].Result(tool.Succeeded("ok")))
	out, err = runner.Advance(t.Context(), id)
	if err != nil || lastKind(out.Turn) != StepTurnStopped ||
		out.Stop.Kind != StopWaiting || len(out.Pending) != 1 ||
		out.Next() != NextResolvePending {
		t.Fatalf("last=%s stop=%+v pending=%v err=%v",
			lastKind(out.Turn), out.Stop, out.Pending, err)
	}
	// Nothing new: Advance records nothing more.
	again, err := runner.Advance(t.Context(), id)
	if err != nil || again.LastSeq != out.LastSeq {
		t.Fatalf("seq %d -> %d err=%v", out.LastSeq, again.LastSeq, err)
	}
}

func TestAdvanceRefusesCommandsAfterTheTurnEnded(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	inbox := newRefusingInbox()
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox}
	id := savedTurn(t, runner)
	done, err := runner.Advance(t.Context(), id)
	if err != nil || !done.Stop.Ended {
		t.Fatalf("stop=%+v err=%v", done.Stop, err)
	}
	late := Resolve{CommandID: "result:x", CallID: "x", Outcome: new(tool.Succeeded("late"))}
	mustPut(t, inbox.MemoryInbox, id, late)
	mustPut(t, inbox.MemoryInbox, id, textSteer("s1", "too late"))
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.LastSeq != done.LastSeq {
		t.Fatalf("seq %d -> %d err=%v", done.LastSeq, out.LastSeq, err)
	}
	for _, id := range []string{"result:x", "s1"} {
		if !errors.Is(inbox.reason(id), ErrTurnEnded) {
			t.Fatalf("%s refused with %v", id, inbox.reason(id))
		}
		if inbox.Has(id) {
			t.Fatalf("%s was not acknowledged", id)
		}
	}
}

func TestAdvanceWarnsAboutACommandTheTurnCannotTake(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	inbox := newRefusingInbox()
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	before, err := runner.Advance(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, inbox.MemoryInbox, id, Resolve{
		CommandID: "result:nope", CallID: "nope", Outcome: new(tool.Succeeded("?")),
	})
	mustPut(t, inbox.MemoryInbox, id, Continue{CommandID: "raise", ModelCalls: 20})
	out, err := runner.Advance(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Next() != before.Next() || out.Stop.Kind != StopWaiting ||
		lastKind(out.Turn) != StepTurnStopped {
		t.Fatalf("next %s -> %s stop=%+v", before.Next(), out.Next(), out.Stop)
	}
	if n := len(warningsOf(out.Turn, WarningInbox)); n != 2 {
		t.Fatalf("inbox warnings = %d", n)
	}
	for _, id := range []string{"result:nope", "raise"} {
		if inbox.reason(id) == nil || inbox.Has(id) {
			t.Fatalf("%s: refused=%v still queued=%t", id, inbox.reason(id), inbox.Has(id))
		}
	}
}

func TestAdvanceAppliesCancelFromInbox(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	inbox := NewMemoryInbox()
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	if _, err := runner.Advance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	mustPut(t, inbox, id, Cancel{CommandID: "cancel", Reason: "user"})
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCanceled || !out.Stop.Ended ||
		lastStep(out.Turn).CommandID != "cancel" || inbox.Has("cancel") {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

// blockingClient blocks each send until its context ends, after telling
// the test the call started.
type blockingClient struct {
	scriptedClient
	started chan struct{}
}

func (c *blockingClient) Send(ctx context.Context, _ llm.Call) (llm.Result, error) {
	c.started <- struct{}{}
	<-ctx.Done()
	return llm.Result{}, ctx.Err()
}

func TestCancelInInboxInterruptsTheRun(t *testing.T) {
	client := &blockingClient{started: make(chan struct{}, 1)}
	inbox := NewMemoryInbox()
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox}
	id := savedTurn(t, runner)
	type result struct {
		out RunResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runner.Advance(t.Context(), id)
		done <- result{out, err}
	}()
	<-client.started
	mustPut(t, inbox, id, Cancel{CommandID: "cancel", Reason: "stop"})
	select {
	case got := <-done:
		last := lastStep(got.out.Turn)
		if got.err != nil || got.out.Stop.Kind != StopCanceled ||
			last.CommandID != "cancel" || got.out.Stop.Reason != "stop" {
			t.Fatalf("stop=%+v err=%v", got.out.Stop, got.err)
		}
		if inbox.Has("cancel") {
			t.Fatal("the cancel was not acknowledged")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not interrupt the run")
	}
}

func TestAdvanceDoesNotContinueFromItsOwnStop(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unneeded")}}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder()}
	id := savedTurn(t, runner)
	pause := make(chan struct{})
	close(pause)
	out, err := runner.Advance(t.Context(), id, WithPause(pause))
	if err != nil || out.Stop.Kind != StopInterrupted || out.Next() != NextContinue {
		t.Fatalf("stop=%+v next=%s err=%v", out.Stop, out.Next(), err)
	}
	if n := len(stepsOf(out.Turn, StepTurnContinued)); n != 1 || client.sends != 0 {
		t.Fatalf("continues=%d sends=%d", n, client.sends)
	}
	// A later call goes on.
	out, err = runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

func TestAdvanceUsesHistory(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder()}
	history := []llm.Message{llm.UserMessage(llm.Text("earlier"))}
	start := startInput()
	start.History = history
	if _, err := runner.Record(t.Context(), Turn{ID: "t1"}, start); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Advance(t.Context(), "t1", WithHistory(history))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(warningsOf(out.Turn, WarningHistoryMismatch)) != 0 {
		t.Fatal("the history was not passed to the Continue")
	}
}

func TestAdvanceNeedsALoader(t *testing.T) {
	runner := Runner{Recorder: writeOnlyRecorder{}}
	if _, err := runner.Advance(t.Context(), "t1"); err == nil {
		t.Fatal("Advance ran without a Loader")
	}
	runner = Runner{Recorder: NewMemoryRecorder()}
	if _, err := runner.Advance(t.Context(), "nope"); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("err = %v", err)
	}
}

type writeOnlyRecorder struct{}

func (writeOnlyRecorder) Commit(context.Context, CommitRequest) error { return nil }

func TestDriveWaitsForTheInboxAndEndsWithTheTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
		final("sent"),
	}}
	inbox := NewMemoryInbox()
	rec := NewMemoryRecorder()
	runner := Runner{
		Client: client, Recorder: rec, Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	done := make(chan error, 1)
	var out RunResult
	go func() {
		var err error
		out, err = runner.Drive(t.Context(), id)
		done <- err
	}()
	// Watch the stops until the turn waits for the result.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for view, err := range FollowStops(ctx, rec, id) {
		if err != nil {
			t.Fatal(err)
		}
		if view.Next() == NextResolvePending {
			mustPut(t, inbox, id, view.Pending[0].Result(tool.Succeeded("ok")))
			break
		}
	}
	select {
	case err := <-done:
		if err != nil || out.Stop.Kind != StopCompleted {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drive did not wake for the result")
	}
}

func TestDriveReturnsWhenContextEnds(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: NewMemoryInbox(),
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	out, err := runner.Drive(ctx, id)
	if !errors.Is(err, context.DeadlineExceeded) || out.Next() != NextResolvePending {
		t.Fatalf("next=%s err=%v", out.Next(), err)
	}
}

func TestAdvanceRecordsDeadlineStop(t *testing.T) {
	agent := testAgent()
	client := &scriptedClient{}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder()}
	agent.Limits.Deadline = time.Now().Add(-time.Second)
	if _, err := runner.Record(t.Context(), Turn{ID: "t2"}, startAgent(agent)); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Advance(t.Context(), "t2")
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitDeadline ||
		!out.Stop.Ended || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
}

func TestFollowStopsYieldsOnlyAtStops(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
		final("sent"),
	}}
	rec := NewMemoryRecorder()
	inbox := NewMemoryInbox()
	runner := Runner{
		Client: client, Recorder: rec, Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var views []TurnView
	for view, err := range FollowStops(ctx, rec, id) {
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, view)
		switch len(views) {
		case 1:
			// The view now: the saved Start, ready to run.
			if view.Next() != NextContinue {
				t.Fatalf("first view next = %s", view.Next())
			}
			if _, err := runner.Advance(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		case 2:
			if view.Next() != NextResolvePending {
				t.Fatalf("second view next = %s", view.Next())
			}
			mustPut(t, inbox, id, view.Pending[0].Result(tool.Succeeded("ok")))
			if _, err := runner.Advance(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(views) != 3 || !views[2].Stop.Ended || views[2].Reply != "sent" {
		t.Fatalf("views=%d last=%+v", len(views), views[len(views)-1].Stop)
	}
}

func TestFollowStopsEndsAtOnceForAnEndedTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	if _, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for view, err := range FollowStops(t.Context(), rec, "t1") {
		if err != nil || !view.Stop.Ended {
			t.Fatalf("view=%+v err=%v", view.Stop, err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("yielded %d views", n)
	}
}

// hookRecorder is a MemoryRecorder that asks before each commit; a non-nil
// error from before refuses the commit, writing nothing.
type hookRecorder struct {
	*MemoryRecorder
	before func(context.Context, CommitRequest) error
}

func (r *hookRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if err := r.before(ctx, req); err != nil {
		return err
	}
	return r.MemoryRecorder.Commit(ctx, req)
}

// failOnce returns a before func that fails the first commit of kind.
func failOnce(kind StepKind) func(context.Context, CommitRequest) error {
	var once sync.Once
	return func(_ context.Context, req CommitRequest) error {
		var err error
		if req.Step.Kind == kind {
			once.Do(func() { err = errors.New("connection lost") })
		}
		return err
	}
}

func TestAdvanceRecoversACrashAfterTheDerivedContinue(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := &hookRecorder{
		MemoryRecorder: NewMemoryRecorder(),
		before:         failOnce(StepModelCallStarted),
	}
	runner := Runner{Client: client, Recorder: rec}
	id := savedTurn(t, runner)
	// The worker records continue:1, then fails before the model call.
	if _, err := runner.Advance(t.Context(), id); err == nil {
		t.Fatal("the failed commit was not reported")
	}
	crashed, err := rec.Load(t.Context(), id)
	if err != nil || lastKind(crashed) != StepTurnContinued {
		t.Fatalf("last=%s err=%v", lastKind(crashed), err)
	}
	if view, _ := crashed.View(); view.Next() != NextRecover || !view.ReadyToContinue {
		t.Fatalf("next=%s ready=%t", view.Next(), view.ReadyToContinue)
	}
	// The next worker recovers it from the record alone.
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
	continued := stepsOf(out.Turn, StepTurnContinued)
	if len(continued) != 2 || continued[1].CommandID != "continue:2" {
		t.Fatalf("continued=%+v", continued)
	}
}

func TestAdvanceWithAStaleLeaseLeavesTheInboxAlone(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
		final("sent"),
	}}
	inbox := newRefusingInbox()
	rec := NewMemoryRecorder()
	runner := Runner{
		Client: client, Recorder: rec, Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	if err := rec.Claim(t.Context(), "t1", "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Record(t.Context(), Turn{ID: "t1"}, startInput(), WithOwner("w1")); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Advance(t.Context(), "t1", WithOwner("w1"))
	if err != nil || out.Next() != NextResolvePending {
		t.Fatalf("next=%s err=%v", out.Next(), err)
	}
	// The lease moves to w2 while w1 still runs.
	if err := rec.Claim(t.Context(), "t1", "w2"); err != nil {
		t.Fatal(err)
	}
	result := out.Pending[0].Result(tool.Succeeded("ok"))
	mustPut(t, inbox.MemoryInbox, "t1", result)
	if _, err := runner.Advance(t.Context(), "t1", WithOwner("w1")); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("stale worker: err = %v", err)
	}
	if !inbox.Has(result.CommandID) || inbox.reason(result.CommandID) != nil {
		t.Fatal("the stale worker refused or acknowledged the result")
	}
	out, err = runner.Advance(t.Context(), "t1", WithOwner("w2"))
	if err != nil || out.Stop.Kind != StopCompleted || inbox.Has(result.CommandID) {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

func TestAdvanceAcknowledgesARecordedCommandWithoutRunningIt(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
		final("sent"),
	}}
	inbox := newRefusingInbox()
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	out, err := runner.Advance(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// A worker recorded the result and crashed before acknowledging it.
	result := out.Pending[0].Result(tool.Succeeded("ok"))
	if _, err := runner.Run(t.Context(), out.Turn, result, WithInbox(nil)); err != nil {
		t.Fatal(err)
	}
	mustPut(t, inbox.MemoryInbox, id, result)
	mustPut(t, inbox.MemoryInbox, id, Continue{CommandID: "continue:9"})
	out, err = runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if inbox.Has(result.CommandID) || inbox.reason(result.CommandID) != nil {
		t.Fatalf("result: queued=%t refused=%v",
			inbox.Has(result.CommandID), inbox.reason(result.CommandID))
	}
	if !errors.Is(inbox.reason("continue:9"), errReservedID) || inbox.Has("continue:9") {
		t.Fatalf("reserved ID refused with %v", inbox.reason("continue:9"))
	}
	if n := len(stepsOf(out.Turn, StepToolFinished)); n != 1 {
		t.Fatalf("tool results recorded = %d", n)
	}
}

func TestAdvanceRecordsRefusalsBeforeACancel(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	inbox := NewMemoryInbox()
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox,
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	if _, err := runner.Advance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	mustPut(t, inbox, id, Resolve{
		CommandID: "result:nope", CallID: "nope", Outcome: new(tool.Succeeded("?")),
	})
	mustPut(t, inbox, id, Cancel{CommandID: "cancel"})
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCanceled {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got := lastStep(out.Turn).Warnings; len(got) != 1 || got[0].Code != WarningInbox {
		t.Fatalf("cancel step warnings = %+v", got)
	}
}

// failingInbox fails every read.
type failingInbox struct{}

func (failingInbox) Waiting(context.Context, string) ([]Command, error) {
	return nil, errors.New("inbox down")
}

func (failingInbox) Delivered(context.Context, string, []string) error { return nil }

func TestDriveWarnsOnceAboutAFailingInbox(t *testing.T) {
	old := driveBackstop
	driveBackstop = 5 * time.Millisecond
	t.Cleanup(func() { driveBackstop = old })
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	rec := NewMemoryRecorder()
	runner := Runner{
		Client: client, Recorder: rec, Inbox: failingInbox{},
		Tools: tool.List{externalTool("mail")},
	}
	id := savedTurn(t, runner)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := runner.Drive(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	turn, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// One warning from Drive and one from the run it made; one stop.
	stops := len(stepsOf(turn, StepTurnStopped))
	warnings := len(warningsOf(turn, WarningInbox))
	if stops != 1 || warnings == 0 || warnings > 2 {
		t.Fatalf("stops=%d inbox warnings=%d", stops, warnings)
	}
}

func TestAdvanceFillsAnInboxContinueFromWithHistory(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	inbox := NewMemoryInbox()
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox}
	history := []llm.Message{llm.UserMessage(llm.Text("earlier"))}
	start := startInput()
	start.History = history
	if _, err := runner.Record(t.Context(), Turn{ID: "t1"}, start); err != nil {
		t.Fatal(err)
	}
	pause := make(chan struct{})
	close(pause)
	out, err := runner.Advance(t.Context(), "t1", WithPause(pause), WithHistory(history))
	if err != nil || out.Next() != NextContinue {
		t.Fatalf("next=%s err=%v", out.Next(), err)
	}
	mustPut(t, inbox, "t1", Continue{CommandID: "go"})
	out, err = runner.Advance(t.Context(), "t1", WithHistory(history))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	continued := stepsOf(out.Turn, StepTurnContinued)
	if len(continued) != 2 || continued[1].CommandID != "go" ||
		len(warningsOf(out.Turn, WarningHistoryMismatch)) != 0 {
		t.Fatalf("continued=%+v: the inbox Continue did not run with the history", continued)
	}
}

func TestDriveWakesAtTheDeadline(t *testing.T) {
	agent := testAgent()
	agent.Limits.Deadline = time.Now().Add(300 * time.Millisecond)
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	runner := Runner{
		Client: client, Recorder: NewMemoryRecorder(), Inbox: NewMemoryInbox(),
		Tools: tool.List{externalTool("mail")},
	}
	if _, err := runner.Record(t.Context(), Turn{ID: "t1"}, startAgent(agent)); err != nil {
		t.Fatal(err)
	}
	// Far below the 15-second backstop.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := runner.Drive(ctx, "t1")
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitDeadline ||
		!out.Stop.Ended {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

func TestInboxCancelDoesNotBreakTheCommitInFlight(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unneeded")}}
	inbox := NewMemoryInbox()
	committing := make(chan struct{}, 1)
	var once sync.Once
	rec := &hookRecorder{
		MemoryRecorder: NewMemoryRecorder(),
		before: func(ctx context.Context, req CommitRequest) error {
			if req.Step.Kind != StepModelCallStarted {
				return nil
			}
			var err error
			once.Do(func() {
				committing <- struct{}{}
				// Long enough for the inbox's Cancel to end the run's
				// context.
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-time.After(300 * time.Millisecond):
				}
			})
			return err
		},
	}
	runner := Runner{Client: client, Recorder: rec, Inbox: inbox}
	id := savedTurn(t, runner)
	type result struct {
		out RunResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runner.Advance(t.Context(), id)
		done <- result{out, err}
	}()
	<-committing
	mustPut(t, inbox, id, Cancel{CommandID: "cancel", Reason: "stop"})
	got := <-done
	if got.err != nil || got.out.Stop.Kind != StopCanceled ||
		lastStep(got.out.Turn).CommandID != "cancel" || inbox.Has("cancel") {
		t.Fatalf("stop=%+v err=%v", got.out.Stop, got.err)
	}
}

func TestInboxCancelAfterTheOutputIsRefused(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final(`{"answer":3}`)}}
	rec := &hookRecorder{
		MemoryRecorder: NewMemoryRecorder(),
		before:         failOnce(StepTurnStopped),
	}
	// The run records the output, then loses its stop.
	if _, err := applyStructuredErr(client, nil, rec); err == nil {
		t.Fatal("the stop was recorded")
	}
	turn, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := replay(turn)
	if err != nil || state.Output == nil {
		t.Fatalf("output=%v err=%v", state.Output, err)
	}
	// A Continue's run is canceled by the inbox before it stops.
	inbox := newRefusingInbox()
	cancel := Cancel{CommandID: "cancel"}
	mustPut(t, inbox.MemoryInbox, "t1", cancel)
	ctx, end := context.WithCancelCause(t.Context())
	end(&inboxCancel{command: cancel})
	rt := runtime{
		ctx: ctx, commitCtx: t.Context(), turn: turn, agent: state.Agent,
		runner: Runner{Recorder: rec}, opts: runOptions{inbox: inbox},
	}
	out := rt.contextStop()
	if rt.err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, rt.err)
	}
	if !errors.Is(inbox.reason("cancel"), errCancelAfterOutput) || inbox.Has("cancel") {
		t.Fatalf("cancel: refused=%v queued=%t", inbox.reason("cancel"), inbox.Has("cancel"))
	}
	if n := len(warningsOf(out.Turn, WarningInbox)); n != 1 {
		t.Fatalf("inbox warnings = %d", n)
	}
}
