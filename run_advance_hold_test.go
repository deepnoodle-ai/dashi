package dashi

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// balanceHook is a BeforeSend hook that holds the turn at a host limit
// until funded is set, as a balance check would.
type balanceHook struct {
	mu     sync.Mutex
	funded bool
	deny   bool
	asks   int
	plans  []SendPlan
}

func (h *balanceHook) fund() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.funded = true
}

func (h *balanceHook) asked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asks
}

func (h *balanceHook) hooks() Hooks {
	return Hooks{BeforeSend: []SendHook{{
		Name: "balance",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.asks++
			h.plans = append(h.plans, plan)
			switch {
			case h.deny:
				return SendDecision{Deny: true, Reason: "account closed"}, nil
			case !h.funded:
				return SendDecision{
					Reason: "balance too low",
					Limit:  &LimitDetail{Kind: LimitHost, Code: "balance"},
				}, nil
			}
			return SendDecision{}, nil
		},
	}}}
}

func TestAdvanceRetriesAHeldSendOnceBeforeSendAllowsIt(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	hook := &balanceHook{}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec, Hooks: hook.hooks()}
	id := savedTurn(t, runner)
	held, err := runner.Advance(t.Context(), id)
	if err != nil || held.Next() != NextFix || held.Stop.Limit == nil ||
		held.Stop.Limit.Code != "balance" || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", held.Stop, client.sends, err)
	}
	// Still unfunded: each pass asks again and records nothing.
	for range 3 {
		out, err := runner.Advance(t.Context(), id)
		if err != nil || out.LastSeq != held.LastSeq {
			t.Fatalf("a poll of the hold wrote steps: seq=%d want %d err=%v",
				out.LastSeq, held.LastSeq, err)
		}
	}
	if n := hook.asked(); n != 4 {
		t.Fatalf("BeforeSend asked %d times, want 4", n)
	}
	// The hook saw the prepared request each time.
	if last := hook.plans[len(hook.plans)-1]; len(last.Request.Snapshot().Input.Messages) == 0 {
		t.Fatal("the asking pass gave BeforeSend no prepared request")
	}
	if client.sends != 0 {
		t.Fatalf("asking sent %d calls", client.sends)
	}
	// The host funds the turn; the next pass runs it.
	hook.fund()
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
	continued := stepsOf(out.Turn, StepTurnContinued)
	if len(continued) != 2 || continued[1].CommandID != "continue:"+strconv.FormatInt(held.LastSeq, 10) {
		t.Fatalf("continued=%+v", continued)
	}
}

func TestAdvanceRecordsADifferentAnswerToAHeldSend(t *testing.T) {
	client := &scriptedClient{}
	hook := &balanceHook{}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Hooks: hook.hooks()}
	id := savedTurn(t, runner)
	if _, err := runner.Advance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	hook.mu.Lock()
	hook.deny = true
	hook.mu.Unlock()
	out, err := runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Kind != StopRefused || client.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
	// A refusal is not a hold: later passes do not ask.
	asks := hook.asked()
	if _, err := runner.Advance(t.Context(), id); err != nil || hook.asked() != asks {
		t.Fatalf("asked %d times after the refusal, err=%v", hook.asked()-asks, err)
	}
}

func TestDrivePollsAHeldSendWithoutWritingAndRunsOnceFunded(t *testing.T) {
	old := driveBackstop
	driveBackstop = 5 * time.Millisecond
	t.Cleanup(func() { driveBackstop = old })
	client := &scriptedClient{results: []llm.Result{final("done")}}
	hook := &balanceHook{}
	rec := NewMemoryRecorder()
	runner := Runner{
		Client: client, Recorder: rec, Hooks: hook.hooks(), Inbox: NewMemoryInbox(),
	}
	id := savedTurn(t, runner)
	done := make(chan error, 1)
	var out RunResult
	go func() {
		var err error
		out, err = runner.Drive(t.Context(), id)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for hook.asked() < 5 {
		if time.Now().After(deadline) {
			t.Fatal("Drive did not poll the hold")
		}
		time.Sleep(time.Millisecond)
	}
	held, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Start, derived Continue, and the hold: polling added nothing.
	if len(held.Steps) != 3 || lastKind(held) != StepTurnStopped {
		t.Fatalf("held turn has %d steps, last %s", len(held.Steps), lastKind(held))
	}
	hook.fund()
	select {
	case err := <-done:
		if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
			t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drive did not run the funded turn")
	}
}

func TestWithHistoryMismatchRejectsDerivedAndInboxContinues(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	inbox := newRefusingInbox()
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec, Inbox: inbox}
	start := startInput()
	start.History = []llm.Message{llm.UserMessage(llm.Text("earlier"))}
	if _, err := runner.Record(t.Context(), Turn{ID: "t1"}, start); err != nil {
		t.Fatal(err)
	}
	other := []llm.Message{llm.UserMessage(llm.Text("something else"))}
	reject := WithHistoryMismatch(HistoryMismatchReject)
	// The derived Continue.
	if _, err := runner.Advance(t.Context(), "t1", WithHistory(other), reject); !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("derived Continue: err=%v", err)
	}
	// An inbox Continue that leaves OnHistoryMismatch empty.
	mustPut(t, inbox.MemoryInbox, "t1", Continue{CommandID: "go", History: other})
	if _, err := runner.Advance(t.Context(), "t1", WithHistory(other), reject); !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("after the inbox Continue: err=%v", err)
	}
	if !errors.Is(inbox.reason("go"), ErrHistoryMismatch) {
		t.Fatalf("inbox Continue refusal = %v", inbox.reason("go"))
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(stepsOf(saved, StepTurnContinued)) != 0 || client.sends != 0 {
		t.Fatalf("a rejected Continue ran: steps=%d sends=%d err=%v",
			len(saved.Steps), client.sends, err)
	}
	if _, err := runner.Advance(t.Context(), "t1", WithHistoryMismatch("maybe")); err == nil {
		t.Fatal("an invalid history-mismatch behavior was accepted")
	}
}

// gatedClient holds each send until the test releases it.
type gatedClient struct {
	scriptedClient
	started chan struct{}
	release chan struct{}
}

func (c *gatedClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.started <- struct{}{}
	<-c.release
	return c.scriptedClient.Send(ctx, call)
}

func TestFollowNextYieldsAWorkingViewAfterEachStop(t *testing.T) {
	client := &gatedClient{
		scriptedClient: scriptedClient{results: []llm.Result{final("done")}},
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
	}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	id := savedTurn(t, runner)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var next []Next
	advanced := make(chan error, 1)
	for view, err := range FollowNext(ctx, rec, id) {
		if err != nil {
			t.Fatal(err)
		}
		next = append(next, view.Next())
		switch len(next) {
		case 1:
			go func() {
				_, err := runner.Advance(t.Context(), id)
				advanced <- err
			}()
		case 2:
			// The run is in its model call: release it.
			<-client.started
			close(client.release)
		}
	}
	if err := <-advanced; err != nil {
		t.Fatal(err)
	}
	want := []Next{NextContinue, NextRunning, NextNone}
	if len(next) != len(want) || next[0] != want[0] || next[1] != want[1] || next[2] != want[2] {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestFollowNextNeverShowsAnEndedTurnWorking(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors:  []error{errors.New("connection lost")},
	}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	first, err := startTurn(testAgent(), runner)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	late := final("done late")
	if _, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID: "late", CallID: "t1:model:1", ModelResult: &late,
	}); err != nil {
		t.Fatal(err)
	}
	saved, _ := rec.Load(t.Context(), "t1")
	if lastKind(saved) == StepTurnStopped {
		t.Fatal("the late result was not recorded after the stop")
	}
	// The turn's last step is the late finish, yet it reads as ended once.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	n := 0
	for view, err := range FollowNext(ctx, rec, "t1") {
		if err != nil || !view.Stop.Ended || view.Next() != NextNone {
			t.Fatalf("view=%+v next=%s err=%v", view.Stop, view.Next(), err)
		}
		n++
	}
	if n != 1 || ctx.Err() != nil {
		t.Fatalf("yielded %d views; ctx err=%v", n, ctx.Err())
	}
}

func TestDriveAndRunsUnwatchTheInbox(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	inbox := NewMemoryInbox()
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Inbox: inbox}
	id := savedTurn(t, runner)
	if _, err := runner.Drive(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	inbox.mu.Lock()
	n := len(inbox.ready)
	inbox.mu.Unlock()
	if n != 0 {
		t.Fatalf("the inbox still holds %d Ready channels", n)
	}
}

func TestMemoryInboxKeepsAChannelWhileAnyoneWatches(t *testing.T) {
	in := NewMemoryInbox()
	ready, unwatchDrive := in.Ready("t1")
	same, unwatchRun := in.Ready("t1")
	if ready != same {
		t.Fatal("watchers of one turn got different channels")
	}
	unwatchRun()
	unwatchRun() // a second call is harmless
	mustPut(t, in, "t1", Cancel{CommandID: "c"})
	select {
	case <-ready:
	default:
		t.Fatal("the remaining watcher was not signaled")
	}
	unwatchDrive()
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.ready) != 0 {
		t.Fatal("the channel outlived its watchers")
	}
}

func TestAskingAHeldTurnCommitsAndShowsNothing(t *testing.T) {
	client := &scriptedClient{}
	hook := &balanceHook{}
	rec := &capturingRecorder{MemoryRecorder: NewMemoryRecorder()}
	var mu sync.Mutex
	events := 0
	runner := Runner{
		Client: client, Recorder: rec, Hooks: hook.hooks(),
		Tools: tool.List{externalTool("mail")},
		Observer: func(Event) {
			mu.Lock()
			defer mu.Unlock()
			events++
		},
	}
	id := savedTurn(t, runner)
	if _, err := runner.Advance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let the Observer drain
	rec.mu.Lock()
	commits := len(rec.requests)
	rec.mu.Unlock()
	mu.Lock()
	seen := events
	mu.Unlock()
	if _, err := runner.Advance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if len(rec.requests) != commits || events != seen || hook.asked() != 2 ||
		client.sends != 0 {
		t.Fatalf("asking: commits %d->%d events %d->%d asks=%d sends=%d",
			commits, len(rec.requests), seen, events, hook.asked(), client.sends)
	}
	if plan := hook.plans[1]; len(plan.Request.Snapshot().Input.Tools) != 1 {
		t.Fatal("the asked request did not offer the turn's tools")
	}
}

func TestAdvancePollsABeforeModelHoldWithoutWriting(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	var mu sync.Mutex
	funded := false
	hooks := Hooks{BeforeModel: []ModelHook{{
		Name: "balance",
		Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			mu.Lock()
			defer mu.Unlock()
			if funded {
				return ModelDecision{}, nil
			}
			return ModelDecision{Stop: &Stop{
				Kind: StopLimited, Reason: "balance too low",
				Limit: &LimitDetail{Kind: LimitHost, Code: "balance"},
			}}, nil
		},
	}}}
	runner := Runner{Client: client, Recorder: NewMemoryRecorder(), Hooks: hooks}
	id := savedTurn(t, runner)
	held, err := runner.Advance(t.Context(), id)
	if err != nil || !heldStop(held.TurnView) {
		t.Fatalf("stop=%+v err=%v", held.Stop, err)
	}
	if out, err := runner.Advance(t.Context(), id); err != nil || out.LastSeq != held.LastSeq {
		t.Fatalf("a poll wrote steps: seq=%d want %d err=%v", out.LastSeq, held.LastSeq, err)
	}
	mu.Lock()
	funded = true
	mu.Unlock()
	if out, err := runner.Advance(t.Context(), id); err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

func TestAdvanceRetriesARecorderHoldOncePerCall(t *testing.T) {
	old := driveBackstop
	driveBackstop = 5 * time.Millisecond
	t.Cleanup(func() { driveBackstop = old })
	client := &scriptedClient{results: []llm.Result{final("done")}}
	var mu sync.Mutex
	funded := false
	rec := &hookRecorder{
		MemoryRecorder: NewMemoryRecorder(),
		before: func(_ context.Context, req CommitRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if req.Step.Kind == StepModelCallStarted && !funded {
				return &CommitError{NothingWritten: true, Err: &LimitError{
					Reason: "balance too low",
					Limit:  LimitDetail{Kind: LimitHost, Code: "balance"},
				}}
			}
			return nil
		},
	}
	runner := Runner{Client: client, Recorder: rec, Inbox: NewMemoryInbox()}
	id := savedTurn(t, runner)
	held, err := runner.Advance(t.Context(), id)
	if err != nil || !heldStop(held.TurnView) {
		t.Fatalf("stop=%+v err=%v", held.Stop, err)
	}
	// The hooks allow the call, so asking cannot tell: one real retry per
	// call, then Drive's polls write nothing.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	out, err := runner.Drive(ctx, id)
	if !errors.Is(err, context.DeadlineExceeded) || out.LastSeq != held.LastSeq+2 {
		t.Fatalf("seq=%d want %d err=%v", out.LastSeq, held.LastSeq+2, err)
	}
	mu.Lock()
	funded = true
	mu.Unlock()
	if out, err := runner.Advance(t.Context(), id); err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}
