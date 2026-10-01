package dashi

import (
	"fmt"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/tooldef"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (r *runtime) run() Outcome {
	for {
		s, e := replay(r.turn)
		if e != nil {
			return r.stop(StopFailed, e.Error())
		}
		if s.Stop.Terminal {
			return r.current()
		}
		r.startSteering(s)
		if s.Interruptions > max(r.agent.Limits.Interruptions, 0) {
			return r.stop(StopLimited, "interruptions")
		}
		if r.ctx.Err() != nil {
			return r.contextStop()
		}
		if s.modelIntent != "" {
			if o := r.recoverModelIntent(s); o != nil {
				return *o
			}
			continue
		}
		if s.unresolvedModel != "" {
			return r.stop(StopResultUnknown, "model attempt needs reconciliation")
		}
		if s.pendingCompaction != "" {
			if o := r.applyActiveCompaction(s); o != nil {
				return *o
			}
			continue
		}
		if stop := budgetOvershotStop(s); stop != nil {
			return r.stopWith(stop)
		}
		if len(s.callOrder) > 0 && hasOpenTools(s) {
			if o, done := r.tools(s); done {
				return o
			}
			continue
		}
		if len(s.hosted) > 0 {
			return r.stop(StopWaiting, "provider approval pending")
		}
		if s.Output != nil {
			return r.stopAfterAnswer(s)
		}
		if submitted := submittedOutput(s); submitted != nil {
			if o := r.commit(Step{
				Kind:   StepOutputRecorded,
				Output: submitted,
			}); o != nil {
				return *o
			}
			return r.stopAfterAnswer(s)
		}
		if s.awaitingClassification && s.latestResult != nil {
			o, again := r.classify(*s.latestResult)
			if !again {
				return o
			}
			continue
		}
		if s.failedModel != "" {
			if lastModelPurpose(r.turn) == ModelPurposeCompaction {
				return r.stop(StopFailed, StopReasonCompactionFailed)
			}
			return r.stop(StopFailed, s.failedModel)
		}
		if stop := budgetSpentStop(s); stop != nil {
			return r.stopWith(stop)
		}
		if s.attemptCap <= s.ModelAttempts {
			return r.stop(StopLimited, "model_attempts")
		}
		if !r.agent.Limits.Deadline.IsZero() &&
			!time.Now().Before(r.agent.Limits.Deadline) {
			return r.stop(StopLimited, "deadline")
		}
		if r.pauseRequested() {
			return r.pauseStop()
		}
		o, again := r.model(s)
		if !again {
			return o
		}
	}
}

// selectTools keeps the tools the agent's Tools names, in the toolset's
// order; an empty selection keeps every tool. A named tool the toolset does
// not offer warns once per turn, on the next committed step.
func (r *runtime) selectTools(s *replayState, tools []tool.Tool) []tool.Tool {
	names := r.agent.Tools
	if len(names) == 0 {
		return tools
	}
	selected := make([]tool.Tool, 0, len(names))
	for _, t := range tools {
		if slices.Contains(names, t.Definition.Name) {
			selected = append(selected, t)
		}
	}
	for _, name := range names {
		if slices.ContainsFunc(tools, func(t tool.Tool) bool {
			return t.Definition.Name == name
		}) {
			continue
		}
		w := Warning{
			Code: WarningToolNotOffered,
			Message: fmt.Sprintf(
				"Agent.Tools names %q, which the runner's tools do not offer",
				name,
			),
		}
		if slices.Contains(s.Warnings, w) ||
			slices.Contains(r.pendingWarnings, w) {
			continue
		}
		r.pendingWarnings = append(r.pendingWarnings, w)
	}
	return selected
}

func (r *runtime) recoverModelIntent(s *replayState) *Outcome {
	unsafe := false
	for _, def := range s.modelDefs {
		if def.Kind == tool.KindProviderHosted && !tooldef.SafeRepeat(def) {
			unsafe = true
			break
		}
	}
	fact := &ModelFact{
		AttemptID:   s.modelIntent,
		MessageID:   s.modelMessages[s.modelIntent],
		Error:       "worker stopped before model result",
		Interrupted: InterruptWorker,
		Uncertain:   unsafe,
	}
	setSettlementCharge(s.modelIntents[s.modelIntent], fact)
	if o := r.commit(Step{Kind: StepModelCallFinished, Model: fact}); o != nil {
		return o
	}
	if unsafe {
		o := r.stop(StopResultUnknown, "provider-hosted tool may have run")
		return &o
	}
	return nil
}

func hasOpenTools(s *replayState) bool {
	for _, id := range s.callOrder {
		if s.Tools[id].Status != ToolFinished {
			return true
		}
	}
	return false
}
