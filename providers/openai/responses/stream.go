// Output item tracking is adapted from Dive's OpenAI stream iterator.
// Completed
// responses use the same decoder as blocking calls, including opaque items.

package responses

import (
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/internal/sse"
	"github.com/deepnoodle-ai/dashi/llm"
)

// readStream accumulates Responses stream events and returns the provider's
// final response when a terminal event arrives. On failure it returns whatever
// was received so far with an incomplete stop kind.
func (wire codec) readStream(
	body io.ReadCloser,
	result llm.Result,
	sink llm.Sink,
) (final llm.Result, failure error) {
	defer func() {
		if failure != nil {
			final.Stop.Kind = llm.StopIncomplete
		}
	}()
	reader := sse.NewServerSentEventsReader[json.RawMessage](body)
	items := map[int]map[string]any{}
	parts := map[int]map[int]map[string]any{}
	summaries := map[int]map[int]string{}
	var envelope map[string]any

	// build reassembles the accumulated items into a response object and
	// decodes it. It runs only when the stream ends without a terminal event,
	// so the response is always marked incomplete.
	build := func() llm.Result {
		if envelope == nil && len(items) == 0 {
			return result
		}
		if envelope == nil {
			envelope = map[string]any{"object": "response", "status": "incomplete"}
		}
		output := make([]any, 0, len(items))
		for _, i := range slices.Sorted(maps.Keys(items)) {
			item := items[i]
			if ps := summaries[i]; len(ps) > 0 {
				values := make([]any, 0, len(ps))
				for _, j := range slices.Sorted(maps.Keys(ps)) {
					values = append(
						values,
						map[string]any{
							"type": "summary_text",
							"text": ps[j],
						},
					)
				}
				item["summary"] = values
			}
			if ps := parts[i]; len(ps) > 0 {
				cs := make([]any, 0, len(ps))
				for _, j := range slices.Sorted(maps.Keys(ps)) {
					cs = append(cs, ps[j])
				}
				item["content"] = cs
			}
			output = append(output, item)
		}
		envelope["output"] = output
		envelope["status"] = "incomplete"
		raw, _ := json.Marshal(envelope)
		decoded, _ := wire.decode(raw, result.Requested)
		return decoded
	}

	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var e struct {
			Type         string
			OutputIndex  int `json:"output_index"`
			ContentIndex int `json:"content_index"`
			SummaryIndex int `json:"summary_index"`
			Delta        string
			Response     json.RawMessage
			Item         json.RawMessage
			Part         json.RawMessage
			Arguments    string
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return build(), providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"decode stream event: %w",
				err,
			)
		}
		switch e.Type {
		case "response.created", "response.in_progress":
			if err := providerhttp.Decode(e.Response, &envelope); err != nil {
				return build(), err
			}
		case "response.output_item.added", "response.output_item.done":
			var item map[string]any
			if err := providerhttp.Decode(e.Item, &item); err != nil || item == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"invalid output item",
				)
			}
			items[e.OutputIndex] = item
			// A finished item carries its complete content and summary, which
			// supersede the parts accumulated from deltas.
			if e.Type == "response.output_item.done" {
				delete(parts, e.OutputIndex)
				delete(summaries, e.OutputIndex)
			}
		case "response.content_part.added", "response.content_part.done":
			var p map[string]any
			if err := providerhttp.Decode(
				e.Part,
				&p,
			); err != nil || p == nil || items[e.OutputIndex] == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"invalid content part",
				)
			}
			if parts[e.OutputIndex] == nil {
				parts[e.OutputIndex] = map[int]map[string]any{}
			}
			parts[e.OutputIndex][e.ContentIndex] = p
		case "response.output_text.delta", "response.refusal.delta":
			if items[e.OutputIndex] == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"text for missing item",
				)
			}
			if parts[e.OutputIndex] == nil {
				parts[e.OutputIndex] = map[int]map[string]any{}
			}
			part := parts[e.OutputIndex][e.ContentIndex]
			if part == nil {
				part = map[string]any{"type": "output_text"}
				parts[e.OutputIndex][e.ContentIndex] = part
			}
			key := "text"
			if e.Type == "response.refusal.delta" {
				key = "refusal"
				part["type"] = "refusal"
			}
			appendString(part, key, e.Delta)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind:         llm.DeltaText,
					Index:        e.OutputIndex,
					ContentIndex: e.ContentIndex,
					Text:         e.Delta,
				},
			)
		case "response.function_call_arguments.delta":
			item := items[e.OutputIndex]
			if item == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"arguments for missing output item",
				)
			}
			appendString(item, "arguments", e.Delta)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind:  llm.DeltaToolArguments,
					Index: e.OutputIndex,
					Text:  e.Delta,
				},
			)
		case "response.function_call_arguments.done":
			if item := items[e.OutputIndex]; item != nil {
				item["arguments"] = e.Arguments
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if e.Type == "response.reasoning_text.delta" && !wire.publicReasoningText {
				// Unmodeled reasoning is private. Its final provider artifact,
				// when present, remains opaque replay data.
				continue
			}
			contentIndex := e.SummaryIndex
			if e.Type == "response.reasoning_text.delta" {
				contentIndex = e.ContentIndex
			}
			if items[e.OutputIndex] == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"reasoning for missing item",
				)
			}
			if summaries[e.OutputIndex] == nil {
				summaries[e.OutputIndex] = map[int]string{}
			}
			summaries[e.OutputIndex][contentIndex] += e.Delta
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind:         llm.DeltaReasoning,
					Index:        e.OutputIndex,
					ContentIndex: contentIndex,
					Text:         e.Delta,
				},
			)
		case "response.completed", "response.incomplete", "response.failed":
			return wire.decode(e.Response, result.Requested)
		case "error":
			return build(), providerhttp.Classify(
				&llm.Error{
					Provider: wire.origin.Provider,
					Category: llm.ErrorProtocol,
					Body:     raw,
				},
				providerhttp.OpenAIReason,
				wire.media,
			)
		default:
			// Lifecycle metadata and final snapshots carry no additional deltas.
			// Unknown delta shapes must not disappear silently.
			if strings.HasSuffix(e.Type, ".delta") {
				return build(), wire.unsupported("stream delta %q", e.Type)
			}
		}
	}
	err := reader.Err()
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return build(), providerhttp.ReadFailure(wire.origin.Provider, err)
}

// appendString appends s to the string stored under key in m, treating a
// missing value as empty.
func appendString(m map[string]any, key, s string) {
	old, _ := m[key].(string)
	m[key] = old + s
}
