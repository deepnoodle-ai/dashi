// Ported from Dive (65c393f), Apache-2.0; adapted for Dashi.

package llm

import "strings"

// StopKind groups the stop reasons providers report by what an agent does
// with the response. Result.Stop.Raw keeps the provider's raw value;
// Providers can refine ClassifyStopReason using the response's content.
type StopKind string

const (
	// StopFinished is a response the model ended on its own: end_turn,
	// stop, stop_sequence. Its tool calls, if any, are meant to run.
	StopFinished StopKind = "finished"

	// StopToolUse is a response that ended in order to run its tool
	// calls.
	StopToolUse StopKind = "tool_use"

	// StopOutputLimit is a response cut off at the output token limit:
	// max_tokens, length. A tool call in it may be truncated.
	StopOutputLimit StopKind = "output_limit"

	// StopContextLimit is a response cut off because the conversation
	// filled the model's context window (Anthropic's
	// model_context_window_exceeded, Mistral's model_length).
	// Unlike an output limit, the same history cannot be sent again as is.
	StopContextLimit StopKind = "context_limit"

	// StopRefusal is a response the provider or model declined to
	// complete: refusal, content_filter, and Gemini's safety and recitation
	// reasons.
	StopRefusal StopKind = "refusal"

	// StopPause is a server tool loop the provider paused (pause_turn).
	// The conversation is sent again, unchanged, to let it continue.
	StopPause StopKind = "pause"

	// StopIncomplete is a response the provider ended early for a reason
	// it did not name as a limit or a refusal: an incomplete, cancelled,
	// timed-out or failed response, or a malformed tool call.
	StopIncomplete StopKind = "incomplete"

	// StopOther is a stop reason Dashi does not recognize, including an
	// empty one. An agent treats such a response as finished only when it
	// carries no client tool call, and never runs a call on it.
	StopOther StopKind = "other"
)

// stopKinds maps every stop reason Dashi's providers report, lowercased, to
// its kind. Adapters that normalize a provider's values say so where they
// map them; the raw spellings below are the ones that reach a result.
var stopKinds = map[string]StopKind{
	// Anthropic, and Ollama through its Anthropic-compatible API.
	"end_turn":                      StopFinished,
	"stop_sequence":                 StopFinished,
	"tool_use":                      StopToolUse,
	"max_tokens":                    StopOutputLimit,
	"model_context_window_exceeded": StopContextLimit,
	"refusal":                       StopRefusal,
	"pause_turn":                    StopPause,

	// Chat Completions (OpenAI, Mistral, OpenRouter, DeepInfra). The adapter
	// reports tool_calls and function_call as tool_use.
	"stop":           StopFinished,
	"length":         StopOutputLimit,
	"model_length":   StopContextLimit, // Mistral: the model's context length was reached
	"content_filter": StopRefusal,
	"tool_calls":     StopToolUse,
	"function_call":  StopToolUse,
	"error":          StopIncomplete,

	// OpenAI Responses, as the adapter maps a response's status and
	// incomplete_details (end_turn, tool_use, max_tokens and content_filter
	// are above).
	"incomplete":        StopIncomplete,
	"completed":         StopFinished,
	"failed":            StopIncomplete,
	"max_output_tokens": StopOutputLimit,
	"cancelled":         StopIncomplete,
	"timeout":           StopIncomplete,

	// Gemini finish reasons, lowercased by the adapter (stop and max_tokens
	// are above). LANGUAGE, OTHER, NO_IMAGE, IMAGE_OTHER and
	// FINISH_REASON_UNSPECIFIED stay StopOther.
	"safety":                   StopRefusal,
	"recitation":               StopRefusal,
	"blocklist":                StopRefusal,
	"prohibited_content":       StopRefusal,
	"spii":                     StopRefusal,
	"image_safety":             StopRefusal,
	"image_prohibited_content": StopRefusal,
	"image_recitation":         StopRefusal,
	"malformed_function_call":  StopIncomplete,
	"unexpected_tool_call":     StopIncomplete,
	"too_many_tool_calls":      StopIncomplete,
}

// ClassifyStopReason maps a provider's stop reason, as Result.Stop.Raw
// carries it, onto the kind an agent acts on. The match ignores case. A
// value Dashi does not recognize, and an empty one, is StopOther.
func ClassifyStopReason(reason string) StopKind {
	if kind, ok := stopKinds[strings.ToLower(reason)]; ok {
		return kind
	}
	return StopOther
}
