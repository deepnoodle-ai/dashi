package delegate_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func finalNotice(v dashi.TurnView) bool {
	return v.Stop.Ended || v.Stop.Kind == dashi.StopCompleted || v.Stop.Kind == dashi.StopCanceled
}
func target(h *harness, t *testing.T) delegate.TaskTarget {
	t.Helper()
	v := h.view(t, childTurnID())
	return delegate.TaskTarget{ID: taskID(), TurnID: childTurnID(), LastSeq: v.LastSeq}
}
func attention(h *harness, t *testing.T) *delegate.AttentionError {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	idle, err := h.tasks.Wait(ctx)
	var e *delegate.AttentionError
	if idle || !errors.As(err, &e) || len(e.Tasks) != 1 {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
	return e
}
func waitingChild(t *testing.T, store func(*dashi.MemoryRecorder) dashi.Recorder) *harness {
	t.Helper()
	h := newHarness(t, scripted(childCallsProbe), delegate.Config{NoticeReady: finalNotice}, store)
	h.base = tool.List{tool.Tool{
		Definition: tool.Definition{Name: "write", Description: "External write", Schema: []byte(`{"type":"object"}`)},
		Executor: executor(func(context.Context, tool.Call) (tool.Outcome, error) {
			h.ran.Add(1)
			return tool.Waiting("external worker owns this write"), nil
		}),
	}}
	h.runner.Tools = tool.Join(h.base, h.tasks.Tools())
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	attention(h, t)
	return h
}

func TestTaskRunFinishesExactWaitingChildWithoutRepeatingEffect(t *testing.T) {
	h := waitingChild(t, nil)
	original := target(h, t)
	if h.ran.Load() != 1 {
		t.Fatal("write was not started exactly once")
	}
	if out, err := h.tasks.Run(t.Context(), original, dashi.Resolve{CommandID: "wrong-call", CallID: "other", Outcome: ptrOutcome(tool.Succeeded("verified"))}); err == nil {
		t.Fatalf("wrong call accepted: %+v", out)
	}
	attention(h, t)
	finish := dashi.Resolve{CommandID: "verify-write", CallID: "c1", Outcome: ptrOutcome(tool.Succeeded("Operator verified this exact call completed; its output was not captured."))}
	finished, err := h.tasks.Run(t.Context(), original, finish)
	if err != nil {
		t.Fatal(err)
	}
	if h.ran.Load() != 1 || len(h.tasks.List()) != 1 || h.tasks.List()[0].Turns != 1 {
		t.Fatal("settlement dispatched or created another child")
	}
	if _, err = h.tasks.Run(t.Context(), original, finish); err != nil {
		t.Fatalf("identical stale receipt: %v", err)
	}
	changed := finish
	changed.Outcome = ptrOutcome(tool.Succeeded("different captured output"))
	if _, err = h.tasks.Run(t.Context(), original, changed); !errors.Is(err, dashi.ErrCommandIDReused) {
		t.Fatalf("payload conflict: %v", err)
	}
	if _, err = h.tasks.Run(t.Context(), original, dashi.Continue{CommandID: "stale"}); !errors.Is(err, dashi.ErrStaleTurn) {
		t.Fatalf("stale action: %v", err)
	}
	out, err := h.tasks.Run(t.Context(), delegate.TaskTarget{ID: taskID(), TurnID: childTurnID(), LastSeq: finished.LastSeq}, dashi.Continue{CommandID: "finish"})
	if err != nil || out.Reply != "child finished" {
		t.Fatalf("answer=%q err=%v", out.Reply, err)
	}
	event := h.next(t, delegate.TaskEnded)
	if event.Task.State != "completed" || h.ran.Load() != 1 {
		t.Fatalf("event=%+v writes=%d", event, h.ran.Load())
	}

	if _, err = h.tasks.Run(t.Context(), original, finish); err != nil {
		t.Fatalf("receipt after later work: %v", err)
	}
	select {
	case e := <-h.taskEvents:
		if e.Kind == delegate.TaskEnded {
			t.Fatalf("duplicate notice: %+v", e)
		}
	default:
	}
	child := h.view(t, childTurnID())
	if h.tasks.List()[0].ModelCalls != child.ModelCalls {
		t.Fatal("same Turn spend/count duplicated after control")
	}
}
func ptrOutcome(v tool.Outcome) *tool.Outcome { return &v }

func TestTaskRunLimitedChildRaiseAndDiscard(t *testing.T) {
	sub := scout()
	sub.Agent.Limits.ModelCalls = 1
	h := newHarness(t, scripted(childCallsProbe), delegate.Config{Subagents: []delegate.Subagent{sub}, NoticeReady: finalNotice}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := attention(h, t)
	if e.Tasks[0].State != "limited" {
		t.Fatalf("state=%s", e.Tasks[0].State)
	}
	before := target(h, t)
	out, err := h.tasks.Run(t.Context(), before, dashi.Continue{CommandID: "raise", ModelCalls: 3})
	if err != nil || out.Reply != "child finished" {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	h.next(t, delegate.TaskEnded)
	h2 := waitingChild(t, nil)
	out, err = h2.tasks.Run(t.Context(), target(h2, t), dashi.Cancel{CommandID: "discard", Reason: "operator discarded the child"})
	if err != nil || out.Stop.Kind != dashi.StopCanceled || h2.ran.Load() != 1 {
		t.Fatalf("stop=%+v err=%v writes=%d", out.Stop, err, h2.ran.Load())
	}
	h2.next(t, delegate.TaskEnded)
}

type commandAckRecorder struct {
	*dashi.MemoryRecorder
	command string
	mode    atomic.Int32 // 1 absent, 2 committed but acknowledgement lost
}

func (r *commandAckRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if req.Step.CommandID == r.command && r.mode.Load() != 0 {
		mode := r.mode.Swap(0)
		if mode == 2 {
			if err := r.MemoryRecorder.Commit(ctx, req); err != nil {
				return err
			}
		}
		return errors.New("lost acknowledgement")
	}
	return r.MemoryRecorder.Commit(ctx, req)
}
func TestTaskRunUnknownCommandResolution(t *testing.T) {
	for _, mode := range []int32{1, 2} {
		t.Run(map[int32]string{1: "absent", 2: "committed"}[mode], func(t *testing.T) {
			var recorder *commandAckRecorder
			h := waitingChild(t, func(r *dashi.MemoryRecorder) dashi.Recorder {
				recorder = &commandAckRecorder{MemoryRecorder: r, command: "verify"}
				return recorder
			})
			before := target(h, t)
			cmd := dashi.Resolve{CommandID: "verify", CallID: "c1", Outcome: ptrOutcome(tool.Succeeded("verified without captured output"))}
			recorder.mode.Store(mode)
			if _, err := h.tasks.Run(t.Context(), before, cmd); err == nil {
				t.Fatal("lost acknowledgement not exposed")
			}
			attention(h, t)
			if mode == 1 {
				if _, err := h.tasks.Run(t.Context(), before, dashi.Cancel{CommandID: "new-action"}); !errors.Is(err, delegate.ErrTaskCommandUnknown) {
					t.Fatalf("unknown write admitted different action: %v", err)
				}
			}
			out, err := h.tasks.Run(t.Context(), before, cmd)
			if err != nil {
				t.Fatalf("reconciled exact retry: %v", err)
			}
			if h.ran.Load() != 1 {
				t.Fatal("effect repeated during reconciliation")
			}
			if _, err = h.tasks.Run(t.Context(), delegate.TaskTarget{ID: taskID(), TurnID: childTurnID(), LastSeq: out.LastSeq}, dashi.Continue{CommandID: "finish"}); err != nil {
				t.Fatal(err)
			}
			h.next(t, delegate.TaskEnded)
		})
	}
}

func TestTaskRunOwnsWorkerCancellationAndBusyFence(t *testing.T) {
	for _, closeWorker := range []bool{false, true} {
		t.Run(map[bool]string{false: "Stop", true: "Close"}[closeWorker], func(t *testing.T) {
			g := newGate()
			var calls atomic.Int32
			sub := scout()
			sub.Agent.Limits.ModelCalls = 1
			child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
				if calls.Add(1) == 1 {
					return toolCall("c1", "write", `{}`), nil
				}
				if err := g.wait(ctx); err != nil {
					return llm.Result{}, err
				}
				return text("done"), nil
			}
			h := newHarness(t, scripted(child), delegate.Config{Subagents: []delegate.Subagent{sub}, NoticeReady: finalNotice}, nil)
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			attention(h, t)
			before := target(h, t)
			done := make(chan error, 1)
			go func() {
				_, err := h.tasks.Run(t.Context(), before, dashi.Continue{CommandID: "raise", ModelCalls: 3})
				done <- err
			}()
			g.waitStarted(t)
			if _, err := h.tasks.Run(t.Context(), before, dashi.Cancel{CommandID: "parallel"}); !errors.Is(err, delegate.ErrTaskBusy) {
				t.Fatalf("busy=%v", err)
			}
			if closeWorker {
				if err := h.tasks.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			expected := dashi.StopCanceled
			if closeWorker {
				expected = dashi.StopInterrupted
			}
			if got := h.view(t, childTurnID()).Stop.Kind; got != expected {
				t.Fatalf("recorded stop=%s want %s", got, expected)
			}
		})
	}
}

func TestTaskPreparationAndFrozenBindings(t *testing.T) {
	var prepared, bindings atomic.Int32
	var h *harness
	selected := &fakeClient{handle: func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if !strings.Contains(inputText(in), "admitted parent evidence") || in.Settings.MaxTokens != 321 {
			t.Errorf("missing prepared evidence/settings: %+v", in)
		}
		return text("frozen child route"), nil
	}}
	cfg := delegate.Config{
		PrepareStart: func(_ context.Context, ref dashi.CallRef, s dashi.Start) (dashi.Start, error) {
			prepared.Add(1)
			h.tasks.List() // callback must run outside Tasks.mu
			s.Input = append(s.Input, llm.UserMessage(llm.Text("admitted parent evidence")))
			s.Agent.Settings.MaxTokens = 321
			s.Agent.NoInheritedSettings = true
			s.Agent.Version = "route-fingerprint"
			return s, nil
		},
		BindServices: func(_ context.Context, ref dashi.CallRef, a dashi.Agent) (delegate.ChildServices, error) {
			bindings.Add(1)
			h.tasks.List()
			if a.Settings.MaxTokens != 321 || a.Version != "route-fingerprint" {
				t.Errorf("wrong binding agent: %+v", a)
			}
			return delegate.ChildServices{Client: selected}, nil
		},
	}
	h = newHarness(t, script("parent client must not run child"), cfg, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.next(t, delegate.TaskEnded).Task; got.Reply != "frozen child route" {
		t.Fatalf("task=%+v", got)
	}
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if prepared.Load() != 1 || bindings.Load() != 2 {
		t.Fatalf("preparations=%d bindings=%d", prepared.Load(), bindings.Load())
	}
}

func TestTaskPreparationRejectsProtectedFieldsAndBindingFailure(t *testing.T) {
	for _, field := range []string{"prompt", "destination", "tools", "limits", "parent", "budget", "history", "binding"} {
		t.Run(field, func(t *testing.T) {
			cfg := delegate.Config{PrepareStart: func(_ context.Context, _ dashi.CallRef, s dashi.Start) (dashi.Start, error) {
				switch field {
				case "prompt":
					s.Input[0] = llm.UserMessage(llm.Text("changed task"))
				case "destination":
					s.Agent.Model = "changed"
				case "tools":
					s.Agent.NoTools = true
				case "limits":
					s.Agent.Limits.ModelCalls = 99
				case "parent":
					s.Parent.CallID = "different"
				case "budget":
					s.Budget = &dashi.Budget{}
				case "history":
					s.History = append(s.History, llm.UserMessage(llm.Text("unrelated history")))
				}
				return s, nil
			}}
			if field == "binding" {
				cfg.BindServices = func(context.Context, dashi.CallRef, dashi.Agent) (delegate.ChildServices, error) {
					return delegate.ChildServices{}, errors.New("recorded route unavailable")
				}
			}
			h := newHarness(t, script("must not dispatch child"), cfg, nil)
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			} // setup error finishes the parent's tool; never creates child
			if _, err := h.rec.Load(t.Context(), childTurnID()); !errors.Is(err, dashi.ErrTurnNotFound) {
				t.Fatalf("setup failure recorded child: %v", err)
			}
			if len(h.tasks.List()) != 0 {
				t.Fatal("setup failure invented task success")
			}
		})
	}
}
