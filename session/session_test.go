package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// TestSessionLastTurnRules builds each row of the last-turn table as turn 1,
// then sends a new message from a fresh handle, with and without Exclusive
// and with each TurnLiveness answer.
func TestSessionLastTurnRules(t *testing.T) {
	type expect struct {
		busy   dashi.Next // empty: the Start closes or starts
		callID bool
		closed bool
	}
	closes := func(session.Liveness, bool) expect { return expect{closed: true} }
	resume := func(l session.Liveness, exclusive bool) expect {
		switch {
		case l == session.LivenessLive:
			return expect{busy: dashi.NextRunning}
		case l == session.LivenessStopped || exclusive:
			return expect{closed: true}
		}
		return expect{busy: dashi.NextResume}
	}
	rows := []struct {
		name  string
		build func(t *testing.T, rec *liveRecorder)
		next  dashi.Next
		want  func(session.Liveness, bool) expect
	}{
		{
			name:  "ended",
			build: buildCompleted,
			next:  dashi.NextNone,
			want:  func(session.Liveness, bool) expect { return expect{} },
		},
		{
			name:  "uncertain call, no stop",
			build: buildCrashInTool("charge"),
			next:  dashi.NextResume,
			want: func(l session.Liveness, _ bool) expect {
				if l == session.LivenessLive {
					// A live worker runs the call: no CallID to settle.
					return expect{busy: dashi.NextRunning}
				}
				return expect{busy: dashi.NextResume, callID: true}
			},
		},
		{
			name:  "uncertain call, stopped",
			build: buildUncertainStop,
			next:  dashi.NextFix,
			want: func(session.Liveness, bool) expect {
				return expect{busy: dashi.NextFix, callID: true}
			},
		},
		{
			name:  "no stop, in a model call",
			build: buildCrashInModel,
			next:  dashi.NextResume,
			want:  resume,
		},
		{
			name:  "no stop, in a read-only call",
			build: buildCrashInTool("lookup"),
			next:  dashi.NextResume,
			want:  resume,
		},
		{
			name:  "waiting",
			build: buildWaiting,
			next:  dashi.NextResolvePending,
			want: func(session.Liveness, bool) expect {
				return expect{busy: dashi.NextResolvePending}
			},
		},
		{
			name:  "interrupted",
			build: buildPaused,
			next:  dashi.NextContinue,
			want:  closes,
		},
		{
			name:  "answered and ready",
			build: buildAnswered,
			next:  dashi.NextContinue,
			want:  closes,
		},
		{
			name:  "model-call limit",
			build: buildAttemptLimit,
			next:  dashi.NextRaiseModelCalls,
			want:  closes,
		},
		{
			name:  "budget limit",
			build: buildBudgetLimit,
			next:  dashi.NextRaiseBudget,
			want:  closes,
		},
		{
			name:  "refused",
			build: buildRefused,
			next:  dashi.NextFix,
			want:  closes,
		},
		{
			name:  "structured output at the model-call limit",
			build: buildOutputAtLimit,
			next:  dashi.NextRaiseModelCalls,
			want:  closes,
		},
	}
	livenesses := []session.Liveness{
		"", // no TurnLiveness capability
		session.LivenessUnknown,
		session.LivenessLive,
		session.LivenessStopped,
		"error",
	}
	for _, row := range rows {
		for _, l := range livenesses {
			for _, exclusive := range []bool{false, true} {
				name := row.name + "/" + string(l)
				if l == "" {
					name = row.name + "/none"
				}
				if exclusive {
					name += "/exclusive"
				}
				t.Run(name, func(t *testing.T) {
					rec := newLiveRecorder()
					row.build(t, rec)
					rec.set(l)
					var r dashi.Recorder = rec
					if l == "" {
						r = rec.MemoryRecorder
					}
					client := dashitest.NewClient(dashitest.Reply("ok"))
					s := handle(r, client)
					s.Exclusive = exclusive
					st, err := s.Open(t.Context())
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					want := row.want(l, exclusive)
					if row.next == dashi.NextResume && l == session.LivenessLive {
						if st.Next != dashi.NextRunning {
							t.Fatalf("open next = %s; want running", st.Next)
						}
					} else if st.Next != row.next {
						t.Fatalf("open next = %s; want %s", st.Next, row.next)
					}
					out, err := s.Send(t.Context(), "next")
					if want.busy != "" {
						requireBusy(t, err, want.busy, want.callID)
						requireTurns(t, rec, 1)
						return
					}
					if err != nil {
						t.Fatalf("send: %v", err)
					}
					if out.Turn.ID != session.TurnID("s", 2) || out.Stop.Kind != dashi.StopCompleted {
						t.Fatalf("new turn %s stopped %s", out.Turn.ID, out.Stop.Kind)
					}
					closed := hasWarning(out.SessionWarnings, session.WarningTurnClosed)
					if closed != want.closed {
						t.Fatalf("turn_closed warning = %v; want %v", closed, want.closed)
					}
					first := load(t, rec, session.TurnID("s", 1))
					if first.Next() != dashi.NextNone {
						t.Fatalf("turn 1 next = %s after the new turn", first.Next())
					}
					if want.closed && first.Output == nil &&
						(first.Stop.Kind != dashi.StopCanceled ||
							first.Stop.Reason != session.CloseReason) {
						t.Fatalf("turn 1 stop = %+v; want a close", first.Stop)
					}
				})
			}
		}
	}
}

// TestSessionBusyMessages checks that each busy error names its way out.
func TestSessionBusyMessages(t *testing.T) {
	tests := []struct {
		err  session.TurnBusyError
		want []string
	}{
		{
			session.TurnBusyError{TurnID: "s-7", Next: dashi.NextResume},
			[]string{"s-7", "Continue{}", "Cancel{}"},
		},
		{
			session.TurnBusyError{TurnID: "s-7", Next: dashi.NextResolvePending},
			[]string{"s-7", "answer", "Cancel{}"},
		},
		{
			session.TurnBusyError{TurnID: "s-7", Next: dashi.NextRunning},
			[]string{"s-7", "steer", "stop it"},
		},
		{
			session.TurnBusyError{
				TurnID: "s-7",
				Next:   dashi.NextResume,
				CallID: "c-3",
			},
			[]string{"c-3", "s-7", "Resolve{}", "Cancel{}"},
		},
		{
			session.TurnBusyError{
				TurnID:    "s-7",
				Next:      dashi.NextResume,
				Duplicate: true,
			},
			[]string{"s-7", "already runs this command"},
		},
	}
	for _, tc := range tests {
		msg := tc.err.Error()
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%q does not name %q", msg, w)
			}
		}
		if !errors.Is(&tc.err, session.ErrTurnBusy) {
			t.Errorf("%q does not match ErrTurnBusy", msg)
		}
	}
}

// TestSessionUncertainCallCancel is the effect check: a worker exits while
// its tool may still act, so a new Exclusive handle refuses a new message
// and names the call. Cancel{} records that the host accepts the risk, and
// the next message starts.
func TestSessionUncertainCallCancel(t *testing.T) {
	rec := newLiveRecorder()
	buildCrashInTool("charge")(t, rec)
	s := handle(rec.MemoryRecorder, dashitest.NewClient(dashitest.Reply("ok")))
	s.Exclusive = true
	_, err := s.Send(t.Context(), "next")
	busy := requireBusy(t, err, dashi.NextResume, true)
	if busy.CallID != "call-1" {
		t.Fatalf("busy call = %q; want call-1", busy.CallID)
	}
	out, err := s.Run(t.Context(), dashi.Cancel{Reason: "the user discarded it"})
	if err != nil || out.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("cancel: %v, stop %+v", err, out.Stop)
	}
	out, err = s.Send(t.Context(), "next")
	if err != nil || out.Turn.ID != session.TurnID("s", 2) {
		t.Fatalf("send after cancel: %v, turn %s", err, out.Turn.ID)
	}
	if hasWarning(out.SessionWarnings, session.WarningTurnClosed) {
		t.Fatal("a canceled turn was closed again")
	}
	first := load(t, rec, session.TurnID("s", 1))
	if first.Stop.Reason != "the user discarded it" ||
		first.Tools["call-1"].Status != dashi.ToolStarted {
		t.Fatalf("turn 1 stop %+v, call %s", first.Stop, first.Tools["call-1"].Status)
	}
}

// TestSessionLostWorker expires the old worker's claim during a model call
// and during a call that is not idempotent. A new message refuses, takeover
// with Continue{} never reruns the tool, no new turn overlaps the unsettled
// call, and the old worker's late commit is rejected.
func TestSessionLostWorker(t *testing.T) {
	t.Run("model call", func(t *testing.T) {
		rec := newLiveRecorder()
		first := session.TurnID("s", 1)
		claim(t, rec, first, "a")
		g := newGate()
		a := handle(rec, gatedClient{dashitest.NewClient(dashitest.Reply("late")), g})
		a.New = true
		done := make(chan error, 1)
		go func() {
			_, err := a.Send(context.Background(), "hello", dashi.WithOwner("a"))
			done <- err
		}()
		g.requireEntered(t, done)
		claim(t, rec, first, "b") // a's lease expired; b took it
		b := handle(rec, dashitest.NewClient(dashitest.Reply("done")))
		_, err := b.Send(t.Context(), "again")
		requireBusy(t, err, dashi.NextResume, false)
		out, err := b.Run(t.Context(), dashi.Continue{}, dashi.WithOwner("b"))
		if err != nil || out.Stop.Kind != dashi.StopCompleted || out.Reply != "done" {
			t.Fatalf("takeover: %v, %s %q", err, out.Stop.Kind, out.Reply)
		}
		close(g.release)
		if err := <-done; !errors.Is(err, dashi.ErrRevisionConflict) {
			t.Fatalf("late commit: %v; want a revision conflict", err)
		}
		requireTurns(t, rec, 1)
		if load(t, rec, first).Reply != "done" {
			t.Fatal("the late result changed the turn")
		}
	})
	t.Run("tool call", func(t *testing.T) {
		rec := newLiveRecorder()
		first := session.TurnID("s", 1)
		claim(t, rec, first, "a")
		g := newGate()
		charge := &chargeTool{gate: g}
		a := handle(rec, dashitest.NewClient(
			dashitest.CallTool("call-1", "charge", `{}`),
		))
		a.Runner.Tools = charge.tools()
		a.New = true
		done := make(chan error, 1)
		go func() {
			_, err := a.Send(context.Background(), "pay", dashi.WithOwner("a"))
			done <- err
		}()
		g.requireEntered(t, done)
		claim(t, rec, first, "b")
		b := handle(rec, dashitest.NewClient(dashitest.Reply("paid")))
		b.Runner.Tools = charge.tools()
		b.Exclusive = true
		_, err := b.Send(t.Context(), "again")
		requireBusy(t, err, dashi.NextResume, true)
		out, err := b.Run(t.Context(), dashi.Continue{}, dashi.WithOwner("b"))
		if err != nil || out.Stop.Kind != dashi.StopResultUnknown {
			t.Fatalf("takeover: %v, stop %+v", err, out.Stop)
		}
		if n := charge.runs.Load(); n != 1 {
			t.Fatalf("charge ran %d times; want 1", n)
		}
		_, err = b.Send(t.Context(), "again")
		requireBusy(t, err, dashi.NextFix, true)
		close(g.release)
		if err := <-done; !errors.Is(err, dashi.ErrRevisionConflict) {
			t.Fatalf("late commit: %v; want a revision conflict", err)
		}
		requireTurns(t, rec, 1)
		// The host checked the charge and settles the call under its lease.
		settle := out.Pending[0].Result(tool.Succeeded("charged once"))
		if _, err := b.Run(t.Context(), settle, dashi.WithOwner("b")); err != nil {
			t.Fatal(err)
		}
		out, err = b.Run(t.Context(), dashi.Continue{}, dashi.WithOwner("b"))
		if err != nil || out.Stop.Kind != dashi.StopCompleted {
			t.Fatalf("answer: %v, stop %+v", err, out.Stop)
		}
		if n := charge.runs.Load(); n != 1 {
			t.Fatalf("charge ran %d times; want 1", n)
		}
	})
}

// TestSessionCommandConflict has two Starts with one command ID and
// different input choose turn 1. The loser gets ErrCommandConflict whether
// it finds the other turn before its run or at its turn_started commit, and
// it starts no turn and makes no model call.
func TestSessionCommandConflict(t *testing.T) {
	for _, at := range []string{"before the run", "at commit"} {
		t.Run(at, func(t *testing.T) {
			rec := dashi.NewMemoryRecorder()
			other := handle(rec, dashitest.NewClient(dashitest.Reply("other")))
			other.New = true
			keyed := func(text string) dashi.Start {
				start := dashi.Say(text)
				start.CommandID = "k"
				return start
			}
			race := &interleave{
				MemoryRecorder: rec,
				atCommit:       at == "at commit",
				turnID:         session.TurnID("s", 1),
				other: func() {
					if _, err := other.Run(context.Background(), keyed("goodbye")); err != nil {
						t.Error(err)
					}
				},
			}
			client := dashitest.NewClient(dashitest.Reply("loser"))
			s := handle(race, client)
			s.New = true
			_, err := s.Run(t.Context(), keyed("hello"))
			if !errors.Is(err, dashi.ErrCommandConflict) {
				t.Fatalf("err = %v; want ErrCommandConflict", err)
			}
			if n := len(client.Inputs()); n != 0 {
				t.Fatalf("model calls = %d; want 0", n)
			}
			requireTurns(t, rec, 1)
		})
	}
}

// TestSessionRunningOnThisHandle refuses a second Start on the handle that
// runs the last turn, whatever Exclusive says.
func TestSessionRunningOnThisHandle(t *testing.T) {
	g := newGate()
	s := handle(nil, gatedClient{dashitest.NewClient(dashitest.Reply("ok")), g})
	s.New = true
	s.Exclusive = true
	done := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), "first")
		done <- err
	}()
	g.requireEntered(t, done)
	st, err := s.Open(t.Context())
	if err != nil || st.Next != dashi.NextRunning {
		t.Fatalf("open: %v, next %s", err, st.Next)
	}
	_, err = s.Send(t.Context(), "second")
	requireBusy(t, err, dashi.NextRunning, false)
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestSessionRunningCallNamesNoCall runs a call that is not idempotent and
// sends while it runs. A handle that knows a worker runs the turn (its own,
// or TurnLiveness says live) refuses as running, with no CallID and no
// Resolve{} or Cancel{} advice; a handle that knows of no worker names the
// call.
func TestSessionRunningCallNamesNoCall(t *testing.T) {
	rec := newLiveRecorder()
	g := newGate()
	charge := &chargeTool{gate: g}
	a := handle(rec, dashitest.NewClient(
		dashitest.CallTool("call-1", "charge", `{}`),
		dashitest.Reply("paid"),
	))
	a.Runner.Tools = charge.tools()
	a.New = true
	done := make(chan error, 1)
	go func() {
		_, err := a.Send(context.Background(), "pay")
		done <- err
	}()
	g.requireEntered(t, done)

	running := func(name string, s *session.Session) {
		_, err := s.Send(t.Context(), "again")
		busy := requireBusy(t, err, dashi.NextRunning, false)
		msg := busy.Error()
		if strings.Contains(msg, "Resolve{}") || strings.Contains(msg, "Cancel{}") {
			t.Fatalf("%s: running turn advises %q", name, msg)
		}
	}
	running("own handle", a)
	rec.set(session.LivenessLive)
	running("live elsewhere", handle(rec, dashitest.NewClient()))

	rec.set(session.LivenessUnknown)
	_, err := handle(rec, dashitest.NewClient()).Send(t.Context(), "again")
	if busy := requireBusy(t, err, dashi.NextResume, true); busy.CallID != "call-1" {
		t.Fatalf("unknown liveness: call = %q; want call-1", busy.CallID)
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireTurns(t, rec, 1)
}

// buildCompleted records a completed turn 1.
func buildCompleted(t *testing.T, rec *liveRecorder) {
	t.Helper()
	sendFirst(t, rec, dashitest.NewClient(dashitest.Reply("hi")), nil)
}

// buildCrashInModel records a turn 1 whose worker stopped during a model
// call: an intent with no result and no stop.
func buildCrashInModel(t *testing.T, rec *liveRecorder) {
	t.Helper()
	fault := &dashitest.FaultRecorder{
		Recorder: rec,
		Kind:     dashi.StepModelCallFinished,
		Fault:    dashitest.Reject,
	}
	sendErr(t, fault, dashitest.NewClient(dashitest.Reply("hi")), nil)
}

// buildCrashInTool records a turn 1 whose worker stopped while the named
// tool ran: started, with no result and no stop.
func buildCrashInTool(name string) func(*testing.T, *liveRecorder) {
	return func(t *testing.T, rec *liveRecorder) {
		t.Helper()
		fault := &dashitest.FaultRecorder{
			Recorder: rec,
			Kind:     dashi.StepToolFinished,
			Fault:    dashitest.Reject,
		}
		client := dashitest.NewClient(dashitest.CallTool("call-1", name, `{}`))
		sendErr(t, fault, client, func(s *session.Session) {
			s.Runner.Tools = (&chargeTool{}).tools()
		})
	}
}

// buildUncertainStop resumes a crash in a tool that is not idempotent, so
// the call is recorded uncertain.
func buildUncertainStop(t *testing.T, rec *liveRecorder) {
	t.Helper()
	buildCrashInTool("charge")(t, rec)
	s := handle(rec.MemoryRecorder, dashitest.NewClient())
	s.Runner.Tools = (&chargeTool{}).tools()
	out, err := s.Run(t.Context(), dashi.Continue{})
	if err != nil || out.Stop.Kind != dashi.StopResultUnknown {
		t.Fatalf("resume: %v, stop %+v", err, out.Stop)
	}
}

func buildWaiting(t *testing.T, rec *liveRecorder) {
	t.Helper()
	client := dashitest.NewClient(dashitest.CallTool("call-1", "charge", `{}`))
	sendFirst(t, rec, client, func(s *session.Session) {
		s.Runner.Tools = (&chargeTool{}).tools()
		s.Runner.Hooks = dashi.RequireApproval("charge")
	})
}

func buildAnswered(t *testing.T, rec *liveRecorder) {
	t.Helper()
	buildWaiting(t, rec)
	s := handle(rec.MemoryRecorder, dashitest.NewClient())
	st, err := s.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), st.Last.Pending[0].Deny("no")); err != nil {
		t.Fatal(err)
	}
}

func buildPaused(t *testing.T, rec *liveRecorder) {
	t.Helper()
	pause := make(chan struct{})
	close(pause)
	s := handle(rec, dashitest.NewClient())
	s.New = true
	out, err := s.Send(t.Context(), "hello", dashi.WithPause(pause))
	if err != nil || out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("paused: %v, stop %+v", err, out.Stop)
	}
}

func buildAttemptLimit(t *testing.T, rec *liveRecorder) {
	t.Helper()
	client := dashitest.NewClient(dashitest.CallTool("call-1", "lookup", `{}`))
	sendFirst(t, rec, client, func(s *session.Session) {
		s.Runner.Tools = (&chargeTool{}).tools()
		s.Agent.Limits = dashi.Limits{ModelCalls: 1, NoReservedFinalReply: true}
	})
}

func buildBudgetLimit(t *testing.T, rec *liveRecorder) {
	t.Helper()
	reply := dashitest.CallTool("call-1", "lookup", `{}`)
	reply.Usage = &llm.Usage{InputTokens: 1, OutputTokens: 10}
	s := handle(rec, dashitest.NewClient(reply))
	s.Runner.Tools = (&chargeTool{}).tools()
	s.New = true
	start := dashi.Say("hello")
	start.Budget = &dashi.Budget{MaxOutputTokens: 5}
	if _, err := s.Run(t.Context(), start); err != nil {
		t.Fatal(err)
	}
}

func buildRefused(t *testing.T, rec *liveRecorder) {
	t.Helper()
	reply := dashitest.Reply("I can't help with that.")
	reply.Stop = llm.Stop{Kind: llm.StopRefusal}
	sendFirst(t, rec, dashitest.NewClient(reply), nil)
}

func buildOutputAtLimit(t *testing.T, rec *liveRecorder) {
	t.Helper()
	client := dashitest.NewClient(
		dashitest.CallTool("call-1", "lookup", `{}`),
		dashitest.Reply(`{"answer":"42"}`),
	)
	s := handle(rec, client)
	s.Runner.Tools = (&chargeTool{}).tools()
	s.Agent.Limits = dashi.Limits{ModelCalls: 2}
	s.New = true
	start := dashi.Say("hello")
	start.OutputSchema = json.RawMessage(`{"type":"object"}`)
	out, err := s.Run(t.Context(), start)
	if err != nil || out.Output == nil {
		t.Fatalf("output: %v, %v", err, out.Output)
	}
}

// sendFirst starts turn 1 of session "s" on a New handle and requires success.
func sendFirst(
	t *testing.T,
	rec dashi.Recorder,
	client llm.Client,
	setup func(*session.Session),
) {
	t.Helper()
	s := handle(rec, client)
	s.New = true
	if setup != nil {
		setup(s)
	}
	if _, err := s.Send(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
}

// sendErr starts turn 1 and requires the recording failure a fault gives.
func sendErr(
	t *testing.T,
	rec dashi.Recorder,
	client llm.Client,
	setup func(*session.Session),
) {
	t.Helper()
	s := handle(rec, client)
	s.New = true
	if setup != nil {
		setup(s)
	}
	if _, err := s.Send(t.Context(), "hello"); !errors.Is(err, dashitest.ErrInjected) {
		t.Fatalf("send: %v; want the injected fault", err)
	}
}

// handle returns a handle on session "s".
func handle(rec dashi.Recorder, client llm.Client) *session.Session {
	return &session.Session{
		ID:     "s",
		Runner: dashi.Runner{Client: client, Recorder: rec},
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
}

func requireBusy(
	t *testing.T,
	err error,
	next dashi.Next,
	callID bool,
) *session.TurnBusyError {
	t.Helper()
	var busy *session.TurnBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v; want *TurnBusyError", err)
	}
	if busy.Next != next || (busy.CallID != "") != callID {
		t.Fatalf("busy = %+v; want next %s, call %v", busy, next, callID)
	}
	return busy
}

// requireTurns checks that session "s" has exactly n turns.
func requireTurns(t *testing.T, rec dashi.Loader, n int) {
	t.Helper()
	if _, err := rec.Load(t.Context(), session.TurnID("s", n)); err != nil {
		t.Fatalf("turn %d: %v", n, err)
	}
	_, err := rec.Load(t.Context(), session.TurnID("s", n+1))
	if !errors.Is(err, dashi.ErrTurnNotFound) {
		t.Fatalf("turn %d exists: %v", n+1, err)
	}
}

func load(t *testing.T, rec dashi.Loader, id string) dashi.TurnView {
	t.Helper()
	turn, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func claim(t *testing.T, rec *liveRecorder, id, owner string) {
	t.Helper()
	if err := rec.Claim(t.Context(), id, owner); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(warnings []dashi.Warning, code dashi.WarningCode) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// liveRecorder is a MemoryRecorder with a fixed TurnLiveness answer.
type liveRecorder struct {
	*dashi.MemoryRecorder
	mu       sync.Mutex
	liveness session.Liveness
}

func newLiveRecorder() *liveRecorder {
	return &liveRecorder{MemoryRecorder: dashi.NewMemoryRecorder()}
}

func (r *liveRecorder) set(l session.Liveness) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liveness = l
}

func (r *liveRecorder) Liveness(context.Context, string) (session.Liveness, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.liveness {
	case "error":
		return "", errors.New("lease table unavailable")
	case "":
		return session.LivenessUnknown, nil
	}
	return r.liveness, nil
}

// gate holds a model or tool call until the test releases it.
type gate struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) wait(ctx context.Context) error {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requireEntered waits until a call reaches the gate, failing the test if
// the run ends first.
func (g *gate) requireEntered(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("the run ended before its call: %v", err)
	}
}

// gatedClient holds each model call at its gate.
type gatedClient struct {
	*dashitest.Client
	gate *gate
}

func (c gatedClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if err := c.gate.wait(ctx); err != nil {
		return llm.Result{}, err
	}
	return c.Client.Send(ctx, call)
}

// chargeTool offers "charge", which is not idempotent, and "lookup", which
// is read-only. With a gate, charge holds until the test releases it.
type chargeTool struct {
	runs atomic.Int32
	gate *gate
}

func (c *chargeTool) tools() tool.Toolset {
	charge, _ := tool.Func(
		"charge",
		"Charge a card.",
		func(ctx context.Context, _ tool.Call, _ struct{}) (tool.Outcome, error) {
			c.runs.Add(1)
			if c.gate != nil {
				if err := c.gate.wait(ctx); err != nil {
					return tool.Outcome{}, err
				}
			}
			return tool.Succeeded("charged"), nil
		},
	)
	lookup, _ := tool.Func(
		"lookup",
		"Look something up.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("found"), nil
		},
		tool.ReadOnly(),
	)
	return tool.List{charge, lookup}
}

// interleave runs other once, before the first Load of turnID or, with
// atCommit, before the first turn_started commit, so another worker starts
// that turn first.
type interleave struct {
	*dashi.MemoryRecorder
	atCommit bool
	turnID   string
	other    func()
	once     sync.Once
}

func (r *interleave) Load(ctx context.Context, id string) (dashi.Turn, error) {
	if !r.atCommit && id == r.turnID {
		r.once.Do(r.other)
	}
	return r.MemoryRecorder.Load(ctx, id)
}

func (r *interleave) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if r.atCommit && req.Step.Kind == dashi.StepTurnStarted {
		r.once.Do(r.other)
	}
	return r.MemoryRecorder.Commit(ctx, req)
}
