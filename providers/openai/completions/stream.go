package completions

import (
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
	var text, reasoning strings.Builder
	calls := map[int]*ToolCall{}
	var reason string
	seen := false
	build := func(complete bool) (llm.Result, error) {
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
		if text.Len() > 0 {
			result.Message.Content = append(
				result.Message.Content,
				llm.Text(text.String()),
			)
		}
		for _, index := range slices.Sorted(maps.Keys(calls)) {
			call := calls[index]
			v := tool.Call{
				ID:   call.ID,
				Name: call.Function.Name,
				Args: json.RawMessage(call.Function.Arguments),
			}
			if err := v.Validate(); err != nil {
				result.Message.Content = append(
					result.Message.Content,
					wire.partialToolCall(*call),
				)
				if complete {
					return result, providerhttp.Failure(
						wire.provider,
						llm.ErrorProtocol,
						"invalid streamed tool call: %w",
						err,
					)
				}
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
		}
		return result, nil
	}
	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var chunk StreamResponse
		if err := json.Unmarshal(raw, &chunk); err != nil {
			partial, _ := build(false)
			return partial, providerhttp.Failure(
				wire.provider,
				llm.ErrorProtocol,
				"decode stream chunk: %w",
				err,
			)
		}
		// A chunk that carries an error object, or is one (vLLM sends
		// "object":"error"), ends the call even when it also has a finish
		// reason: OpenRouter pairs its error with finish_reason "error".
		if (len(chunk.Error) > 0 &&
			string(chunk.Error) != "null") ||
			chunk.Object == "error" {
			partial, _ := build(false)
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
			partial, _ := build(false)
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
			partial, _ := build(false)
			return partial, wire.unsupported("structured reasoning details")
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
				partial, _ := build(false)
				return partial, wire.unsupported("stream content part %q", part.Type)
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			call := calls[delta.Index]
			if call == nil {
				call = &ToolCall{Type: "function"}
				calls[delta.Index] = call
			}
			if delta.Type != "" && delta.Type != "function" {
				partial, _ := build(false)
				return partial, wire.unsupported("stream tool call type %q", delta.Type)
			}
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Function.Name != "" {
				call.Function.Name = delta.Function.Name
			}
			call.Function.Arguments += delta.Function.Arguments
			providerhttp.Emit(
				sink,
				llm.Delta{
					Kind:  llm.DeltaToolArguments,
					Index: delta.Index,
					Text:  delta.Function.Arguments,
				},
			)
		}
		if choice.FinishReason != "" {
			reason = choice.FinishReason
		}
	}
	partial, _ := build(false)
	if ctx.Err() != nil {
		return partial, ctx.Err()
	}
	if err := reader.Err(); err != nil {
		return partial, providerhttp.Failure(
			wire.provider,
			llm.ErrorTransport,
			"stream interrupted: %w",
			err,
		)
	}
	if !seen || reason == "" {
		return partial, providerhttp.Failure(
			wire.provider,
			llm.ErrorTransport,
			"stream ended without finish reason: %w",
			io.ErrUnexpectedEOF,
		)
	}
	return build(true)
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
