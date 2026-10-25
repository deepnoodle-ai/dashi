package llm

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
)

// ContentChangeAction describes a preparation transformation, independently of
// setting adjustments. New actions require explicit implementation support.
type ContentChangeAction string

const (
	// ContentChangeStripReplay removes incompatible private data from portable
	// content.
	ContentChangeStripReplay ContentChangeAction = "strip_replay"
	// ContentChangeOmitReasoning drops a recognized reasoning block that cannot
	// be replayed to the selected destination under an explicit input policy.
	ContentChangeOmitReasoning ContentChangeAction = "omit_reasoning"
	// ContentChangeOmitProviderToolActivity records one omitted foreign
	// provider tool item: a use, a result, an approval request, or an
	// approval response.
	ContentChangeOmitProviderToolActivity ContentChangeAction = "omit_provider_tool_activity"
	// ContentChangeConvertRole records an API's representation of developer
	// authority.
	ContentChangeConvertRole ContentChangeAction = "convert_role"
	// ContentChangeEncodeToolError records a tool failure represented as visible
	// text.
	ContentChangeEncodeToolError ContentChangeAction = "encode_tool_error"
	// ContentChangeRemapToolID records a portable ID substituted for one call
	// or its matching result. Both positions carry the same mapping; the
	// original Input retains the source ID.
	ContentChangeRemapToolID ContentChangeAction = "remap_tool_id"
	// ContentChangeRenderReminder records a typed reminder rendered to tagged
	// text and the authority selected for this destination.
	ContentChangeRenderReminder ContentChangeAction = "render_reminder"
	// ContentChangeDetectMIMEType records MIME types detected for inline
	// bytes that had none. One change covers every such block at a position,
	// including all blocks of a tool result; Reason names the types. It is
	// recorded on every preparation, since the input keeps the empty type.
	// Extension matches are fixed; sniffed results come from
	// http.DetectContentType and could change with a Go release, so the
	// recorded Reason, not a new detection, says what a past request sent.
	ContentChangeDetectMIMEType ContentChangeAction = "detect_mime_type"
	// ContentChangeUnknownMIMEType accompanies ContentChangeDetectMIMEType
	// when detection found nothing better than content.UnknownMIMEType. It is
	// the one content change reported as a warning.
	ContentChangeUnknownMIMEType ContentChangeAction = "unknown_mime_type"
)

// ContentChange explains a transformation at an original input position. The
// original Input and exact Body remain in RequestSnapshot; saved history is
// never rewritten. Indices are zero-based, including for role conversions.
type ContentChange struct {
	// MessageIndex locates the message in the original Input.Messages.
	MessageIndex int `json:"message_index"`
	// ContentIndex locates the content in that original message.
	ContentIndex int `json:"content_index"`
	// Action is the machine-readable decision applied at this position.
	Action ContentChangeAction `json:"action"`
	// Reason explains why this API required the transformation.
	Reason string `json:"reason"`
	// FromRole and ToRole are set for role conversion or reminder rendering.
	FromRole Role `json:"from_role,omitempty"`
	// ToRole is the authority encoded by a role conversion or reminder rendering.
	ToRole Role `json:"to_role,omitempty"`
	// FromID is the original call ID for ContentChangeRemapToolID.
	FromID string `json:"from_id,omitempty"`
	// ToID is the ID used in the prepared body for that call or result.
	ToID string `json:"to_id,omitempty"`
}

// Warning reports whether this change asks the caller to fix its input, and
// describes it. Only an unknown MIME type does; other changes are recorded
// adaptations.
func (c ContentChange) Warning() (Warning, bool) {
	if c.Action != ContentChangeUnknownMIMEType {
		return Warning{}, false
	}
	return Warning{
		Code: WarningMIMETypeUnknown,
		Message: fmt.Sprintf(
			"message %d content %d: %s",
			c.MessageIndex,
			c.ContentIndex,
			c.Reason,
		),
	}, true
}

func validateContentChanges(p RequestSnapshot) error {
	type position struct {
		message, content int
		action           ContentChangeAction
	}
	seen := map[position]bool{}
	for _, change := range p.ContentChanges {
		if change.MessageIndex < 0 ||
			change.MessageIndex >= len(p.Input.Messages) {
			return errors.New("llm: content change requires an original message index")
		}
		m := p.Input.Messages[change.MessageIndex]
		if change.ContentIndex < 0 ||
			change.ContentIndex >= len(m.Content) ||
			change.Reason == "" {
			return errors.New("llm: content change requires an original content index and reason")
		}
		key := position{
			change.MessageIndex,
			change.ContentIndex,
			change.Action,
		}
		if seen[key] {
			return errors.New("llm: duplicate content change")
		}
		seen[key] = true
		c := m.Content[change.ContentIndex]
		if change.Action != ContentChangeConvertRole &&
			change.Action != ContentChangeRenderReminder &&
			(change.FromRole != "" ||
				change.ToRole != "") {
			return errors.New("llm: role fields require a role conversion")
		}
		if change.Action != ContentChangeRemapToolID &&
			(change.FromID != "" || change.ToID != "") {
			return errors.New("llm: tool ID fields require a tool ID remap")
		}
		switch change.Action {
		case ContentChangeStripReplay:
			if c.Replay == nil ||
				c.Replay.Origin == p.Origin ||
				(c.Kind != ContentBlock &&
					c.Kind != ContentToolCall) {
				return errors.New("llm: replay strip requires incompatible data on portable content")
			}
		case ContentChangeOmitReasoning:
			if p.Input.ContentPolicy.UnreplayableReasoning != ReasoningOmit ||
				c.Kind != ContentReasoning ||
				c.Reasoning == nil ||
				m.Role != RoleAssistant ||
				(c.Replay != nil &&
					c.Replay.Origin == p.Origin) ||
				seen[position{
					change.MessageIndex,
					change.ContentIndex,
					ContentChangeStripReplay,
				}] {
				return errors.New("llm: reasoning omission requires explicit policy and an unreplayable original block")
			}
		case ContentChangeOmitProviderToolActivity:
			// An approval response carries no replay; its own origin
			// names the destination that asked for the decision.
			foreignResponse := c.ProviderToolApprovalResponse != nil &&
				c.ProviderToolApprovalResponse.Origin != p.Origin &&
				m.Role == RoleUser
			foreignOutput := (c.ProviderToolUse != nil ||
				c.ProviderToolResult != nil ||
				c.ProviderToolApprovalRequest != nil) &&
				c.Replay != nil &&
				c.Replay.Origin != p.Origin &&
				m.Role == RoleAssistant
			if p.Input.ContentPolicy.ForeignProviderToolActivity != ProviderToolOmit ||
				(!foreignResponse && !foreignOutput) {
				return errors.New(
					"llm: provider tool omission requires explicit policy and foreign replay",
				)
			}
		case ContentChangeConvertRole:
			if m.Role != RoleDeveloper ||
				change.FromRole != m.Role ||
				change.ToRole != RoleSystem {
				return errors.New("llm: unsupported recorded role conversion")
			}
		case ContentChangeRenderReminder:
			if c.Kind != ContentReminder ||
				c.Reminder == nil ||
				m.Role != RoleUser ||
				len(m.Content) != 1 ||
				change.FromRole != RoleUser ||
				change.ToRole != ReminderPreparedRole(
					*c.Reminder,
					p.Origin,
				) {
				return errors.New("llm: invalid reminder rendering decision")
			}
		case ContentChangeDetectMIMEType, ContentChangeUnknownMIMEType:
			if !hasUntypedInline(c) {
				return errors.New(
					"llm: MIME detection requires inline data without a type",
				)
			}
		case ContentChangeEncodeToolError:
			if c.ToolResult == nil || !c.ToolResult.IsError {
				return errors.New("llm: tool error encoding requires a failed tool result")
			}
		case ContentChangeRemapToolID:
			if change.FromID == "" ||
				change.FromID == change.ToID ||
				!utf8.ValidString(change.FromID) ||
				!portableToolID(change.ToID) {
				return errors.New(
					"llm: tool ID remap requires distinct original and portable IDs",
				)
			}
			if (m.Role != RoleAssistant ||
				c.Kind != ContentToolCall ||
				c.ToolCall.ID != change.FromID) &&
				(m.Role != RoleUser ||
					c.Kind != ContentToolResult ||
					c.ToolResult.CallID != change.FromID) {
				return errors.New(
					"llm: tool ID remap requires a matching call or result",
				)
			}
		default:
			return errors.New("llm: unsupported content change action")
		}
	}
	return validateToolIDRemaps(p)
}

// A remap record constrains the whole exchange: a changed call and result
// must agree, and the new ID cannot name an unchanged call.
func validateToolIDRemaps(p RequestSnapshot) error {
	type position struct{ message, content int }
	decisions := map[position]ContentChange{}
	for _, change := range p.ContentChanges {
		if change.Action == ContentChangeRemapToolID {
			decisions[position{
				change.MessageIndex,
				change.ContentIndex,
			}] = change
		}
	}
	if len(decisions) == 0 {
		return nil
	}

	type exchange struct {
		call, result     bool
		callTo, resultTo string
	}
	exchanges := map[string]exchange{}
	callSeen := map[string]bool{}
	resultSeen := map[string]bool{}
	for mi, message := range p.Input.Messages {
		for ci, part := range message.Content {
			decision, remapped := decisions[position{mi, ci}]
			switch part.Kind {
			case ContentToolCall:
				id := part.ToolCall.ID
				if !utf8.ValidString(id) || callSeen[id] {
					return errors.New(
						"llm: tool ID remap requires unique UTF-8 source calls",
					)
				}
				callSeen[id] = true
				if remapped {
					exchange := exchanges[id]
					exchange.call = true
					exchange.callTo = decision.ToID
					exchanges[id] = exchange
				}
			case ContentToolResult:
				id := part.ToolResult.CallID
				if !utf8.ValidString(id) || resultSeen[id] {
					return errors.New(
						"llm: tool ID remap requires unique UTF-8 source results",
					)
				}
				if !callSeen[id] {
					return errors.New(
						"llm: tool ID remap requires results after calls",
					)
				}
				resultSeen[id] = true
				if remapped {
					exchange := exchanges[id]
					exchange.result = true
					exchange.resultTo = decision.ToID
					exchanges[id] = exchange
				}
			}
		}
	}

	targets := map[string]bool{}
	for id, exchange := range exchanges {
		if !exchange.call || !exchange.result ||
			exchange.callTo != exchange.resultTo {
			return errors.New(
				"llm: tool ID remap requires matching call and result decisions",
			)
		}
		if targets[exchange.callTo] {
			return errors.New(
				"llm: tool ID remap target collides with another mapping",
			)
		}
		targets[exchange.callTo] = true
		if !resultSeen[id] {
			return errors.New("llm: tool ID remap requires a paired result")
		}
	}
	for id := range callSeen {
		if _, remapped := exchanges[id]; !remapped && targets[id] {
			return errors.New(
				"llm: tool ID remap target collides with a retained call ID",
			)
		}
	}
	return nil
}

// hasUntypedInline reports whether c carries inline bytes without a MIME
// type, directly or in a tool result.
func hasUntypedInline(c Content) bool {
	var blocks []content.Block
	switch {
	case c.Block != nil:
		blocks = []content.Block{*c.Block}
	case c.ToolResult != nil:
		blocks = c.ToolResult.Output.Content
	}
	for _, b := range blocks {
		var s *content.Source
		switch {
		case b.Image != nil:
			s = &b.Image.Source
		case b.Document != nil:
			s = &b.Document.Source
		case b.Audio != nil:
			s = &b.Audio.Source
		case b.Video != nil:
			s = &b.Video.Source
		}
		if s != nil && s.Data != nil && s.MIMEType == "" {
			return true
		}
	}
	return false
}

func portableToolID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
