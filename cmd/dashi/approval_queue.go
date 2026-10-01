package main

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type brokerOwner struct {
	TaskID string
	Call   dashi.CallRef
}
type brokerOwnerKey struct{}

// attributeToolHooks pins the exact hook call before the workspace policy
// reaches its callback. CurrentCall alone is not present in BeforeTool hooks.
func attributeToolHooks(hooks dashi.Hooks) dashi.Hooks {
	hooks.BeforeTool = slices.Clone(hooks.BeforeTool)
	for i := range hooks.BeforeTool {
		original := hooks.BeforeTool[i].Func
		hooks.BeforeTool[i].Func = func(ctx context.Context, plan dashi.ToolPlan) (dashi.ToolDecision, error) {
			owner := brokerOwner{Call: dashi.CallRef{TurnID: plan.Turn.ID, CallID: plan.Call.ID}}
			if plan.Turn.Parent != nil {
				owner.TaskID = delegate.ChildID(plan.Turn.Parent.TurnID, plan.Turn.Parent.CallID)
			}
			return original(context.WithValue(ctx, brokerOwnerKey{}, owner), plan)
		}
	}
	return hooks
}

func promptOwner(ctx context.Context) (brokerOwner, error) {
	if owner, ok := ctx.Value(brokerOwnerKey{}).(brokerOwner); ok {
		return owner, nil
	}
	ref, ok := dashi.CurrentCall(ctx)
	if !ok {
		return brokerOwner{}, errors.New("prompt has no original call identity")
	}
	owner := brokerOwner{Call: ref}
	if turn, ok := dashi.CurrentTurn(ctx); ok && turn.Parent != nil {
		owner.TaskID = delegate.ChildID(turn.Parent.TurnID, turn.Parent.CallID)
	}
	return owner, nil
}

type brokerPrompt struct {
	ID       uint64
	Owner    brokerOwner
	Approval *ApprovalRequest
	Question *Question
	ctx      context.Context
	cancel   context.CancelFunc
	response chan brokerResponse
}
type brokerResponse struct {
	approval Approval
	answer   workspace.Answer
}

// approvalBroker serializes questions and approvals without restoring a
// guessed controller mode. Its UI adapter derives mode from actual owners.
// Cancellation dismisses this exact ID; an answer cannot spill to the next ID.
type approvalBroker struct {
	gate    chan struct{}
	mu      sync.Mutex
	next    uint64
	active  *brokerPrompt
	present func(brokerPrompt)
	dismiss func(uint64)
}

func newApprovalBroker(present func(brokerPrompt), dismiss func(uint64)) *approvalBroker {
	b := &approvalBroker{gate: make(chan struct{}, 1), present: present, dismiss: dismiss}
	b.gate <- struct{}{}
	return b
}
func (b *approvalBroker) run(ctx context.Context, prompt brokerPrompt) (brokerResponse, error) {
	select {
	case <-ctx.Done():
		return brokerResponse{}, ctx.Err()
	case <-b.gate:
	}
	defer func() { b.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return brokerResponse{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.mu.Lock()
	b.next++
	prompt.ID = b.next
	prompt.ctx = ctx
	prompt.cancel = cancel
	prompt.response = make(chan brokerResponse, 1)
	b.active = &prompt
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.active == &prompt {
			b.active = nil
		}
		b.mu.Unlock()
		if b.dismiss != nil {
			b.dismiss(prompt.ID)
		}
	}()
	b.present(prompt)
	select {
	case <-ctx.Done():
		return brokerResponse{}, ctx.Err()
	case response := <-prompt.response:
		if err := ctx.Err(); err != nil {
			return brokerResponse{}, err
		}
		return response, nil
	}
}
func (b *approvalBroker) respond(id uint64, owner brokerOwner, response brokerResponse) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.active
	if p == nil || p.ID != id || p.Owner != owner || p.ctx.Err() != nil {
		return false
	}
	select {
	case p.response <- response:
		b.active = nil
		return true
	default:
		return false
	}
}
func (b *approvalBroker) approve(ctx context.Context, request ApprovalRequest) (Approval, error) {
	owner, err := promptOwner(ctx)
	if err != nil {
		return Approval{}, err
	}
	request.Args = slices.Clone(request.Args)
	response, err := b.run(ctx, brokerPrompt{Owner: owner, Approval: &request})
	return response.approval, err
}
func (b *approvalBroker) ask(ctx context.Context, question Question) (workspace.Answer, error) {
	owner, err := promptOwner(ctx)
	if err != nil {
		return workspace.Answer{}, err
	}
	question.Choices = slices.Clone(question.Choices)
	response, err := b.run(ctx, brokerPrompt{Owner: owner, Question: &question})
	return response.answer, err
}

func (b *approvalBroker) cancelActive() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != nil {
		b.active.cancel()
	}
}

func (b *approvalBroker) cancelRequest(id uint64, owner brokerOwner) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != nil && b.active.ID == id && b.active.Owner == owner {
		b.active.cancel()
	}
}

// presentOnly reserves the same dialog owner for a native details viewport.
// A child prompt waits until this viewport closes; it cannot replace the draft.
func (b *approvalBroker) presentOnly(ctx context.Context, fn func()) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.gate:
	}
	defer func() { b.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	fn()
	return ctx.Err()
}
