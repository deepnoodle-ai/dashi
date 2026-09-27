// Package llm defines one model call, independent of agent and session state.
// Agent loops, sessions, and recording build on this package; nothing here
// knows about turns, so every caller (normal execution, compaction, child
// work) goes through the same Prepare and Send pipeline.
// Usage, pricing, and stop classification are adapted from Dive (65c393f),
// Apache-2.0; the request and preparation contracts are defined by Kernel.
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

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/tool"
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
	// ContentHostedUse reports a provider-executed call, never a local call.
	ContentHostedUse ContentKind = "hosted_use"
	// ContentHostedResult reports a separate provider-executed result block.
	ContentHostedResult ContentKind = "hosted_result"
	// ContentHostedApprovalRequest asks for an application decision.
	ContentHostedApprovalRequest ContentKind = "hosted_approval_request"
	// ContentHostedApprovalResponse records that application decision.
	ContentHostedApprovalResponse ContentKind = "hosted_approval_response"
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

	// HostedUse is provider activity and cannot be executed locally.
	HostedUse *HostedUse `json:"hosted_use,omitempty"`

	// HostedResult is provider activity paired with HostedUse.
	HostedResult *HostedResult `json:"hosted_result,omitempty"`

	// HostedApprovalRequest requires an explicit application response.
	HostedApprovalRequest *HostedApprovalRequest `json:"hosted_approval_request,omitempty"`

	// HostedApprovalResponse carries the application decision.
	HostedApprovalResponse *HostedApprovalResponse `json:"hosted_approval_response,omitempty"`

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

// Block wraps a block without cloning its nested data. Use Content.Clone before
// independently modifying a copy.
func Block(block content.Block) Content {
	return Content{Kind: ContentBlock, Block: &block}
}

// Validate checks a recordable content value with exactly one matching payload.
// Partial provider data is valid here; Input.Validate prohibits its replay.
func (c Content) Validate() error {
	if count(c.Block != nil, c.ToolCall != nil, c.ToolResult != nil,
		c.HostedUse != nil, c.HostedResult != nil,
		c.HostedApprovalRequest != nil, c.HostedApprovalResponse != nil,
		c.Reasoning != nil, c.Refusal != nil, c.Reminder != nil, c.Opaque != nil) != 1 {
		return errors.New("llm: content requires one payload")
	}
	if c.Replay != nil {
		if c.Kind == ContentReminder {
			return errors.New("llm: reminder cannot carry provider replay data")
		}
		if c.Kind == ContentOpaque {
			return errors.New("llm: opaque content cannot also carry replay data")
		}
		if c.Kind == ContentHostedApprovalResponse {
			return errors.New(
				"llm: hosted approval response cannot carry provider replay",
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
	case c.Kind == ContentHostedUse && c.HostedUse != nil:
		return c.HostedUse.Validate()
	case c.Kind == ContentHostedResult && c.HostedResult != nil:
		return c.HostedResult.Validate()
	case c.Kind == ContentHostedApprovalRequest && c.HostedApprovalRequest != nil:
		return c.HostedApprovalRequest.Validate()
	case c.Kind == ContentHostedApprovalResponse && c.HostedApprovalResponse != nil:
		return c.HostedApprovalResponse.Validate()
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

// Settings are generation controls for one call. Pointer fields distinguish
// "not set, use the provider default" from an explicit zero or false.
type Settings struct {
	// MaxTokens limits output; zero leaves the limit to preparation or the
	// service.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature distinguishes an explicit zero from an omitted sampling
	// setting.
	Temperature *float64 `json:"temperature,omitempty"`

	// Reasoning optionally selects an effort level or token budget.
	Reasoning *ReasoningSettings `json:"reasoning,omitempty"`

	// ToolChoice optionally constrains which tool the model may call.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// ParallelTools distinguishes explicitly disabled parallel calls from an
	// omitted setting.
	ParallelTools *bool `json:"parallel_tools,omitempty"`
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

	// Settings holds requested values; Prepare snapshots them before applying
	// defaults.
	Settings Settings `json:"settings,omitempty"`

	// ContentPolicy selects any permitted loss while preparing this input.
	ContentPolicy ContentPolicy `json:"content_policy,omitzero"`
}

// Validate checks provider-independent rules. Provider-specific limits are
// checked by Client.Prepare.
func (r Input) Validate() error {
	if r.Model == "" {
		return errors.New("llm: model is required")
	}
	if err := r.ContentPolicy.Validate(); err != nil {
		return err
	}
	if r.Settings.MaxTokens < 0 {
		return errors.New("llm: max tokens cannot be negative")
	}
	if t := r.Settings.Temperature; t != nil && (math.IsNaN(*t) || math.IsInf(*t, 0) || *t < 0) {
		return errors.New("llm: invalid temperature")
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
			return fmt.Errorf("llm: invalid tool choice %q", c.Mode)
		}
	}
	for _, m := range r.Messages {
		switch m.Role {
		case RoleUser, RoleAssistant, RoleSystem, RoleDeveloper:
		default:
			return fmt.Errorf("llm: invalid role %q", m.Role)
		}
		for _, c := range m.Content {
			if err := c.Validate(); err != nil {
				return err
			}
			if c.Kind == ContentReminder && (m.Role != RoleUser || len(m.Content) != 1) {
				return errors.New("llm: reminder requires its own user-role message")
			}
			for _, o := range []*Opaque{c.Replay, c.Opaque} {
				if o != nil && o.Partial {
					return errors.New("llm: incomplete provider data cannot be replayed")
				}
				if o != nil && o.Version != 1 {
					return fmt.Errorf("llm: unsupported opaque wrapper version %d", o.Version)
				}
			}
		}
	}
	return nil
}

// Stop explains why a response ended. Kind is what an agent acts on; Raw keeps
// the provider's value for logging and for reasons Kernel does not recognize.
type Stop struct {
	// Kind groups provider reasons for execution decisions.
	Kind StopKind `json:"kind"`

	// Raw preserves the provider reason even when Kind cannot classify it.
	Raw string `json:"raw"`
}

// Result is the outcome of one model call, identical whether streamed or not.
// Copies share message payloads, usage, cost, and raw bytes. Message.Clone and
// Usage.Copy detach the corresponding values for independent editing.
type Result struct {
	// ID is the provider-reported response identifier, when available.
	ID string `json:"id,omitempty"`

	// Message holds accumulated output; inspect Stop before executing any tool
	// calls.
	Message Message `json:"message"`

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
// Its caller owns attempt identity, recording, and retry decisions. Deltas are
// provisional; inspect the final result and stop before executing tool calls.
// An error may accompany partial data or leave provider receipt uncertain.
type Client interface {
	// Provider returns the application's stable name for this destination.
	Provider() string

	// API returns the wire format independently of provider identity.
	API() string

	// ModelInfo returns owned model knowledge; false means the model is
	// unknown.
	ModelInfo(string) (ModelInfo, bool)

	// Prepare validates the full input and freezes a request without network
	// access. Preparation does not establish dispatch or provider receipt.
	Prepare(context.Context, Input) (Request, error)

	// Send performs at most one dispatch and collects a result. Calling Send
	// again is a new attempt; a failure may occur before dispatch.
	Send(context.Context, Request, Sink) (Result, error)
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

// Error is a categorized provider failure. Status, Code, and Body carry the
// provider's response when there was one; RetryAfter is the provider's hint,
// or zero.
type Error struct {
	// Provider identifies the configured provider associated with the failure.
	Provider string

	// Category allows decisions without parsing provider-specific error text.
	Category ErrorCategory
	// Disposition records what is known about this attempt's remote effect.
	// An absent value is unknown and must not authorize a retry.
	Disposition FailureDisposition

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

// FailureDisposition is evidence about a failed send, independent of whether
// the error is retryable under a caller's policy.
type FailureDisposition string

const (
	FailureNotDispatched FailureDisposition = "not_dispatched"
	FailureRejected      FailureDisposition = "rejected"
	FailureUnknown       FailureDisposition = "unknown"
)

// Error includes provider identity and category while retaining cause text.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("llm: %s: %s: %v", e.Provider, e.Category, e.Cause)
	}
	return fmt.Sprintf("llm: %s: %s (HTTP %d, %s)", e.Provider, e.Category, e.Status, e.Code)
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
