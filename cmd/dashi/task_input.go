package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/content/mention"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
)

// taskInputs owns child-only stores for one leased parent. Tasks.Close must
// finish before close releases these pinned descriptors.
type taskInputs struct {
	loader     dashi.Loader
	parent     *session.AttachmentStore
	parentBind func() (*session.AttachmentStore, error)
	clientFor  func(context.Context, dashi.Agent) (llm.Client, error)
	mu         sync.Mutex
	stores     map[string]*session.AttachmentStore
}

func (h *taskInputs) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, store := range h.stores {
		store.Close()
		delete(h.stores, id)
	}
}

func (h *taskInputs) childStore(ref dashi.CallRef, create bool) (*session.AttachmentStore, error) {
	parent, err := h.parentStore()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	id := delegate.ChildID(ref.TurnID, ref.CallID)
	if store := h.stores[id]; store != nil {
		return store, nil
	}
	store, err := parent.ChildAttachments(ref, create)
	if err != nil {
		return nil, err
	}
	if h.stores == nil {
		h.stores = map[string]*session.AttachmentStore{}
	}
	h.stores[id] = store
	return store, nil
}

// originatingTask replays only through the original call start. Later context
// changes and live workspace files cannot enter this child's initial evidence.
func (h *taskInputs) originatingTask(ctx context.Context, ref dashi.CallRef) (dashi.Turn, dashi.TurnView, error) {
	if h.loader == nil {
		return dashi.Turn{}, dashi.TurnView{}, fmt.Errorf("task requires a recorder with authoritative loading")
	}
	turn, err := h.loader.Load(ctx, ref.TurnID)
	if err != nil {
		return turn, dashi.TurnView{}, err
	}
	if turn.ID != ref.TurnID {
		return turn, dashi.TurnView{}, fmt.Errorf("task parent identity differs")
	}
	for i, step := range turn.Steps {
		if step.Kind == dashi.StepToolStarted && step.Tool != nil && step.Tool.Call.ID == ref.CallID {
			if step.Tool.Call.Name != delegate.ToolName {
				return turn, dashi.TurnView{}, fmt.Errorf("task parent call is not a task")
			}
			turn.Steps = turn.Steps[: i+1 : i+1]
			view, err := dashi.Replay(turn)
			return turn, view, err
		}
	}
	return turn, dashi.TurnView{}, fmt.Errorf("task parent call has no recorded start")
}

func (h *taskInputs) prepareStart(ctx context.Context, ref dashi.CallRef, start dashi.Start) (dashi.Start, error) {
	if start.Parent == nil || *start.Parent != ref || start.CommandID != delegate.ChildID(ref.TurnID, ref.CallID)+"/start" {
		return start, fmt.Errorf("task Start parent identity differs")
	}
	turn, view, err := h.originatingTask(ctx, ref)
	if err != nil {
		return start, err
	}
	if start.Agent.Provider != view.Agent.Provider || start.Agent.API != view.Agent.API || start.Agent.Model != view.Agent.Model {
		return start, fmt.Errorf("task inherited destination differs")
	}
	var args struct {
		Prompt string `json:"prompt"`
	}
	call := turn.Steps[len(turn.Steps)-1].Tool.Call
	if err := json.Unmarshal(call.Args, &args); err != nil || len(start.Input) != 1 || start.Input[0].Text() != args.Prompt {
		return start, fmt.Errorf("task prompt differs from original call")
	}
	// Resolve trust and recorded fingerprint before any content publication.
	if _, err := h.clientFor(taskBindingContext(ctx, ref), view.Agent); err != nil {
		return start, err
	}
	raw, err := json.Marshal(view.Agent)
	if err != nil {
		return start, err
	}
	var owned dashi.Agent
	if err := json.Unmarshal(raw, &owned); err != nil {
		return start, err
	}
	start.Agent.Settings = owned.Settings
	start.Agent.NoInheritedSettings = true
	start.Agent.ActiveCompaction = owned.ActiveCompaction
	start.Agent.Revision = owned.Revision
	store, err := h.childStore(ref, true)
	if err != nil {
		return start, err
	}
	var wire int64
	for _, message := range turn.Steps[0].Started.Input {
		copied := message.Clone()
		copied.Content = nil
		for _, part := range message.Content {
			if part.Block == nil {
				continue
			}
			block := part.Block.Clone()
			var source *content.Source
			name := "parent-evidence"
			if block.Image != nil {
				source = &block.Image.Source
			}
			if block.Document != nil {
				source = &block.Document.Source
				name = block.Document.Title
				if name == "" {
					name = "parent-evidence"
				}
			}
			if source == nil || source.Ref == nil {
				continue
			}
			if source.Origin != (content.Origin{}) {
				return start, fmt.Errorf("task evidence has provider-bound origin")
			}
			size, err := mention.WireSize(source.Ref.Size)
			if err != nil || size > 32<<20-wire {
				return start, fmt.Errorf("task evidence exceeds message limit")
			}
			wire += size
			parent, err := h.parentStore()
			if err != nil {
				return start, err
			}
			data, err := parent.Resolve(ctx, ref.TurnID, *source.Ref)
			if err != nil {
				return start, err
			}
			digest := sha256.Sum256(data)
			if int64(len(data)) != source.Ref.Size || hex.EncodeToString(digest[:]) != source.Ref.SHA256 {
				return start, fmt.Errorf("task parent evidence integrity differs")
			}
			key, _ := json.Marshal(source.Ref)
			identity := sha256.Sum256(key)
			childRef, err := store.Publish(ctx, "att_"+hex.EncodeToString(identity[:16]), name, source.MIMEType, data)
			if err != nil {
				return start, err
			}
			*source = content.Source{MIMEType: source.MIMEType, Ref: &childRef}
			copied.Content = append(copied.Content, llm.Block(block))
		}
		if len(copied.Content) > 0 {
			start.Input = append(start.Input, copied)
		}
	}
	for _, name := range []string{"workspace-instructions-source", "workspace-instructions", "skills"} {
		item, ok := view.Context(dashi.ContextRuntime, name)
		if !ok {
			continue
		}
		message := item.Message.Clone()
		message.Context = &llm.MessageContext{Origin: string(item.Origin), Name: item.Name}
		start.Input = append(start.Input, message)
	}
	return start, nil
}

func (h *taskInputs) bindServices(ctx context.Context, ref dashi.CallRef, agent dashi.Agent) (delegate.ChildServices, error) {
	// This immutable client never follows foreground turnClient.bind.
	client, err := h.clientFor(taskBindingContext(ctx, ref), agent)
	if err != nil {
		return delegate.ChildServices{}, err
	}
	if _, mutable := client.(*turnClient); mutable {
		return delegate.ChildServices{}, fmt.Errorf("task client must be immutable")
	}
	store, err := h.childStore(ref, false)
	if err != nil {
		return delegate.ChildServices{}, err
	}
	return delegate.ChildServices{Client: client, ContentResolver: store}, nil
}

func taskBindingContext(ctx context.Context, ref dashi.CallRef) context.Context {
	return context.WithValue(ctx, brokerOwnerKey{}, brokerOwner{TaskID: delegate.ChildID(ref.TurnID, ref.CallID), Call: ref})
}

func (h *taskInputs) parentStore() (*session.AttachmentStore, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.parent != nil {
		return h.parent, nil
	}
	if h.parentBind == nil {
		return nil, fmt.Errorf("task parent evidence store unavailable")
	}
	store, err := h.parentBind()
	if err == nil {
		h.parent = store
	}
	return store, err
}
