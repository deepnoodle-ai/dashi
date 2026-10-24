package gemini

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/internal/sse"
	"github.com/deepnoodle-ai/dashi/llm"
)

// readStream assembles GenerateContent SSE chunks before authoritative decode.
// Google sends content fragments, while usage, finish reason, and grounding
// metadata can arrive on later chunks. A read failure retains the parts
// already observed.
//
// Provider-tool counts reach the final Usage whichever chunk carried them:
// the assembled response keeps the latest grounding and URL context metadata,
// and the counts seen across all chunks are merged in by maximum, so metadata
// that repeats across chunks is never summed.
func (wire codec) readStream(
	ctx context.Context,
	body io.ReadCloser,
	initial llm.Result,
	sink llm.Sink,
) (llm.Result, error) {
	reader := sse.NewServerSentEventsReader[json.RawMessage](body)
	var parts []map[string]any
	var responseID, modelVersion, reason string
	var usage, grounding, urlContext json.RawMessage
	var meters streamMeters
	var lastUsage *llm.Usage
	var indexes partIndexes
	seen := false
	// buildErr keeps a decode failure of the assembled response, so a stream
	// that otherwise ended cleanly still reports it.
	var buildErr error
	build := func() llm.Result {
		candidate := map[string]any{}
		if len(parts) > 0 {
			candidate["content"] = map[string]any{"role": "model", "parts": parts}
		}
		if reason != "" {
			candidate["finishReason"] = reason
		}
		if grounding != nil {
			candidate["groundingMetadata"] = grounding
		}
		if urlContext != nil {
			candidate["urlContextMetadata"] = urlContext
		}
		response := map[string]any{
			"responseId":   responseID,
			"modelVersion": modelVersion,
			"candidates":   []any{candidate},
		}
		if usage != nil {
			response["usageMetadata"] = usage
		}
		raw, _ := json.Marshal(response)
		result, err := wire.decode(raw, initial.Requested)
		if err != nil {
			buildErr = err
			return initial
		}
		recordMeters(result.Usage, meters.counts())
		return result
	}
	for {
		raw, ok := reader.Next()
		if !ok {
			break
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return build(), providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"decode stream chunk: %w",
				err,
			)
		}
		if len(envelope["error"]) > 0 {
			return build(), providerhttp.Classify(
				&llm.Error{
					Provider: wire.origin.Provider,
					Category: llm.ErrorProtocol,
					Body:     raw,
				},
				errorReason,
				wire.media,
			)
		}
		var chunk responseWire
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return build(), providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"decode stream chunk: %w",
				err,
			)
		}
		if chunk.ResponseID != "" {
			responseID = chunk.ResponseID
		}
		if chunk.ModelVersion != "" {
			modelVersion = chunk.ModelVersion
		}
		changed := false
		if len(chunk.Candidates) > 0 {
			candidate := chunk.Candidates[0]
			if present(candidate.GroundingMetadata) {
				grounding = candidate.GroundingMetadata
			}
			if present(candidate.URLContextMetadata) {
				urlContext = candidate.URLContextMetadata
			}
			changed = meters.observe(
				candidate.GroundingMetadata,
				candidate.URLContextMetadata,
				codeRuns(candidate.Content),
			)
		}
		if chunk.UsageMetadata != nil {
			usage = envelope["usageMetadata"]
			normalized, err := chunk.UsageMetadata.normalize()
			if err != nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"%w",
					err,
				)
			}
			lastUsage = normalized
			changed = true
		}
		// A usage delta also follows metadata that arrives after the last
		// usageMetadata, so live totals see the counts before the Result.
		if changed && lastUsage != nil {
			providerhttp.Emit(sink, llm.Delta{
				Kind:  llm.DeltaUsage,
				Usage: usageWithMeters(lastUsage, meters.counts()),
			})
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
			if err := providerhttp.Decode(rawPart, &decoded); err != nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"decode stream part: %w",
					err,
				)
			}
			var part map[string]any
			if err := providerhttp.Decode(rawPart, &part); err != nil || part == nil {
				return build(), providerhttp.Failure(
					wire.origin.Provider,
					llm.ErrorProtocol,
					"invalid stream part",
				)
			}
			index, merge := indexes.next(part)
			if decoded.Text != "" {
				kind := llm.DeltaText
				if decoded.Thought {
					kind = llm.DeltaReasoning
				}
				providerhttp.Emit(
					sink,
					llm.Delta{
						Kind:  kind,
						Index: index,
						Text:  decoded.Text,
					},
				)
			}
			if decoded.FunctionCall != nil {
				args, _ := callArgs(decoded.FunctionCall.Args)
				providerhttp.Emit(
					sink,
					llm.Delta{
						Kind:  llm.DeltaToolArguments,
						Index: index,
						Text:  string(args),
					},
				)
			}
			if merge {
				last := parts[len(parts)-1]
				last["text"] = last["text"].(string) + decoded.Text
				if decoded.ThoughtSignature != "" {
					last["thoughtSignature"] = decoded.ThoughtSignature
				}
				continue
			}
			parts = append(parts, part)
		}
	}
	partial := build()
	if ctx.Err() != nil {
		return partial, ctx.Err()
	}
	if err := reader.Err(); err != nil {
		return partial, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorTransport,
			"stream interrupted: %w",
			err,
		)
	}
	if buildErr != nil {
		return partial, buildErr
	}
	if !seen || strings.TrimSpace(reason) == "" {
		return partial, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorTransport,
			"stream ended without finish reason: %w",
			io.ErrUnexpectedEOF,
		)
	}
	return partial, nil
}

// partIndexes numbers stream parts as the assembled message numbers them.
// Google splits one text or thought part across chunks, so a fragment that
// holds only text extends the text part before it; any other part starts a
// new one. Both stream paths use it, so their delta indexes agree with each
// other and with the final Message.Content.
type partIndexes struct {
	count int
	// open reports whether the latest part is text that a fragment may
	// extend; thought is its thought flag.
	open    bool
	thought bool
}

// next returns the part's index and whether it extends the previous part.
func (x *partIndexes) next(part map[string]any) (index int, merge bool) {
	text, hasText, thought, signed, ok := textFragment(part)
	if x.open && ok && thought == x.thought && (text != "" || signed) {
		// A signature closes the part it lands on.
		x.open = !signed
		return x.count - 1, true
	}
	x.count++
	x.open, x.thought = ok && hasText && !signed, thought
	return x.count - 1, false
}

// textFragment reports whether part holds only text fields: text, thought,
// and thoughtSignature. A signature may arrive in a part of its own.
func textFragment(
	part map[string]any,
) (text string, hasText, thought, signed, ok bool) {
	for key, value := range part {
		switch key {
		case "text":
			text, hasText = value.(string)
			if !hasText {
				return "", false, false, false, false
			}
		case "thought":
			thought = value == true
		case "thoughtSignature":
			signed = true
		default:
			return "", false, false, false, false
		}
	}
	return text, hasText, thought, signed, hasText || signed
}

// codeRuns counts the executableCode parts in one chunk's content. Each is one
// code run in Google's sandbox.
func codeRuns(content *contentWire) int {
	if content == nil {
		return 0
	}
	runs := 0
	for _, raw := range content.Parts {
		var part struct {
			ExecutableCode json.RawMessage `json:"executableCode"`
		}
		if json.Unmarshal(raw, &part) == nil && present(part.ExecutableCode) {
			runs++
		}
	}
	return runs
}
