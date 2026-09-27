package gemini

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/internal/sse"
	"github.com/deepnoodle-ai/kernel/llm"
)

// readStream assembles GenerateContent SSE chunks before authoritative decode.
// Google sends content fragments, while usage and finish reason can arrive on
// later chunks. A read failure retains the parts already observed.
func (wire codec) readStream(ctx context.Context, body io.ReadCloser, initial llm.Result, sink llm.Sink) (llm.Result, error) {
	reader := sse.NewServerSentEventsReader[json.RawMessage](body)
	var parts []map[string]any
	var responseID, modelVersion, reason string
	var usage json.RawMessage
	seen := false
	build := func() llm.Result {
		candidate := map[string]any{"content": map[string]any{"role": "model", "parts": parts}}
		if reason != "" {
			candidate["finishReason"] = reason
		}
		response := map[string]any{"responseId": responseID, "modelVersion": modelVersion,
			"candidates": []any{candidate}}
		if usage != nil {
			response["usageMetadata"] = usage
		}
		raw, _ := json.Marshal(response)
		result, err := wire.decode(raw, initial.Requested)
		if err != nil {
			return initial
		}
		return result
	}
	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode stream chunk: %w", err)
		}
		if len(envelope["error"]) > 0 {
			return build(), &llm.Error{Provider: wire.origin.Provider, Category: llm.ErrorProtocol, Body: raw}
		}
		var chunk responseWire
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode stream chunk: %w", err)
		}
		if chunk.ResponseID != "" {
			responseID = chunk.ResponseID
		}
		if chunk.ModelVersion != "" {
			modelVersion = chunk.ModelVersion
		}
		if chunk.UsageMetadata != nil {
			usage = envelope["usageMetadata"]
			normalized, err := chunk.UsageMetadata.normalize()
			if err != nil {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "%w", err)
			}
			providerhttp.Emit(sink, llm.Delta{Kind: llm.DeltaUsage, Usage: normalized})
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		seen = true
		candidate := chunk.Candidates[0]
		if candidate.FinishReason != "" {
			reason = candidate.FinishReason
		}
		if candidate.Content == nil {
			continue
		}
		for _, rawPart := range candidate.Content.Parts {
			var decoded partWire
			if err := json.Unmarshal(rawPart, &decoded); err != nil {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode stream part: %w", err)
			}
			var part map[string]any
			if err := providerhttp.Decode(rawPart, &part); err != nil || part == nil {
				return build(), providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "invalid stream part")
			}
			if decoded.Text != "" {
				kind := llm.DeltaText
				if decoded.Thought {
					kind = llm.DeltaReasoning
				}
				providerhttp.Emit(sink, llm.Delta{Kind: kind, Index: len(parts), Text: decoded.Text})
			}
			if decoded.FunctionCall != nil {
				args, _ := json.Marshal(decoded.FunctionCall.Args)
				providerhttp.Emit(sink, llm.Delta{Kind: llm.DeltaToolArguments, Index: len(parts), Text: string(args)})
			}
			if len(parts) > 0 && decoded.FunctionCall == nil {
				last := parts[len(parts)-1]
				_, lastText := last["text"].(string)
				_, hasSignature := last["thoughtSignature"]
				if lastText && !hasSignature && decoded.Thought == (last["thought"] == true) &&
					len(decoded.InlineData) == 0 && len(decoded.FileData) == 0 &&
					(decoded.Text != "" || decoded.ThoughtSignature != "") {
					last["text"] = last["text"].(string) + decoded.Text
					if decoded.ThoughtSignature != "" {
						last["thoughtSignature"] = decoded.ThoughtSignature
					}
					continue
				}
			}
			parts = append(parts, part)
		}
	}
	partial := build()
	if ctx.Err() != nil {
		return partial, ctx.Err()
	}
	if err := reader.Err(); err != nil {
		return partial, providerhttp.Failure(wire.origin.Provider, llm.ErrorTransport, "stream interrupted: %w", err)
	}
	if !seen || strings.TrimSpace(reason) == "" {
		return partial, providerhttp.Failure(wire.origin.Provider, llm.ErrorTransport,
			"stream ended without finish reason: %w", io.ErrUnexpectedEOF)
	}
	return partial, nil
}
