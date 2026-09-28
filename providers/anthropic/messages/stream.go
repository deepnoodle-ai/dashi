// Block ordering, incremental JSON, and cumulative usage handling are adapted
// from Dive's llm/stream.go. The final decoder is shared with complete calls.

package messages

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"

	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/internal/sse"
	"github.com/deepnoodle-ai/kernel/llm"
)

// readStream accumulates Messages stream events into the Result a complete
// call would return. Events are checked against the block lifecycle (start,
// deltas, stop), and any violation fails the call. On failure it returns
// whatever was received so far with an incomplete stop kind.
func (wire codec) readStream(body io.ReadCloser, result llm.Result, sink llm.Sink) (final llm.Result, failure error) {
	defer func() {
		if failure != nil {
			final.Stop.Kind = llm.StopIncomplete
		}
	}()
	reader := sse.NewServerSentEventsReader[json.RawMessage](body)
	blocks := map[int]map[string]any{}
	arguments := map[int]string{}
	stopped := map[int]bool{}
	var buildErr error
	var envelope map[string]json.RawMessage
	started := false

	// build reassembles the accumulated blocks into a complete message and
	// decodes it with the complete-call decoder. Tool input that is not yet
	// valid JSON becomes partial opaque content, so an unfinished call can
	// never be executed or replayed.
	build := func() llm.Result {
		if envelope == nil {
			return result
		}
		indices := slices.Sorted(maps.Keys(blocks))
		content := make([]any, 0, len(indices))
		partial := map[int]*llm.Opaque{}
		for position, i := range indices {
			b := blocks[i]
			if a, ok := arguments[i]; ok {
				if json.Valid([]byte(a)) {
					b["input"] = json.RawMessage(a)
				} else {
					unfinished := maps.Clone(b)
					delete(unfinished, "input")
					unfinished["partial_json"] = a
					raw, _ := json.Marshal(unfinished)
					partial[position] = &llm.Opaque{
						Version: 1,
						Origin:  wire.origin,
						Type:    "tool_use",
						Unit:    llm.OpaqueUnitContentBlock,
						Partial: true,
						Raw:     raw,
					}
				}
			}
			content = append(content, b)
		}
		envelope["content"], _ = json.Marshal(content)
		if result.Usage != nil {
			usage, _ := json.Marshal(result.Usage)
			if result.ServiceTier != "" {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(usage, &fields)
				fields["service_tier"], _ = json.Marshal(result.ServiceTier)
				usage, _ = json.Marshal(fields)
			}
			envelope["usage"] = usage
		}
		raw, _ := json.Marshal(envelope)
		decoded, err := wire.decode(raw, result.Requested)
		buildErr = err
		for position, replay := range partial {
			if position < len(decoded.Message.Content) {
				decoded.Message.Content[position] = llm.Content{Kind: llm.ContentOpaque, Opaque: replay}
			}
			content[position] = replay.Raw
		}
		if len(partial) > 0 {
			envelope["content"], _ = json.Marshal(content)
			decoded.Raw, _ = json.Marshal(envelope)
		}
		return decoded
	}

	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var e struct {
			Type         string
			Index        int
			Message      json.RawMessage
			ContentBlock json.RawMessage `json:"content_block"`
			Usage        json.RawMessage
			Delta        struct {
				Type         string
				Text         string
				Thinking     string
				Signature    string
				PartialJSON  string          `json:"partial_json"`
				StopReason   string          `json:"stop_reason"`
				StopSequence json.RawMessage `json:"stop_sequence"`
				Citation     json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode stream event: %w", err)
		}
		switch e.Type {
		case "message_start":
			if started {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "duplicate message start")
			}
			started = true
			if err := json.Unmarshal(e.Message, &envelope); err != nil {
				return result, err
			}
			var err error
			result, err = wire.decode(e.Message, result.Requested)
			if err != nil {
				return result, err
			}
		case "content_block_start":
			if !started {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "content before message start")
			}
			var b map[string]any
			if err := providerhttp.Decode(e.ContentBlock, &b); err != nil || b == nil {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "invalid content block")
			}
			if _, exists := blocks[e.Index]; exists || e.Index < 0 {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "duplicate or negative block index")
			}
			blocks[e.Index] = b
			c, err := wire.decodeContent(e.ContentBlock)
			if err != nil {
				return build(), err
			}
			providerhttp.Emit(sink, llm.Delta{Kind: llm.DeltaContent, Index: e.Index, Content: &c})
		case "content_block_delta":
			b := blocks[e.Index]
			if b == nil || stopped[e.Index] {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "delta for missing block %d", e.Index)
			}
			d := llm.Delta{Index: e.Index}
			switch e.Delta.Type {
			case "text_delta":
				appendString(b, "text", e.Delta.Text)
				d.Kind = "text"
				d.Text = e.Delta.Text
			case "thinking_delta":
				appendString(b, "thinking", e.Delta.Thinking)
				d.Kind = "reasoning"
				d.Text = e.Delta.Thinking
			case "signature_delta":
				// Signatures are accumulated for replay but not emitted.
				appendString(b, "signature", e.Delta.Signature)
			case "input_json_delta":
				arguments[e.Index] += e.Delta.PartialJSON
				d.Kind = "tool_arguments"
				d.Text = e.Delta.PartialJSON
			case "citations_delta":
				var c any
				if err := providerhttp.Decode(e.Delta.Citation, &c); err != nil {
					return build(), err
				}
				cs, _ := b["citations"].([]any)
				b["citations"] = append(cs, c)
			default:
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorUnsupported, "unknown stream delta %q", e.Delta.Type)
			}
			if d.Kind != "" {
				providerhttp.Emit(sink, d)
			}
		case "content_block_stop":
			if blocks[e.Index] == nil || stopped[e.Index] {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "stop for missing or closed block")
			}
			stopped[e.Index] = true
			if a, ok := arguments[e.Index]; ok && !json.Valid([]byte(a)) {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "incomplete tool arguments")
			}
		case "message_delta":
			if envelope == nil {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "message delta before start")
			}
			if e.Delta.StopReason != "" {
				envelope["stop_reason"], _ = json.Marshal(e.Delta.StopReason)
			}
			if len(e.Delta.StopSequence) > 0 {
				envelope["stop_sequence"] = e.Delta.StopSequence
			}
			// Usage in message_delta is cumulative, so it is absorbed into the
			// usage reported at message_start rather than summed.
			if len(e.Usage) > 0 {
				usage, tier, err := decodeUsage(e.Usage)
				if err != nil {
					return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode stream usage: %w", err)
				}
				if tier != "" {
					result.ServiceTier = tier
				}
				if usage != nil {
					if result.Usage == nil {
						result.Usage = &llm.Usage{}
					}
					result.Usage.Absorb(usage)
					providerhttp.Emit(sink, llm.Delta{Kind: llm.DeltaUsage, Usage: result.Usage})
				}
			}
		case "message_stop":
			if !started {
				return result, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "stop before start")
			}
			if len(stopped) != len(blocks) {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "message stopped with open blocks")
			}
			built := build()
			if built.Stop.Raw == "" {
				return built, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "message stopped without a stop reason")
			}
			return built, buildErr
		case "ping":
			// Keep-alive with no content.
		case "error":
			return build(), providerhttp.Classify(&llm.Error{
				Provider: wire.origin.Provider,
				Category: llm.ErrorProtocol,
				Body:     raw,
				Cause:    errors.New("provider stream error"),
			}, errorReason)
		default:
			return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorUnsupported, "unknown stream event %q", e.Type)
		}
	}
	err := reader.Err()
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorTransport, "stream interrupted: %w", err)
}

// appendString appends s to the string stored under key in m, treating a
// missing value as empty.
func appendString(m map[string]any, key, s string) {
	old, _ := m[key].(string)
	m[key] = old + s
}
