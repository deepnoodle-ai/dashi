package main

import (
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi"
)

// Purpose belongs to the recorded attempt, not to its preview text. Keep the
// map for late previews as well as live rendering and recorded replay.
type activeContextAttempt struct {
	before       int
	done         bool
	awaitingStop bool
}

func activeContextUnavailable(s dashi.Step, w dashi.Warning) (Block, bool) {
	text := w.Message
	switch w.Code {
	case dashi.WarningActiveCompactionMediaEstimateUnavailable:
		text = "Active-Turn protection unavailable for media. Dashi cannot estimate this request's context size. The provider decides whether it fits; cost limits still apply."
	case dashi.WarningActiveCompactionUnavailable:
	default:
		return Block{}, false
	}
	return Block{ID: "active-protection:" + s.TurnID, Kind: KindCompaction, State: "unavailable", Text: text, Done: true}, true
}

func (r *Renderer) isActiveSummary(id string) bool {
	return r.activeContext[id] != nil
}

func (r *Renderer) isActiveSummaryFact(m *dashi.ModelFact) bool {
	return m != nil && (m.Purpose == dashi.ModelPurposeCompaction || r.isActiveSummary(m.ModelCallID))
}

func (r *Renderer) activeContextIntent(s dashi.Step) []Block {
	if s.Model == nil || s.Model.Purpose != dashi.ModelPurposeCompaction {
		return nil
	}
	if r.activeContext == nil {
		r.activeContext = make(map[string]*activeContextAttempt)
	}
	a := &activeContextAttempt{}
	if s.Model.Compaction != nil {
		a.before = s.Model.Compaction.EstimatedBefore
	}
	r.activeContext[s.Model.ModelCallID] = a
	return []Block{activeContextBlock(s.Model.ModelCallID, StateRunning, "Compacting this Turn's settled work...", false)}
}

func activeContextBlock(id, state, text string, done bool) Block {
	return Block{ID: "active-compaction:" + id, Kind: KindCompaction, State: state, Text: text, Done: done}
}

func (r *Renderer) activeContextSettled(s dashi.Step) []Block {
	m := s.Model
	if m == nil {
		return nil
	}
	a := r.activeContext[m.ModelCallID]
	if a == nil {
		// A standalone settlement can still be identified by its purpose.
		if r.activeContext == nil {
			r.activeContext = make(map[string]*activeContextAttempt)
		}
		a = &activeContextAttempt{}
		r.activeContext[m.ModelCallID] = a
	}
	switch {
	case m.Interrupted != "":
		a.done = true
		a.awaitingStop = true
		return []Block{activeContextBlock(m.ModelCallID, "interrupted", "Active-Turn compaction interrupted; no replacement applied.", true)}
	case m.Error != "":
		a.done = true
		return []Block{activeContextBlock(m.ModelCallID, StateFailed, "Active-Turn summary failed; previous context kept. "+m.Error, true)}
	default:
		return []Block{activeContextBlock(m.ModelCallID, StateRunning, "Active-Turn summary recorded; applying context...", false)}
	}
}

func (r *Renderer) activeContextApplied(s dashi.Step) []Block {
	if s.Compacted == nil {
		return nil
	}
	f := s.Compacted
	text := "Active-Turn context compacted. Full execution record retained."
	if a := r.activeContext[f.ModelCallID]; a != nil {
		a.done = true
		if a.before > 0 {
			text = fmt.Sprintf("Active-Turn context compacted: estimated input %s → %s tokens. Full record retained.", groupDigits(a.before), groupDigits(f.EstimatedAfter))
		}
	}
	return []Block{activeContextBlock(f.ModelCallID, StateOK, text, true)}
}

func (r *Renderer) activeContextStopped(stop dashi.Stop) []Block {
	if stop.Kind == "" {
		return nil
	}
	var ids []string
	for id, a := range r.activeContext {
		if !a.done || a.awaitingStop {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var blocks []Block
	for _, id := range ids {
		r.activeContext[id].done = true
		r.activeContext[id].awaitingStop = false
		state, text := StateFailed, "Active-Turn compaction stopped; context application is unconfirmed. Reopen to inspect the recorded work."
		if stop.Kind == dashi.StopInterrupted {
			state, text = "interrupted", "Active-Turn compaction interrupted; no replacement applied."
		} else if stop.Kind == dashi.StopCanceled {
			text = "Active-Turn compaction canceled."
		}
		blocks = append(blocks, activeContextBlock(id, state, text, true))
	}
	return blocks
}
