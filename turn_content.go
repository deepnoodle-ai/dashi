package dashi

import (
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/llmtext"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Citations returns the citations on assistant text across every settled
// model call in the turn, in the order the provider reported them. A turn
// that used hosted tools may cite sources in several calls, so the final
// message alone can miss some. Citations are not deduplicated: one source
// may be cited twice with different excerpts.
func (t Turn) Citations() []content.Citation {
	var out []content.Citation
	for _, result := range finishedResults(t.Steps) {
		for _, part := range result.Message.Content {
			if part.Kind != llm.ContentBlock ||
				part.Block == nil ||
				part.Block.Text == nil {
				continue
			}
			for _, citation := range part.Block.Text.Citations {
				citation.Raw = append([]byte(nil), citation.Raw...)
				out = append(out, citation)
			}
		}
	}
	return out
}

// ProviderToolUses returns the provider-run tool calls across every settled model
// call in the turn, in order. Some providers report a hosted call's result
// as a separate item; ProviderToolUses copies that result's output and error flag
// onto its use, so each entry is one call with its result either way.
func (t Turn) ProviderToolUses() []llm.ProviderToolUse {
	var out []llm.ProviderToolUse
	index := map[string]int{}
	for _, result := range finishedResults(t.Steps) {
		for _, part := range result.Message.Content {
			switch {
			case part.Kind == llm.ContentProviderToolUse && part.ProviderToolUse != nil:
				index[part.ProviderToolUse.ID] = len(out)
				out = append(out, part.ProviderToolUse.Clone())
			case part.Kind == llm.ContentProviderToolResult &&
				part.ProviderToolResult != nil:
				i, ok := index[part.ProviderToolResult.CallID]
				if !ok {
					continue
				}
				r := part.ProviderToolResult.Clone()
				out[i].Output = append(out[i].Output, r.Output...)
				out[i].IsError = out[i].IsError || r.IsError
			}
		}
	}
	return out
}

// settledResults returns the actionable assistant results in step order:
// settled or reconciled calls that neither failed nor were interrupted.
func finishedResults(steps []Step) []*llm.Result {
	var out []*llm.Result
	turn := Turn{Steps: steps}
	for _, step := range steps {
		if step.Model != nil && modelPurpose(turn, step.Model.ModelCallID) == ModelPurposeCompaction {
			continue
		}
		if result := actionableResult(step); result != nil {
			out = append(out, result)
		}
	}
	return out
}

// actionableResult returns the step's assistant result when the step
// settles or reconciles a call that neither failed nor was interrupted.
func actionableResult(step Step) *llm.Result {
	if (step.Kind != StepModelCallFinished &&
		step.Kind != StepModelCallFinishedByHost) ||
		step.Model == nil ||
		step.Model.Purpose == ModelPurposeCompaction ||
		step.Model.Result == nil ||
		step.Model.Error != "" ||
		step.Model.Interrupted != InterruptNone {
		return nil
	}
	return step.Model.Result
}

// answerText returns the text of the latest assistant answer. Messages the
// provider paused and then continued are one answer, so their content is
// joined with the final message's. A call an urgent steer cut short ends
// the chain: the model wrote what follows after reading the steer, so text
// from before the cut is not part of that answer.
func answerText(steps []Step) string {
	var chain []*llm.Result
	turn := Turn{Steps: steps}
	for _, step := range steps {
		if step.Model != nil && modelPurpose(turn, step.Model.ModelCallID) == ModelPurposeCompaction {
			continue
		}
		if step.Kind == StepModelCallFinished && step.Model != nil &&
			step.Model.Interrupted == InterruptSteering {
			chain = nil
			continue
		}
		result := actionableResult(step)
		if result == nil {
			continue
		}
		if n := len(chain); n > 0 && chain[n-1].Stop.Kind != llm.StopPause {
			chain = nil
		}
		chain = append(chain, result)
	}
	var parts []llm.Content
	for _, result := range chain {
		parts = append(parts, result.Message.Content...)
	}
	return llmtext.Join(parts)
}
