package completions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/internal/sse"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// readStream adapts Dive's Chat Completions chunk accumulation to Dashi's
// provisional deltas and one authoritative final result. Finish reasons are
// held until the trailing usage chunk has been read.
func (wire codec) readStream(
	ctx context.Context,
	body io.ReadCloser,
	initial llm.Result,
	sink llm.Sink,
) (llm.Result, error) {
	reader := sse.NewServerSentEventsReader[json.RawMessage](body)
	result := initial
	var text, reasoning, refusalText strings.Builder
	// details collects the elements of each streamed reasoning_details array,
	// merging fragments of one item, into the array a complete response
	// carries.
	var details reasoningDetailItems
	calls := map[int]*ToolCall{}
	callIDs := map[string]int{}
	callRaw := map[int][]byte{}
	var reason string
	seen := false
	// build assembles the result. Only a stream that reached its finish
	// reason is complete; on any other exit every tool call becomes partial
	// opaque content, even one whose arguments happen to be valid JSON, so a
	// call the provider never confirmed cannot be executed or replayed.
	build := func(complete bool) llm.Result {
		result.Message = llm.Message{Role: llm.RoleAssistant}
		if reasoning.Len() > 0 {
			result.Message.Content = append(
				result.Message.Content,
				llm.Content{
					Kind:      llm.ContentReasoning,
					Reasoning: &llm.Reasoning{Text: reasoning.String()},
				},
			)
		}
		if len(details) > 0 {
			raw := details.marshal()
			c, _ := wire.reasoningDetails(raw)
			result.Message.Content = append(result.Message.Content, c)
		}
		if text.Len() > 0 {
			result.Message.Content = append(
				result.Message.Content,
				llm.Text(text.String()),
			)
		}
		if refusalText.Len() > 0 {
			result.Message.Content = append(
				result.Message.Content,
				refusal(refusalText.String()),
			)
		}
		for _, index := range slices.Sorted(maps.Keys(calls)) {
			call := calls[index]
			v := tool.Call{
				ID:   call.ID,
				Name: call.Function.Name,
				Args: json.RawMessage(call.Function.Arguments),
			}
			if !complete || v.Validate() != nil {
				result.Message.Content = append(
					result.Message.Content,
					wire.partialToolCall(*call),
				)
				continue
			}
			result.Message.Content = append(
				result.Message.Content,
				llm.Content{
					Kind:     llm.ContentToolCall,
					ToolCall: &v,
				},
			)
		}
		if complete {
			result.Stop = llm.Stop{Kind: llm.ClassifyStopReason(reason), Raw: reason}
			if refusalText.Len() > 0 {
				result.Stop.Kind = refusalStop(result.Stop.Kind)
			}
		}
		return result
	}
	protocolError := func(raw []byte, format string, args ...any) (llm.Result, error) {
		return build(false), &llm.Error{
			Provider: wire.provider,
			Category: llm.ErrorProtocol,
			Body:     slices.Clone(raw),
			Cause:    fmt.Errorf(format, args...),
		}
	}
	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var chunk StreamResponse
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return protocolError(raw, "decode stream chunk: %w", err)
		}
		// A chunk that carries an error object, or is one (vLLM sends
		// "object":"error"), ends the call even when it also has a finish
		// reason: OpenRouter pairs its error with finish_reason "error".
		if (len(chunk.Error) > 0 &&
			string(chunk.Error) != "null") ||
			chunk.Object == "error" {
			partial := build(false)
			cause := errors.New("provider stream error")
			if message := streamErrorMessage(chunk.Error, raw); message != "" {
				cause = fmt.Errorf("provider stream error: %s", message)
			}
			return partial, providerhttp.Classify(
				&llm.Error{
					Provider: wire.provider,
					Category: llm.ErrorProtocol,
					Body:     slices.Clone(raw),
					Cause:    cause,
				},
				providerhttp.OpenAIReason,
				wire.media,
			)
		}
		if chunk.ID != "" {
			result.ID = chunk.ID
		}
		if chunk.Model != "" {
			result.Served = llm.ModelRef{Provider: wire.provider, Model: chunk.Model}
		}
		if chunk.ServiceTier != "" {
			result.ServiceTier = chunk.ServiceTier
		}
		if chunk.Usage.present {
			usage := chunk.Usage.toLLMUsage()
			result.Usage = &usage
			result.Cost = wire.reportedCost(chunk.Usage)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind:  llm.DeltaUsage,
					Usage: result.Usage,
				},
			)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
			partial := build(false)
			return partial, providerhttp.Failure(
				wire.provider,
				llm.ErrorProtocol,
				"stream has unexpected choices",
			)
		}
		seen = true
		choice := chunk.Choices[0]
		if choice.Delta.Reasoning != "" {
			reasoning.WriteString(choice.Delta.Reasoning)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind: llm.DeltaReasoning,
					Text: choice.Delta.Reasoning,
				},
			)
		}
		if len(choice.Delta.ReasoningDetails) > 0 {
			var fragment []json.RawMessage
			if err := json.Unmarshal(choice.Delta.ReasoningDetails, &fragment); err != nil {
				return protocolError(raw, "decode stream reasoning details: %w", err)
			}
			for _, element := range fragment {
				details.add(element)
			}
		}
		if choice.Delta.Content != "" {
			text.WriteString(choice.Delta.Content)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind: llm.DeltaText,
					Text: choice.Delta.Content,
				},
			)
		}
		if choice.Delta.Refusal != "" {
			refusalText.WriteString(choice.Delta.Refusal)
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind: llm.DeltaText,
					Text: choice.Delta.Refusal,
				},
			)
		}
		for _, part := range choice.Delta.ContentParts {
			switch part.Type {
			case "text":
				text.WriteString(part.Text)
				providerhttp.Emit(sink, llm.Delta{Kind: llm.DeltaText, Text: part.Text})
			case "thinking":
				fragment := joinTextParts(part.Thinking)
				reasoning.WriteString(fragment)
				providerhttp.Emit(
					sink,
					llm.Delta{
						Kind: llm.DeltaReasoning,
						Text: fragment,
					},
				)
			default:
				partial := build(false)
				return partial, wire.unsupported("stream content part %q", part.Type)
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			if delta.Type != "" && delta.Type != "function" {
				partial := build(false)
				return partial, wire.unsupported("stream tool call type %q", delta.Type)
			}
			index := delta.Index
			if delta.indexPresent {
				if index < 0 {
					return protocolError(raw, "negative stream tool index %d", index)
				}
				if prior, ok := callIDs[delta.ID]; delta.ID != "" &&
					ok && prior != index {
					return protocolError(
						raw,
						"duplicate stream tool ID %q at indices %d and %d",
						delta.ID,
						prior,
						index,
					)
				}
			} else {
				var ok bool
				index, ok = callIDs[delta.ID]
				if delta.ID == "" || !ok {
					return protocolError(
						raw,
						"stream tool fragment has no index or matching ID",
					)
				}
			}
			call := calls[index]
			if call == nil {
				call = &ToolCall{Type: "function"}
				calls[index] = call
			}
			if call.ID != "" && delta.ID != "" && call.ID != delta.ID {
				return protocolError(
					raw,
					"stream tool index %d changed ID from %q to %q",
					index,
					call.ID,
					delta.ID,
				)
			}
			if call.Function.Name != "" && delta.Function.Name != "" &&
				call.Function.Name != delta.Function.Name {
				return protocolError(
					raw,
					"stream tool index %d changed name from %q to %q",
					index,
					call.Function.Name,
					delta.Function.Name,
				)
			}
			if delta.ID != "" {
				call.ID = delta.ID
				callIDs[delta.ID] = index
			}
			if delta.Function.Name != "" {
				call.Function.Name = delta.Function.Name
			}
			call.Function.Arguments += delta.Function.Arguments
			callRaw[index] = slices.Clone(raw)
			if delta.Function.Arguments != "" {
				providerhttp.Emit(
					sink,
					llm.Delta{
						Kind:  llm.DeltaToolArguments,
						Index: index,
						Text:  delta.Function.Arguments,
					},
				)
			}
		}
		if choice.FinishReason != "" {
			reason = choice.FinishReason
		}
	}
	partial := build(false)
	if ctx.Err() != nil {
		return partial, ctx.Err()
	}
	if err := reader.Err(); err != nil {
		return partial, providerhttp.StreamFailure(wire.provider, err)
	}
	if !seen || reason == "" {
		return partial, providerhttp.Failure(
			wire.provider,
			llm.ErrorTransport,
			"stream ended without finish reason: %w",
			io.ErrUnexpectedEOF,
		)
	}
	seenIDs := map[string]bool{}
	for _, index := range slices.Sorted(maps.Keys(calls)) {
		call := calls[index]
		if err := (tool.Call{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: json.RawMessage(call.Function.Arguments),
		}).Validate(); err != nil {
			return protocolError(
				callRaw[index],
				"invalid streamed tool call at index %d: %w",
				index,
				err,
			)
		}
		if seenIDs[call.ID] {
			return protocolError(
				callRaw[index],
				"duplicate streamed tool ID %q",
				call.ID,
			)
		}
		seenIDs[call.ID] = true
	}
	return build(true), nil
}

// streamErrorMessage returns the provider's message from an in-stream error:
// the nested {"error":{"message":...}} shape, a bare {"error":"..."}, or the
// flat {"object":"error","message":...} shape. It is empty when none decodes.
func streamErrorMessage(nested json.RawMessage, raw []byte) string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(nested, &v) == nil && v.Message != "" {
		return v.Message
	}
	var s string
	if json.Unmarshal(nested, &s) == nil && s != "" {
		return s
	}
	if json.Unmarshal(raw, &v) == nil {
		return v.Message
	}
	return ""
}

func (wire codec) partialToolCall(call ToolCall) llm.Content {
	raw, _ := json.Marshal(call)
	return llm.Content{
		Kind: llm.ContentOpaque,
		Opaque: &llm.Opaque{
			Version: 1,
			Origin:  wire.origin,
			Type:    "tool_call",
			Unit:    llm.OpaqueUnitItem,
			Partial: true,
			Raw:     raw,
		},
	}
}

// reasoningDetailItems merges streamed reasoning_details elements. A provider
// such as OpenRouter splits one item over several chunks that share an
// "index" and "type"; the complete response carries the merged item. String
// fields that carry content (text, summary, data) are concatenated in arrival
// order. Other fields keep their first non-null value. Field order follows
// first arrival so the merged item replays as the provider sent it. Elements
// that are not objects or have no index stay separate.
type reasoningDetailItems []*reasoningDetailItem

type reasoningDetailItem struct {
	raw    json.RawMessage // set for an element that is not merged
	keys   []string
	fields map[string]json.RawMessage
	index  string
	kind   string
}

func (items *reasoningDetailItems) add(element json.RawMessage) {
	var fields map[string]json.RawMessage
	var keys []string
	if err := json.Unmarshal(element, &fields); err == nil && fields != nil {
		keys = objectKeys(element)
	}
	index, hasIndex := fields["index"]
	if fields == nil || !hasIndex || len(keys) != len(fields) {
		*items = append(*items, &reasoningDetailItem{raw: element})
		return
	}
	kind := string(fields["type"])
	for _, item := range *items {
		if item.raw != nil || item.index != string(index) || item.kind != kind {
			continue
		}
		item.merge(keys, fields)
		return
	}
	*items = append(*items, &reasoningDetailItem{
		keys:   keys,
		fields: fields,
		index:  string(index),
		kind:   kind,
	})
}

func (item *reasoningDetailItem) merge(
	keys []string,
	fields map[string]json.RawMessage,
) {
	for _, key := range keys {
		value := fields[key]
		current, ok := item.fields[key]
		if !ok || string(current) == "null" {
			if !ok {
				item.keys = append(item.keys, key)
			}
			item.fields[key] = value
			continue
		}
		switch key {
		case "text", "summary", "data":
			var a, b string
			if json.Unmarshal(current, &a) != nil ||
				json.Unmarshal(value, &b) != nil {
				continue
			}
			// Marshaling a string cannot fail.
			item.fields[key], _ = json.Marshal(a + b)
		}
	}
}

// marshal returns the merged items as one JSON array.
func (items reasoningDetailItems) marshal() json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		if item.raw != nil {
			buf.Write(item.raw)
			continue
		}
		buf.WriteByte('{')
		for j, key := range item.keys {
			if j > 0 {
				buf.WriteByte(',')
			}
			// Marshaling a string cannot fail.
			name, _ := json.Marshal(key)
			buf.Write(name)
			buf.WriteByte(':')
			buf.Write(item.fields[key])
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// objectKeys returns the top-level keys of a JSON object in source order.
func objectKeys(object json.RawMessage) []string {
	decoder := json.NewDecoder(bytes.NewReader(object))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		key, _ := token.(string)
		keys = append(keys, key)
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil
		}
	}
	return keys
}
