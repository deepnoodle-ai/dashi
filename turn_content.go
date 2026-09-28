package dashi

import (
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Citations returns the citations on assistant text across every settled
// model call in the turn, in the order the provider reported them. A turn
// that used hosted tools may cite sources in several calls, so the final
// message alone can miss some. Citations are not deduplicated: one source
// may be cited twice with different excerpts.
func (t Turn) Citations() []content.Citation {
	var out []content.Citation
	for _, result := range settledResults(t.Steps) {
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

// HostedUses returns the provider-run tool calls across every settled model
// call in the turn, in order. Some providers report a hosted call's result
// as a separate item; HostedUses copies that result's output and error flag
// onto its use, so each entry is one call with its result either way.
func (t Turn) HostedUses() []llm.HostedUse {
	var out []llm.HostedUse
	index := map[string]int{}
	for _, result := range settledResults(t.Steps) {
		for _, part := range result.Message.Content {
			switch {
			case part.Kind == llm.ContentHostedUse && part.HostedUse != nil:
				index[part.HostedUse.ID] = len(out)
				out = append(out, part.HostedUse.Clone())
			case part.Kind == llm.ContentHostedResult &&
				part.HostedResult != nil:
				i, ok := index[part.HostedResult.CallID]
				if !ok {
					continue
				}
				r := part.HostedResult.Clone()
				out[i].Output = append(out[i].Output, r.Output...)
				out[i].IsError = out[i].IsError || r.IsError
			}
		}
	}
	return out
}

// settledResults returns the actionable assistant results in step order:
// settled or reconciled calls that neither failed nor were interrupted.
func settledResults(steps []Step) []*llm.Result {
	var out []*llm.Result
	for _, step := range steps {
		if (step.Kind != StepModelSettled &&
			step.Kind != StepModelReconciled) ||
			step.Model == nil ||
			step.Model.Result == nil ||
			step.Model.Error != "" ||
			step.Model.Interrupted != InterruptNone {
			continue
		}
		out = append(out, step.Model.Result)
	}
	return out
}

// answerText returns the text of the latest assistant answer. Messages the
// provider paused and then continued are one answer, so their content is
// joined with the final message's.
func answerText(steps []Step) string {
	results := settledResults(steps)
	if len(results) == 0 {
		return ""
	}
	first := len(results) - 1
	for first > 0 && results[first-1].Stop.Kind == llm.StopPause {
		first--
	}
	var parts []llm.Content
	for _, result := range results[first:] {
		parts = append(parts, result.Message.Content...)
	}
	return joinText(parts)
}

// joinText concatenates text blocks. Adjacent blocks are joined as they are,
// because providers split one cited sentence into several blocks. Any other
// item between two text blocks (a hosted tool use, a tool call, reasoning)
// ends a passage, so the next text starts after a blank line.
func joinText(parts []llm.Content) string {
	var out strings.Builder
	gap := false
	for _, part := range parts {
		if part.Kind != llm.ContentBlock ||
			part.Block == nil ||
			part.Block.Text == nil {
			gap = true
			continue
		}
		if gap && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n\n")
		}
		gap = false
		out.WriteString(part.Block.Text.Text)
	}
	return out.String()
}
