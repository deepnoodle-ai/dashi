package main

import (
	"context"
	"errors"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Binding is lazy: an unavailable attachment platform cannot prevent a
// text-only session from opening. Every bound descriptor lives under its lease.
type engineAttachmentResolver struct {
	mu      sync.Mutex
	store   *sessionstore.Store
	session *sessionstore.Session
	bound   *sessionstore.AttachmentStore
}

func (r *engineAttachmentResolver) bind() (*sessionstore.AttachmentStore, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound == nil {
		var err error
		r.bound, err = r.store.Attachments(r.session)
		if err != nil {
			return nil, err
		}
	}
	return r.bound, nil
}
func (r *engineAttachmentResolver) Load(ctx context.Context, id string, ref content.Ref) ([]byte, error) {
	a, err := r.bind()
	if err != nil {
		return nil, err
	}
	return a.Load(ctx, id, ref)
}
func (r *engineAttachmentResolver) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound != nil {
		r.bound.Close()
		r.bound = nil
	}
}

func (e *Engine) inputReceipt(p *pendingPrompt, state inputReceiptState) {
	if p.input != nil && e.cfg.Receipt != nil {
		e.cfg.Receipt(p.input, inputReceipt{p.input.id, p.input.draftRevision, state, true})
	}
}

// PromptInput admits one owned media request. It never queues it as text or
// steers it into another Turn; unresolved work preserves the incoming draft.
func (e *Engine) PromptInput(ctx context.Context, in *submittedInput) (res Result, err error) {
	if in == nil {
		return Result{}, errors.New("attachment input is missing")
	}
	fail := func(err error) (Result, error) {
		if e.cfg.Receipt != nil {
			e.cfg.Receipt(in, inputReceipt{in.id, in.draftRevision, inputPreRecordFailure, true})
		}
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return fail(err)
	}
	if !e.cfg.MCP.Ready() {
		return fail(errors.New("MCP startup has not completed; no work was started"))
	}
	e.inputMu.Lock()
	if e.pending != nil {
		same := e.pending.input == in
		e.inputMu.Unlock()
		if same {
			return e.resumePending(ctx)
		}
		return fail(errors.New("another prompt is pending; continue it before retrying these attachments"))
	}
	if err := e.flushLaterLocked(); err != nil {
		e.inputMu.Unlock()
		return fail(err)
	}
	e.mu.Lock()
	resolver := e.attachmentResolver
	e.mu.Unlock()
	store, err := resolver.bind()
	if err == nil {
		err = in.publish(ctx, store)
	}
	if err == nil {
		e.mu.Lock()
		e.sess.Model = e.model
		if e.cfg.Preferences != nil {
			e.sess.NextTurn = e.cfg.Preferences.metadata()
		}
		err = store.SaveSessionHeader(ctx, e.sess)
		e.mu.Unlock()
	}
	if err != nil {
		e.inputMu.Unlock()
		return fail(err)
	}
	message, err := in.messageForStart()
	if err != nil {
		e.inputMu.Unlock()
		return fail(err)
	}
	e.mu.Lock()
	start := dashi.Start{Input: []llm.Message{message}, CommandID: dashi.NewID(), Agent: e.agent()}
	e.mu.Unlock()
	if _, err := e.handle().Open(ctx); errors.Is(err, filerecorder.ErrCorrupt) {
		start.CommandID = ""
	}
	e.pending = &pendingPrompt{start: start, input: in}
	e.inputMu.Unlock()
	return e.runPending(ctx)
}
