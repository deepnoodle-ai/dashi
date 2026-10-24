package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func TestBrokerCancellationNeverAnswersAnotherOriginalCall(t *testing.T) {
	shown := make(chan brokerPrompt, 4)
	dismissed := make(chan uint64, 4)
	broker := newApprovalBroker(func(p brokerPrompt) <-chan struct{} { shown <- p; return nil }, func(id uint64) { dismissed <- id })
	owner1 := brokerOwner{TaskID: "child-A", Call: dashi.CallRef{TurnID: "A-1", CallID: "write-A"}}
	owner2 := brokerOwner{TaskID: "child-B", Call: dashi.CallRef{TurnID: "B-1", CallID: "question-B"}}
	ctx1, cancel := context.WithCancel(context.WithValue(t.Context(), brokerOwnerKey{}, owner1))
	first := make(chan error, 1)
	go func() {
		_, err := broker.approve(ctx1, ApprovalRequest{Tool: "write", Args: []byte(`{"path":"a"}`)})
		first <- err
	}()
	p1 := <-shown
	second := make(chan workspace.Answer, 1)
	go func() {
		answer, _ := broker.ask(context.WithValue(t.Context(), brokerOwnerKey{}, owner2), Question{Prompt: "B question"})
		second <- answer
	}()
	select {
	case p := <-shown:
		t.Fatalf("overlapping prompt: %+v", p)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if id := <-dismissed; id != p1.ID {
		t.Fatal("wrong cancellation dismissed")
	}
	p2 := <-shown
	if p2.Owner != owner2 || p2.ID == p1.ID {
		t.Fatal("original owner lost")
	}
	if broker.respond(p1.ID, owner1, brokerResponse{approval: Approval{Allow: true, Remember: true}}) {
		t.Fatal("stale grant accepted")
	}
	if broker.respond(p2.ID, owner1, brokerResponse{answer: workspace.Answer{Text: "wrong"}}) {
		t.Fatal("wrong original call accepted")
	}
	if !broker.respond(p2.ID, owner2, brokerResponse{answer: workspace.Answer{Text: "B exact answer"}}) {
		t.Fatal("correct answer rejected")
	}
	if broker.respond(p2.ID, owner2, brokerResponse{}) {
		t.Fatal("duplicate answer accepted")
	}
	if answer := <-second; answer.Text != "B exact answer" {
		t.Fatal(answer)
	}
}

func TestBrokerRejectsUnattributedToolPrompt(t *testing.T) {
	b := newApprovalBroker(func(brokerPrompt) <-chan struct{} { t.Fatal("unattributed prompt shown"); return nil }, nil)
	if _, err := b.approve(t.Context(), ApprovalRequest{}); err == nil {
		t.Fatal("original call missing")
	}
}

func TestBrokerToolHookPreservesExactChildCall(t *testing.T) {
	parent := dashi.CallRef{TurnID: "parent-1", CallID: "task"}
	seen := brokerOwner{}
	hooks := attributeToolHooks(dashi.Hooks{BeforeTool: []dashi.ToolHook{{Name: "approval", Func: func(ctx context.Context, _ dashi.ToolPlan) (dashi.ToolDecision, error) {
		var err error
		seen, err = promptOwner(ctx)
		return dashi.ToolDecision{}, err
	}}}})
	plan := dashi.ToolPlan{Turn: dashi.TurnView{ID: "child-1", Parent: &parent}, Call: tool.Call{ID: "original-write", Name: "write_file", Args: []byte(`{"path":"a"}`)}}
	if _, err := hooks.BeforeTool[0].Func(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if seen.Call.TurnID != plan.Turn.ID || seen.Call.CallID != plan.Call.ID || seen.TaskID != delegate.ChildID(parent.TurnID, parent.CallID) {
		t.Fatalf("hook attribution changed call: %+v", seen)
	}
}

// A canceled prompt's dialog must end before the next prompt opens, or its
// late endDialog clears the approval mode of the prompt after it.
func TestBrokerNextPromptWaitsForCanceledDialog(t *testing.T) {
	h := newHarness(t)
	c := h.app.ctrl
	entered := make(chan struct{})
	modes := make(chan Mode, 1)
	h.ui.answer = func(ctx context.Context, r ApprovalRequest) (Approval, error) {
		if r.Tool == "first" {
			close(entered)
			<-ctx.Done()
			// The UI goroutine returns after the broker saw the cancel.
			time.Sleep(50 * time.Millisecond)
			return Approval{}, ctx.Err()
		}
		time.Sleep(100 * time.Millisecond)
		modes <- c.Mode()
		return Approval{Allow: true}, nil
	}
	owner := func(call string) context.Context {
		return context.WithValue(t.Context(), brokerOwnerKey{}, brokerOwner{Call: dashi.CallRef{TurnID: "t", CallID: call}})
	}
	ctx1, cancel := context.WithCancel(owner("1"))
	first := make(chan error, 1)
	go func() {
		_, err := c.Approve(ctx1, ApprovalRequest{Tool: "first"})
		first <- err
	}()
	<-entered
	second := make(chan error, 1)
	go func() {
		_, err := c.Approve(owner("2"), ApprovalRequest{Tool: "second"})
		second <- err
	}()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if mode := <-modes; mode != ModeApproval {
		t.Fatalf("second prompt mode=%v, want approval", mode)
	}
}
