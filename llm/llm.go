// Package llm defines one model call, independent of agent and session state.
// Agent loops, sessions, and recording build on this package; nothing here
// knows about turns, so every caller (normal execution, compaction, child
// work) goes through the same Prepare and Send pipeline.
// Usage, pricing, and stop classification are adapted from Dive (65c393f),
// Apache-2.0; the request and preparation contracts are defined by Dashi.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/contentsrc"
	"github.com/deepnoodle-ai/dashi/internal/outputschema"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Role attributes a message to its author. RoleDeveloper stays distinct from
// RoleSystem where supported; other APIs receive developer instructions as
// system content.
type Role string

// Message roles.
const (
	// RoleUser attributes input or tool results to the caller.
	RoleUser Role = "user"
	// RoleAssistant attributes generated content or tool calls to the model.
	RoleAssistant Role = "assistant"
	// RoleSystem carries instructions with system-level authority.
	RoleSystem Role = "system"
	// RoleDeveloper preserves developer instructions separately where the API
	// supports them.
	RoleDeveloper Role = "developer"
)

// ContentKind identifies the payload carried by a Content value.
type ContentKind string

// Content kinds. ContentBlock carries shared content.Block values; the others
// are model-specific control and replay data.
const (
	// ContentBlock wraps a shared content block without model control data.
	ContentBlock ContentKind = "block"
	// ContentToolCall requests execution of an identified tool call.
	ContentToolCall ContentKind = "tool_call"
	// ContentToolResult answers a tool call with its output and failure flag.
	ContentToolResult ContentKind = "tool_result"
	// ContentProviderToolUse reports a provider-executed call, never a local call.
	ContentProviderToolUse ContentKind = "provider_tool_use"
	// ContentProviderToolResult reports a separate provider-executed result block.
	ContentProviderToolResult ContentKind = "provider_tool_result"
	// ContentProviderToolApprovalRequest asks for an application decision.
	ContentProviderToolApprovalRequest ContentKind = "provider_tool_approval_request"
	// ContentProviderToolApprovalResponse records that application decision.
	ContentProviderToolApprovalResponse ContentKind = "provider_tool_approval_response"
	// ContentReasoning carries display reasoning and optional private replay
	// data.
	ContentReasoning ContentKind = "reasoning"
	// ContentRefusal preserves refusal text separately from ordinary output.
	ContentRefusal ContentKind = "refusal"
	// ContentReminder carries named runtime context until provider preparation.
	ContentReminder ContentKind = "reminder"
	// ContentOpaque retains provider content outside the canonical vocabulary.
	ContentOpaque ContentKind = "opaque"
)

// Reasoning is model reasoning output, kept so it can be shown to people and
// replayed to the provider that produced it.
type Reasoning struct {
	// Text is a display projection; editing it does not alter private replay
	// data.
	Text string `json:"text"`
}

// ToolResult answers the tool call identified by CallID. IsError tells the
// model that Output describes a failed call rather than a normal result.
type ToolResult struct {
	// CallID must identify the tool call this result answers and be valid UTF-8
	// so stored JSON preserves that association.
	CallID string `json:"call_id"`

	// Output supplies the content to return to the model.
	Output tool.Output `json:"output"`

	// IsError marks a known tool failure the model can respond to.
	IsError bool `json:"is_error,omitempty"`
}

// Content is a model-message union. Block holds shared content.Block values;
// control and provider replay data stay in this package.
type Content struct {
	// Kind selects exactly one payload; Replay may accompany known
	// content.
	Kind ContentKind `json:"kind"`

	// Block is required only for ContentBlock.
	Block *content.Block `json:"block,omitempty"`

	// ToolCall is required only for ContentToolCall.
	ToolCall *tool.Call `json:"tool_call,omitempty"`

	// ToolResult is required only for ContentToolResult.
	ToolResult *ToolResult `json:"tool_result,omitempty"`

	// ProviderToolUse is provider activity and cannot be executed locally.
	ProviderToolUse *ProviderToolUse `json:"provider_tool_use,omitempty"`

	// ProviderToolResult is provider activity paired with ProviderToolUse.
	ProviderToolResult *ProviderToolResult `json:"provider_tool_result,omitempty"`

	// ProviderToolApprovalRequest requires an explicit application response.
	ProviderToolApprovalRequest *ProviderToolApprovalRequest `json:"provider_tool_approval_request,omitempty"`

	// ProviderToolApprovalResponse carries the application decision.
	ProviderToolApprovalResponse *ProviderToolApprovalResponse `json:"provider_tool_approval_response,omitempty"`

	// Reasoning is required only for ContentReasoning.
	Reasoning *Reasoning `json:"reasoning,omitempty"`

	// Refusal is required only for ContentRefusal; an empty refusal is still a
	// payload.
	Refusal *string `json:"refusal,omitempty"`

	// Reminder is runtime context, separate from user-authored text.
	Reminder *Reminder `json:"reminder,omitempty"`

	// Opaque is required only for ContentOpaque.
	Opaque *Opaque `json:"opaque,omitempty"`

	// Replay preserves private data on known content, including signed or
	// encrypted reasoning. It is incompatible with ContentOpaque.
	Replay *Opaque `json:"replay,omitempty"`
}

// Text constructs a shared text block without provider-specific replay data.
func Text(text string) Content {
	b := content.TextBlock(text)
	return Content{Kind: ContentBlock, Block: &b}
}

// Image constructs an image from source, such as content.Source{URL: u} or
// content.Source{Data: b}. Inline bytes may omit the MIME type; preparation
// detects it.
func Image(source content.Source) Content {
	return Block(content.ImageBlock(source))
}

// Document constructs a document from source with an optional title. The
// title is shown to APIs that accept one, is echoed on citations, and its file
// extension guides MIME detection for inline bytes without a MIME type.
func Document(source content.Source, title string) Content {
	return Block(content.DocumentBlock(source, title))
}

// Block wraps a block without cloning its nested data. Use Content.Clone before
// independently modifying a copy.
func Block(block content.Block) Content {
	return Content{Kind: ContentBlock, Block: &block}
}

// sources returns pointers to every source in c: a block's source and the
// sources in blocks of a tool result, hosted-use output, or hosted result. They
// alias c's payload.
func (c Content) sources() []*content.Source {
	var block, result, providerTool []content.Block
	if c.Block != nil {
		block = []content.Block{*c.Block}
	}
	if c.ToolResult != nil {
		result = c.ToolResult.Output.Content
	}
	if c.ProviderToolResult != nil {
		providerTool = c.ProviderToolResult.Output
	}
	if c.ProviderToolUse != nil {
		providerTool = append(providerTool[:len(providerTool):len(providerTool)], c.ProviderToolUse.Output...)
	}
	return contentsrc.Blocks(block, result, providerTool)
}

// Validate checks a recordable content value with exactly one matching payload.
// Partial provider data is valid here; Input.Validate prohibits its replay.
func (c Content) Validate() error {
	if count(
		c.Block != nil,
		c.ToolCall != nil,
		c.ToolResult != nil,
		c.ProviderToolUse != nil,
		c.ProviderToolResult != nil,
		c.ProviderToolApprovalRequest != nil,
		c.ProviderToolApprovalResponse != nil,
		c.Reasoning != nil,
		c.Refusal != nil,
		c.Reminder != nil,
		c.Opaque != nil,
	) != 1 {
		return errors.New("llm: content requires one payload")
	}
	if c.Replay != nil {
		if c.Kind == ContentReminder {
			return errors.New("llm: reminder cannot carry provider replay data")
		}
		if c.Kind == ContentOpaque {
			return errors.New("llm: opaque content cannot also carry replay data")
		}
		if c.Kind == ContentProviderToolApprovalResponse {
			return errors.New(
				"llm: provider tool approval response cannot carry provider replay",
			)
		}
		if err := c.Replay.Validate(); err != nil {
			return err
		}
	}
	switch {
	case c.Kind == ContentBlock && c.Block != nil:
		return c.Block.Validate()
	case c.Kind == ContentToolCall && c.ToolCall != nil:
		return c.ToolCall.Validate()
	case c.Kind == ContentToolResult && c.ToolResult != nil:
		if c.ToolResult.CallID == "" {
			return errors.New("llm: missing tool result call ID")
		}
		if !utf8.ValidString(c.ToolResult.CallID) {
			return errors.New("llm: tool result call ID must be valid UTF-8")
		}
		for _, b := range c.ToolResult.Output.Content {
			if err := b.Validate(); err != nil {
				return err
			}
		}
		return nil
	case c.Kind == ContentProviderToolUse && c.ProviderToolUse != nil:
		return c.ProviderToolUse.Validate()
	case c.Kind == ContentProviderToolResult && c.ProviderToolResult != nil:
		return c.ProviderToolResult.Validate()
	case c.Kind == ContentProviderToolApprovalRequest &&
		c.ProviderToolApprovalRequest != nil:
		return c.ProviderToolApprovalRequest.Validate()
	case c.Kind == ContentProviderToolApprovalResponse &&
		c.ProviderToolApprovalResponse != nil:
		return c.ProviderToolApprovalResponse.Validate()
	case c.Kind == ContentReasoning && c.Reasoning != nil:
		return nil
	case c.Kind == ContentRefusal && c.Refusal != nil:
		return nil
	case c.Kind == ContentReminder && c.Reminder != nil:
		return c.Reminder.Validate()
	case c.Kind == ContentOpaque && c.Opaque != nil:
		return c.Opaque.Validate()
	}
	return fmt.Errorf("llm: invalid content kind %q", c.Kind)
}

// ToolChoiceMode selects automatic, required, disabled, or named tool use.
type ToolChoiceMode string

// Tool choice modes.
const (
	// ToolChoiceAuto allows the model to decide whether to call tools.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceAny requires a tool call without naming a particular tool.
	ToolChoiceAny ToolChoiceMode = "any"
	// ToolChoiceNone disables tool calls for this request.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceTool requires the tool named by ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// DeltaKind identifies a provisional streaming observation.
type DeltaKind string

// Streaming delta kinds.
const (
	// DeltaText previews generated text before a final result is available.
	DeltaText DeltaKind = "text"
	// DeltaReasoning previews display reasoning without making it replayable.
	DeltaReasoning DeltaKind = "reasoning"
	// DeltaToolArguments previews argument fragments that may not yet be valid
	// JSON.
	DeltaToolArguments DeltaKind = "tool_arguments"
	// DeltaContent carries a provisional content snapshot.
	DeltaContent DeltaKind = "content"
	// DeltaUsage carries provisional accounting data from a stream.
	DeltaUsage DeltaKind = "usage"
)

// Message is role-attributed content exchanged with a model.
type Message struct {
	// Role sets the authority and interpretation of the content.
	Role Role `json:"role"`

	// Content preserves message order; copying Message shares its slice and
	// payloads.
	Content []Content `json:"content"`

	// Source describes generated output independently of its Result. It is nil
	// for caller-authored messages and does not authorize private replay.
	Source *MessageSource `json:"source,omitempty"`

	// Context preserves context slot identity through history and compaction.
	// Providers must not send this descriptive metadata to the model.
	Context *MessageContext `json:"context,omitempty"`
}

// MessageContext identifies a named context slot in projected conversation
// history. It adds no authority or provider-visible content. A nil Context on
// Message means that the slot identity is unknown.
type MessageContext struct {
	Origin string `json:"origin"`
	Name   string `json:"name"`
}

// MessageSource preserves response provenance when a message leaves its Result.
// Requested comes from the frozen request; Served and ResponseID are reported
// facts, empty when unknown. This is not an attempt or dispatch record.
type MessageSource struct {
	// Requested identifies the configured destination and requested model.
	Requested ModelRef `json:"requested"`
	// Served identifies the model reported by that destination, if known.
	Served ModelRef `json:"served"`
	// API identifies the wire protocol that produced this message.
	API string `json:"api"`
	// ResponseID is the provider-reported response identifier, if known.
	ResponseID string `json:"response_id,omitempty"`
}

// UserMessage constructs a user message with independently owned content.
func UserMessage(parts ...Content) Message {
	m := Message{Role: RoleUser, Content: make([]Content, len(parts))}
	for i, part := range parts {
		m.Content[i] = part.Clone()
	}
	return m
}

// AssistantMessage constructs an assistant message with independently owned
// content.
func AssistantMessage(parts ...Content) Message {
	m := UserMessage(parts...)
	m.Role = RoleAssistant
	return m
}

// Text concatenates the message's text blocks, skipping reasoning, refusals,
// and tool content.
func (m Message) Text() string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Block != nil && c.Block.Text != nil {
			b.WriteString(c.Block.Text.Text)
		}
	}
	return b.String()
}

// ReasoningSettings requests model reasoning by effort level or token budget.
type ReasoningSettings struct {
	// Effort selects a provider-supported level and excludes BudgetTokens.
	Effort string `json:"effort,omitempty"`

	// BudgetTokens selects a positive token budget and excludes Effort.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// ToolChoice constrains which tools the model may call. Mode is one of
// "auto", "any", "none", or "tool"; Name is set only in "tool" mode.
type ToolChoice struct {
	// Mode selects automatic, required, disabled, or named tool use.
	Mode ToolChoiceMode `json:"mode"`

	// Name must name a defined tool when Mode is ToolChoiceTool; otherwise it must
	// be empty.
	Name string `json:"name,omitempty"`
}

// DefaultMaxTokens is the output allowance preparation sends when
// Settings.MaxTokens is zero, for every provider. A model with a smaller known
// MaxOutputTokens gets that limit instead. The default is recorded as an
// AdjustmentDefault on the prepared request, not as a warning. Budget checks
// reserve cost against this allowance, so it bounds the worst-case cost of a
// call whose settings leave MaxTokens unset.
const DefaultMaxTokens = 32_768

// Settings are generation controls for one call. Pointer fields distinguish
// "not set, use the provider default" from an explicit zero or false.
type Settings struct {
	// MaxTokens limits output tokens, including any reasoning tokens the
	// provider counts as output. Zero selects DefaultMaxTokens during
	// preparation, lowered to the model's known output limit, so every call
	// has a finite allowance that a cost cap can bound.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature distinguishes an explicit zero from an omitted sampling
	// setting.
	Temperature *float64 `json:"temperature,omitempty"`

	// Reasoning optionally selects an effort level or token budget.
	Reasoning *ReasoningSettings `json:"reasoning,omitempty"`

	// ReasoningSummary optionally requests a public reasoning summary, independently
	// of effort or budget. False clears inherited opt-in; it does not filter
	// unsolicited output or remove earlier summaries and replay data.
	ReasoningSummary *bool `json:"reasoning_summary,omitempty"`

	// ToolChoice optionally constrains which tool the model may call.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// ParallelTools distinguishes explicitly disabled parallel calls from an
	// omitted setting.
	ParallelTools *bool `json:"parallel_tools,omitempty"`

	// Citations asks the model to cite passages of the request's documents,
	// including documents in tool results; the citations arrive on the answer's
	// text blocks. It applies to every document, because Anthropic enables
	// citations for all documents in a request or none. An API without
	// document citations omits the setting and warns when the request carries
	// a document; under CapabilityStrict that is an error. Cited answers arrive
	// as several text blocks, one per cited passage.
	Citations bool `json:"citations,omitempty"`

	// PromptCache asks the provider to reuse the start of the request. The
	// zero value caches where Dashi places cache points, with the
	// provider's default lifetime; see PromptCache.
	PromptCache PromptCache `json:"prompt_cache,omitzero"`
}

// Input specifies a model call before preparation binds it to a destination.
// Copies share messages, tools, and settings; Prepare snapshots these values.
type Input struct {
	// Model is the requested model name within the configured provider.
	Model string `json:"model"`

	// Stream selects transport during Prepare, independently of observation.
	Stream bool `json:"stream,omitempty"`

	// System supplies call-wide instructions separately from message history.
	System string `json:"system,omitempty"`

	// Messages holds ordered history; callers must not mutate it during
	// preparation.
	Messages []Message `json:"messages"`

	// Tools contains uniquely named definitions available for this call.
	Tools []tool.Definition `json:"tools,omitempty"`

	// OutputSchema requests a schema-constrained final JSON object for this
	// model call. Only adapters with a qualified native route accept it.
	// Agent turns use their separate, recorded OutputSchema contract.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`

	// Settings holds requested values; Prepare snapshots them before applying
	// defaults.
	Settings Settings `json:"settings,omitzero"`

	// ContentPolicy selects any permitted loss while preparing this input.
	ContentPolicy ContentPolicy `json:"content_policy,omitzero"`
}

// Validate checks provider-independent rules. Provider-specific limits are
// checked by Client.Prepare.
func (r Input) Validate() error {
	if r.Model == "" {
		return errors.New("llm: model is required; catalog.Default names a provider's general-purpose model")
	}
	if err := r.ContentPolicy.Validate(); err != nil {
		return err
	}
	if len(r.OutputSchema) > 0 {
		if _, err := outputschema.Compile(r.OutputSchema); err != nil {
			return fmt.Errorf("llm: output_schema: %w", err)
		}
	}
	if r.Settings.MaxTokens < 0 {
		return errors.New("llm: max tokens cannot be negative")
	}
	if t := r.Settings.Temperature; t != nil &&
		(math.IsNaN(*t) ||
			math.IsInf(
				*t,
				0,
			) || *t < 0) {
		return errors.New("llm: invalid temperature")
	}
	if err := r.Settings.PromptCache.validate(); err != nil {
		return err
	}
	if s := r.Settings.Reasoning; s != nil && !validReasoning(s) {
		return errors.New("llm: choose reasoning effort or token budget")
	}
	names := map[string]bool{}
	for _, d := range r.Tools {
		if err := d.Validate(); err != nil {
			return err
		}
		if names[d.Name] {
			return fmt.Errorf("llm: duplicate tool %q", d.Name)
		}
		names[d.Name] = true
	}
	if c := r.Settings.ToolChoice; c != nil {
		switch c.Mode {
		case ToolChoiceAuto, ToolChoiceAny, ToolChoiceNone:
			if c.Name != "" {
				return errors.New("llm: named tool choice requires tool mode")
			}
		case ToolChoiceTool:
			if !names[c.Name] {
				return errors.New("llm: selected tool is not defined")
			}
		default:
			return fmt.Errorf(
				"llm: invalid tool choice %q",
				c.Mode,
			)
		}
	}
	for _, m := range r.Messages {
		if m.Context != nil && (m.Context.Origin == "" || m.Context.Name == "") {
			return errors.New("llm: message context requires origin and name")
		}
		switch m.Role {
		case RoleUser, RoleAssistant, RoleSystem, RoleDeveloper:
		default:
			return fmt.Errorf("llm: invalid role %q", m.Role)
		}
		for _, c := range m.Content {
			if err := c.Validate(); err != nil {
				return err
			}
			if c.Kind == ContentReminder &&
				(m.Role != RoleUser ||
					len(m.Content) != 1) {
				return errors.New("llm: reminder requires its own user-role message")
			}
			for _, o := range []*Opaque{c.Replay, c.Opaque} {
				if o != nil && o.Partial {
					return errors.New("llm: incomplete provider data cannot be replayed")
				}
				if o != nil && o.Version != 1 {
					return fmt.Errorf(
						"llm: unsupported opaque wrapper version %d",
						o.Version,
					)
				}
			}
		}
	}
	return nil
}

// Stop explains why a response ended. Kind is what an agent acts on; Raw keeps
// the provider's value for logging and for reasons Dashi does not recognize.
type Stop struct {
	// Kind groups provider reasons for execution decisions.
	Kind StopKind `json:"kind"`

	// Raw preserves the provider reason even when Kind cannot classify it.
	Raw string `json:"raw"`
}

// Result is the outcome of one model call, identical whether streamed or not.
// Copies share message payloads, output, usage, cost, and raw bytes.
// Message.Clone and Usage.Copy detach their values for independent editing.
type Result struct {
	// ID is the provider-reported response identifier, when available.
	ID string `json:"id,omitempty"`

	// Message holds accumulated output; inspect Stop before executing any tool
	// calls.
	Message Message `json:"message"`

	// Output is a locally schema-validated final JSON object when this call
	// requested OutputSchema and finished successfully. Nil means no validated
	// value, including refusals, output limits, and partial results.
	Output json.RawMessage `json:"output,omitempty"`

	// Stop distinguishes completion, interruption, limits, and other outcomes.
	Stop Stop `json:"stop"`

	// Usage is nil when the provider did not report it, which is not the same
	// as zero usage.
	Usage *Usage `json:"usage,omitempty"`

	// Cost is a provider-reported amount or explicit list-price estimate for
	// this attempt. Nil means unknown, including when usage is known.
	Cost *Cost `json:"cost,omitempty"`

	// ServiceTier is the provider-reported serving tier when available. A
	// caller must know the tier before selecting a tier-specific price sheet.
	ServiceTier string `json:"service_tier,omitempty"`

	// Requested records the configured provider and requested model name.
	Requested ModelRef `json:"requested"`

	// Served retains that provider and the reported model, empty when unknown;
	// it does not identify a proxy's upstream.
	Served ModelRef `json:"served"`

	// Warnings repeats the prepared request's warnings, so a caller holding
	// only the result can still report what was not sent as asked.
	Warnings []Warning `json:"warnings,omitempty"`

	// Raw is the provider's response body, kept for debugging and fidelity.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Delta is a provisional streaming observation. Kind is one of "text",
// "reasoning", "tool_arguments", "content", or "usage". Index is the position
// of the provider's output item or block; ContentIndex locates a part within
// that item for providers that nest them.
type Delta struct {
	// Kind determines how to interpret this provisional observation.
	Kind DeltaKind `json:"kind"`

	// Index locates the provider output item or block.
	Index int `json:"index"`

	// ContentIndex locates a nested part within Index when the API has message
	// parts.
	ContentIndex int `json:"content_index,omitempty"`

	// Text holds a text or argument fragment; argument fragments may be invalid
	// JSON.
	Text string `json:"text,omitempty"`

	// Content optionally carries a content snapshot isolated from the final
	// result.
	Content *Content `json:"content,omitempty"`

	// Usage optionally carries an accounting snapshot isolated from the final
	// result.
	Usage *Usage `json:"usage,omitempty"`
}

// Sink observes deltas. Send must invoke it synchronously and stop invoking it
// before returning. It never selects the transport mode; Input.Stream does. A
// nil sink discards previews while Send still collects the result.
type Sink func(Delta)

// Client prepares model input offline and sends a request without retries.
// Its caller owns attempt identity, recording, and retry decisions, and passes
// that identity to Send in Call.Identity. Deltas are
// provisional; inspect the final result and stop before executing tool calls.
// An error may accompany partial data or leave provider receipt uncertain.
type Client interface {
	// Provider returns the application's stable name for this destination.
	Provider() string

	// API returns the wire format independently of provider identity.
	API() string

	// ModelInfo returns owned model knowledge: the application's configured
	// entry, or else the built-in catalog's documented facts when this
	// client's endpoint is one the catalog covers. False means unknown.
	ModelInfo(string) (ModelInfo, bool)

	// Prepare validates the full input and freezes a request without network
	// access. Preparation does not establish dispatch or provider receipt.
	Prepare(context.Context, Input) (Request, error)

	// Send performs at most one dispatch of call.Request and collects a
	// result, reporting deltas to call.Sink. Calling Send again is a new
	// attempt; a failure may occur before dispatch. See Attempt for what
	// call.Attempt guarantees when it is set.
	//
	// Ending the context must stop generation at the provider, not only stop
	// reading it: close the stream or cancel the remote job. Dashi cancels a
	// call when a person interrupts it and charges it as sent, so output
	// generated after the cancel is paid for and never seen.
	Send(context.Context, Call) (Result, error)
}

// ErrorCategory classifies provider failures so callers can decide whether to
// retry, fix the request, or give up without parsing provider-specific codes.
type ErrorCategory string

// Error categories.
const (
	// ErrorInvalidRequest identifies a request that failed validation or was
	// rejected.
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	// ErrorUnsupported identifies a feature the client cannot accept or verify.
	ErrorUnsupported ErrorCategory = "unsupported"
	// ErrorAuthentication identifies an authentication or access rejection.
	ErrorAuthentication ErrorCategory = "authentication"
	// ErrorRateLimited identifies a rate or quota rejection; inspect Code and
	// Retryable.
	ErrorRateLimited ErrorCategory = "rate_limited"
	// ErrorOverloaded identifies a provider-side failure reported by HTTP
	// status.
	ErrorOverloaded ErrorCategory = "overloaded"
	// ErrorTransport indicates failed communication, possibly after a remote
	// effect.
	ErrorTransport ErrorCategory = "transport"
	// ErrorProtocol indicates a response that cannot be interpreted as a valid
	// result.
	ErrorProtocol ErrorCategory = "protocol"
)

// ErrorReason names a specific, actionable cause within an ErrorCategory. It
// refines Category rather than replacing it, so code that switches on Category
// is unaffected. The empty reason means the failure was not classified.
type ErrorReason string

// Error reasons.
const (
	// ReasonContextWindowExceeded identifies a request whose input, together
	// with any requested output budget, does not fit the model's context
	// window. Shortening the history, for example by compacting it, and
	// sending again may succeed.
	ReasonContextWindowExceeded ErrorReason = "context_window_exceeded"
	// ReasonMediaRejected identifies attached media the provider would not
	// accept: an unsupported type, a file or image that is too large or
	// cannot be decoded, or too many items. Removing or converting the media
	// and sending again may succeed.
	ReasonMediaRejected ErrorReason = "media_rejected"
	// ReasonResponseStartTimeout means the Dashi response-start timer expired
	// before the provider's response headers arrived.
	ReasonResponseStartTimeout ErrorReason = "response_start_timeout"
	// ReasonStreamIdleTimeout means no response-body bytes arrived during one
	// blocking read of a model stream.
	ReasonStreamIdleTimeout ErrorReason = "stream_idle_timeout"
	// ReasonCallTimeout means Dashi's total model-call timer expired.
	ReasonCallTimeout ErrorReason = "call_timeout"
)

// ErrNoAPIKey reports a client built without an API key for an endpoint that
// needs one. Construction and Prepare still succeed, so offline work such as
// tests and cost estimates needs no key; Send fails before dispatch with an
// *Error of category ErrorAuthentication that wraps ErrNoAPIKey and names the
// environment variables the constructor read.
var ErrNoAPIKey = errors.New("llm: no API key")

// Error is a categorized provider failure. Status, Code, and Body carry the
// provider's response when there was one; RetryAfter is the provider's hint,
// or zero.
type Error struct {
	// Provider identifies the configured provider associated with the failure.
	Provider string

	// Category allows decisions without parsing provider-specific error text.
	Category ErrorCategory
	// Reason refines Category when a provider reports a failure Dashi
	// recognizes; such failures keep the category the provider's response
	// implies, usually ErrorInvalidRequest. Classification is best effort:
	// empty means unclassified, not that a reason is ruled out. Which
	// provider codes, statuses, and messages are recognized is an
	// implementation detail that may change as providers change.
	Reason ErrorReason
	// SendStatus records what is known about this attempt's remote effect.
	// An absent value is unknown and must not authorize a retry.
	SendStatus SendStatus

	// Status is the HTTP status when a response was received, or zero.
	Status int

	// Code retains the provider error code, which may refine retry advice.
	Code string
	// Retryable is a hint from an explicit HTTP rejection, not permission to
	// retry an uncertain effect. The caller owns recording and retry policy.
	Retryable bool

	// RetryAfter preserves the provider delay hint; zero means none was
	// supplied.
	RetryAfter time.Duration

	// Body retains available error response bytes; copying Error shares them.
	Body []byte

	// Cause preserves the underlying error for errors.Is and errors.As.
	Cause error
}

// SendStatus is evidence about a failed send, independent of whether
// the error is retryable under a caller's policy.
type SendStatus string

const (
	// SendStatusNotSent means the request did not leave the client.
	SendStatusNotSent SendStatus = "not_sent"
	// SendStatusProviderRejected means the provider explicitly rejected the request.
	SendStatusProviderRejected SendStatus = "provider_rejected"
	// SendStatusUnknown means dispatch or acceptance could not be established.
	SendStatusUnknown SendStatus = "unknown"
)

// Error includes provider identity, category, and any reason while retaining
// cause text.
func (e *Error) Error() string {
	category := string(e.Category)
	if e.Reason != "" {
		category += " (" + string(e.Reason) + ")"
	}
	if e.Cause != nil {
		return fmt.Sprintf(
			"llm: %s: %s: %v",
			e.Provider,
			category,
			e.Cause,
		)
	}
	return fmt.Sprintf(
		"llm: %s: %s (HTTP %d, %s)",
		e.Provider,
		category,
		e.Status,
		e.Code,
	)
}

// IsContextWindowExceeded reports whether err wraps an *Error whose Reason is
// ReasonContextWindowExceeded.
func IsContextWindowExceeded(err error) bool {
	return hasReason(err, ReasonContextWindowExceeded)
}

// IsMediaRejected reports whether err wraps an *Error whose Reason is
// ReasonMediaRejected.
func IsMediaRejected(err error) bool {
	return hasReason(err, ReasonMediaRejected)
}

func hasReason(err error, reason ErrorReason) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Reason == reason
}

// Unwrap exposes the original failure to errors.Is and errors.As.
func (e *Error) Unwrap() error {
	return e.Cause
}

// validReasoning reports whether s selects exactly one of effort or a
// non-negative token budget.
func validReasoning(s *ReasoningSettings) bool {
	if s.BudgetTokens < 0 {
		return false
	}
	return (s.BudgetTokens > 0) != (s.Effort != "")
}

func count(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}
