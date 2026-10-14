package dashi

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/internal/llmtext"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Each validation call owns this cache. Covered prefixes are immutable, and
// no cache outlives the rebuild of the turn's state or shares mutable data
// with a public caller.
type contextValidationCache struct {
	prefixes map[int64][]contextMessage
	states   map[int64]*replayState
	needed   map[int64]bool
}

type contextMessage struct {
	Ref     TurnMessageRef
	Message llm.Message
}

const activeSummaryFrame = compactiontext.ActiveFrame

func modelPurpose(t Turn, modelCall string) ModelPurpose {
	for _, step := range t.Steps {
		if step.Kind == StepModelCallStarted && step.Model.ModelCallID == modelCall {
			return step.Model.Purpose
		}
	}
	return ""
}
func compactionPlan(t Turn, modelCall string) *ContextCompactionPlan {
	for _, step := range t.Steps {
		if step.Kind == StepModelCallStarted && step.Model.ModelCallID == modelCall {
			return step.Model.Compaction
		}
	}
	return nil
}
func replaceContext(messages []contextMessage, plan *ContextCompactionPlan, fact ContextCompactionFact, seq int64) []contextMessage {
	covered := map[TurnMessageRef]bool{}
	for _, ref := range plan.Covered {
		covered[ref] = true
	}
	out := make([]contextMessage, 0, len(messages))
	inserted := false
	for _, m := range messages {
		if covered[m.Ref] {
			if !inserted {
				// A fallback is not a summary, so it has no summary frame.
				text := fact.SummaryFallback
				if fact.Summary != "" {
					text = activeSummaryFrame + "\n\n" + fact.Summary
				}
				out = append(out, contextMessage{Ref: TurnMessageRef{Seq: seq}, Message: llm.UserMessage(llm.Text(text))})
				inserted = true
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

type contextGroup struct {
	Messages []contextMessage
	Eligible bool
}

// A model message and all of its paired results have one source sequence.
// Provider pauses form a chain through their closing result. Added user items
// remain independent protected messages even when interleaved in that chain.
func contextGroups(t Turn, messages []contextMessage) []contextGroup {
	groupSeq := map[int64]int64{}
	eligible := map[int64]bool{}
	var chain int64
	for _, step := range t.Steps {
		switch step.Kind {
		case StepContextCompacted:
			groupSeq[step.Seq] = step.Seq
			eligible[step.Seq] = true
		case StepModelCallFinished, StepModelCallFinishedByHost:
			f := step.Model
			if modelPurpose(t, f.ModelCallID) == ModelPurposeCompaction || f.Result == nil || f.Error != "" || f.Interrupted != "" {
				continue
			}
			if chain == 0 {
				chain = step.Seq
			}
			groupSeq[step.Seq] = chain
			safe := f.Result.Stop.Kind == llm.StopFinished || f.Result.Stop.Kind == llm.StopToolUse
			for _, c := range f.Result.Message.Content {
				if c.Kind == llm.ContentProviderToolApprovalRequest {
					safe = false
				}
			}
			if f.Result.Stop.Kind != llm.StopPause {
				eligible[chain] = safe
				chain = 0
			}
		}
	}
	indices := map[int64]int{}
	var groups []contextGroup
	for _, m := range messages {
		id := groupSeq[m.Ref.Seq]
		if id == 0 {
			continue
		}
		index, ok := indices[id]
		if !ok {
			index = len(groups)
			indices[id] = index
			groups = append(groups, contextGroup{Eligible: eligible[id]})
		}
		g := &groups[index]
		g.Messages = append(g.Messages, m)
		if m.Message.Role == llm.RoleSystem || m.Message.Role == llm.RoleDeveloper || m.Message.Context != nil {
			g.Eligible = false
		}
	}
	return groups
}
func coveredMessages(t Turn, plan *ContextCompactionPlan) ([]llm.Message, error) {
	return coveredMessagesCached(t, plan, &contextValidationCache{prefixes: map[int64][]contextMessage{}})
}

func coveredMessagesCached(t Turn, plan *ContextCompactionPlan, cache *contextValidationCache) ([]llm.Message, error) {
	if plan == nil || plan.Through < 1 || plan.Through > int64(len(t.Steps)) || plan.Window < 1024 || plan.EstimatedBefore < 0 || len(plan.Covered) == 0 {
		return nil, errors.New("invalid context compaction plan")
	}
	prefix := t
	prefix.Steps = t.Steps[:plan.Through]
	visible, ok := cache.prefixes[plan.Through]
	if !ok {
		state := cache.states[plan.Through]
		var err error
		if state == nil {
			state, err = replayCached(prefix, cache)
			if err != nil {
				return nil, err
			}
		}
		visible, err = turnMessagesState(prefix, nil, true, state, cache)
		if err != nil {
			return nil, err
		}
		cache.prefixes[plan.Through] = visible
	}
	selected := map[TurnMessageRef]bool{}
	for _, ref := range plan.Covered {
		if ref.Seq <= 0 || ref.Index < 0 || selected[ref] {
			return nil, errors.New("invalid or repeated compaction coverage")
		}
		selected[ref] = true
	}
	permitted := map[TurnMessageRef]bool{}
	for _, g := range contextGroups(prefix, visible) {
		count := 0
		for _, m := range g.Messages {
			if selected[m.Ref] {
				count++
			}
		}
		if count > 0 {
			if !g.Eligible || count != len(g.Messages) {
				return nil, errors.New("compaction splits or covers a protected group")
			}
			for _, m := range g.Messages {
				permitted[m.Ref] = true
			}
		}
	}
	var actual []TurnMessageRef
	var messages []llm.Message
	for _, m := range visible {
		if selected[m.Ref] {
			if !permitted[m.Ref] {
				return nil, errors.New("compaction covers protected context")
			}
			actual = append(actual, m.Ref)
			messages = append(messages, m.Message)
		}
	}
	if !slices.Equal(actual, plan.Covered) {
		return nil, errors.New("compaction coverage differs from visible source order")
	}
	hash, err := historyHash(messages)
	if err != nil {
		return nil, err
	}
	if hash != plan.Hash {
		return nil, errors.New("compaction coverage hash mismatch")
	}
	return messages, nil
}
func (s *replayState) validateCompactionCallStart(step Step) error {
	f := step.Model
	if f.Purpose == "" {
		if f.Compaction != nil {
			return errors.New("ordinary call start has compaction plan")
		}
		if s.pendingCompaction != "" {
			return errors.New("summary must be applied before ordinary dispatch")
		}
		return nil
	}
	if f.Purpose != ModelPurposeCompaction || f.Compaction == nil || f.Compaction.Through >= step.Seq || f.Compaction.Window != s.ActiveCompactionWindow || s.Agent.ActiveCompaction == nil || s.Agent.ActiveCompaction.Disabled || s.Origin == StartCompaction || len(f.Tools) > 0 || f.FinalReplyOnly || s.modelCallCap-s.ModelCalls < 2 || s.pendingCompaction != "" {
		return errors.New("invalid summary call start")
	}
	_, err := coveredMessagesCached(s.source, f.Compaction, s.contextCache)
	return err
}
func (s *replayState) validateCompactionFinish(f *ModelFact) error {
	callStart := s.startedModelCalls[f.ModelCallID]
	if callStart == nil {
		return errors.New("missing model call start")
	}
	if f.Purpose != "" && f.Purpose != callStart.Purpose {
		return errors.New("model call result changed purpose")
	}
	if f.Compaction != nil {
		return errors.New("a call's result cannot change the summary plan")
	}
	if callStart.Purpose == ModelPurposeCompaction && f.Error == "" && f.Interrupted == "" {
		if f.Result == nil {
			return errors.New("summary lacks result")
		}
		if err := validateModelResult(*f.Result, s.Agent); err != nil {
			return err
		}
		if _, err := validActiveSummary(f.Result); err != nil {
			return err
		}
	}
	return nil
}
func validActiveSummary(result *llm.Result) (string, error) {
	if result == nil || result.Stop.Kind != llm.StopFinished {
		return "", errors.New("summary did not finish")
	}
	if activeSummaryHasTools(result) {
		return "", errors.New("summary requested tool work")
	}
	text := activeSummaryText(result)
	if text == "" {
		return "", errors.New("summary is empty")
	}
	return text, nil
}

func activeSummaryText(result *llm.Result) string {
	return compactiontext.Summary(llmtext.Join(result.Message.Content))
}

func activeSummaryHasTools(result *llm.Result) bool {
	for _, c := range result.Message.Content {
		if c.Kind == llm.ContentToolCall || c.Kind == llm.ContentProviderToolUse || c.Kind == llm.ContentProviderToolResult || c.Kind == llm.ContentProviderToolApprovalRequest || c.Kind == llm.ContentProviderToolApprovalResponse {
			return true
		}
	}
	return false
}
func (s *replayState) applyContextCompaction(step Step) error {
	f := step.Compacted
	if f == nil || f.ModelCallID == "" || s.Stop.Ended {
		return errors.New("context replacement has no summary call")
	}
	callStart := s.startedModelCalls[f.ModelCallID]
	finished := s.modelFinishes[f.ModelCallID]
	if callStart == nil || callStart.Purpose != ModelPurposeCompaction || callStart.Compaction == nil || finished == nil || finished.Interrupted != "" {
		return errors.New("context replacement has no finished summary call")
	}
	switch {
	case f.Summary != "" && f.SummaryFallback == "":
		if f.ModelCallID != s.pendingCompaction || finished.Error != "" {
			return errors.New("context replacement has no unapplied summary")
		}
		text, err := validActiveSummary(finished.Result)
		if err != nil {
			return err
		}
		if text != f.Summary || f.EstimatedAfter < 0 || f.EstimatedAfter >= callStart.Compaction.EstimatedBefore {
			return errors.New("context replacement lacks matching summary or savings")
		}
	case f.Summary == "" && strings.HasPrefix(f.SummaryFallback, compactiontext.FallbackNote):
		// A fallback stands in for a failed summary call, or for a finished
		// summary that a resumed run could not verify.
		if f.ModelCallID != s.pendingCompaction && f.ModelCallID != s.failedSummary || f.EstimatedAfter < 0 {
			return errors.New("summary fallback has no failed or unapplied summary")
		}
	default:
		return errors.New("context replacement needs a summary or a summary fallback")
	}
	if _, err := coveredMessagesCached(s.source, callStart.Compaction, s.contextCache); err != nil {
		return fmt.Errorf("context replacement: %w", err)
	}
	s.ContextCompactions = append(s.ContextCompactions, *f)
	s.pendingCompaction = ""
	s.failedSummary = ""
	s.failedModel = ""
	s.compactionUsed = true
	return nil
}
