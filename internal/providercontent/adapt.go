// Package providercontent adapts selected history during offline preparation.
// It owns shared replay, reference, and pairing checks; it does not dispatch,
// fetch files, rewrite saved history, or synthesize tool outcomes.
package providercontent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

// AdaptOptions describes a destination capability that cannot be inferred from
// API alone. Chat Completions plaintext reasoning support is provider-specific.
type AdaptOptions struct {
	// ReplayPlainReasoning confirms that this destination can encode
	// caller-authored reasoning without a private replay attachment.
	ReplayPlainReasoning bool
}

// Adapt returns owned messages and decisions at original input positions.
// Provider encoders remain responsible for representing canonical values.
func Adapt(input llm.Input, origin content.Origin, options ...AdaptOptions) (llm.Input, []llm.ContentChange, error) {
	if len(options) > 1 {
		return llm.Input{}, nil, errors.New("providercontent: one adaptation option is supported")
	}
	var capability AdaptOptions
	if len(options) > 0 {
		capability = options[0]
	}
	remappedIDs := toolIDRemaps(input)
	out := input
	out.Messages = make([]llm.Message, 0, len(input.Messages))
	var changes []llm.ContentChange
	omittedReasoning := false
	usableHistory := false
	calls := map[string]bool{}
	pending := map[string]bool{}
	for mi, original := range input.Messages {
		m := original.Clone()
		kept := make([]llm.Content, 0, len(m.Content))
		for ci := range m.Content {
			c := &m.Content[ci]
			fail := func(reason string) (llm.Input, []llm.ContentChange, error) {
				return llm.Input{}, nil, &llm.Error{Provider: origin.Provider, Category: llm.ErrorUnsupported,
					Cause: fmt.Errorf("providercontent: message %d content %d: %s", mi, ci, reason)}
			}
			record := func(action llm.ContentChangeAction, reason string) {
				changes = append(changes, llm.ContentChange{MessageIndex: mi, ContentIndex: ci, Action: action, Reason: reason})
			}
			remapID := func(id *string) {
				toID, ok := remappedIDs[*id]
				if !ok {
					return
				}
				fromID := *id
				*id = toID
				changes = append(changes, llm.ContentChange{
					MessageIndex: mi,
					ContentIndex: ci,
					Action:       llm.ContentChangeRemapToolID,
					Reason:       "tool ID is outside Kernel's portable subset",
					FromID:       fromID,
					ToID:         toID,
				})
			}
			if c.ToolCall != nil {
				remapID(&c.ToolCall.ID)
			}
			if c.ToolResult != nil {
				remapID(&c.ToolResult.CallID)
			}
			if err := checkReferences(*c, origin); err != nil {
				return fail(err.Error())
			}
			if c.Opaque != nil && c.Opaque.Origin != origin {
				return fail("opaque content requires its original destination")
			}
			if c.Kind == llm.ContentReasoning {
				if m.Role != llm.RoleAssistant {
					return fail("reasoning requires assistant role")
				}
				plainReplay := c.Replay == nil && origin.API == "openai.completions" && capability.ReplayPlainReasoning
				unreplayable := !plainReplay && (c.Replay == nil || c.Replay.Origin != origin || origin.API == "openai.completions")
				if unreplayable {
					if input.ContentPolicy.UnreplayableReasoning != llm.ReasoningOmit {
						return fail("reasoning requires its original destination or an explicit omission policy")
					}
					if c.Replay != nil {
						if err := checkReasoningReplay(*c); err != nil {
							return fail(err.Error())
						}
					}
					record(llm.ContentChangeOmitReasoning, "reasoning cannot be replayed to this destination")
					omittedReasoning = true
					continue
				}
			}
			if c.Replay != nil {
				foreign := c.Replay.Origin != origin
				if err := checkReplay(*c, original.Content[ci], m.Role, foreign); err != nil {
					return fail(err.Error())
				}
				if foreign {
					c.Replay = nil
					record(llm.ContentChangeStripReplay, "incompatible replay origin; recognized canonical content is portable")
				}
			}
			if c.ToolCall != nil {
				id := original.Content[ci].ToolCall.ID
				if m.Role != llm.RoleAssistant || calls[id] || id == "" || !utf8.ValidString(id) {
					return fail("tool call requires assistant role and a unique nonempty UTF-8 ID")
				}
				calls[id], pending[id] = true, true
			}
			if c.ToolResult != nil {
				id := original.Content[ci].ToolResult.CallID
				if m.Role != llm.RoleUser || !pending[id] || !utf8.ValidString(id) {
					return fail("tool result requires user role and an unanswered earlier call")
				}
				delete(pending, id)
				if c.ToolResult.IsError && origin.API == "openai.responses" {
					record(llm.ContentChangeEncodeToolError, "Responses represents the tool error flag with a visible failure prefix")
				}
			}
			if m.Role == llm.RoleDeveloper && origin.API == "anthropic.messages" {
				record(llm.ContentChangeConvertRole, "Messages encodes developer instructions as system content")
				changes[len(changes)-1].FromRole = llm.RoleDeveloper
				changes[len(changes)-1].ToRole = llm.RoleSystem
			}
			kept = append(kept, *c)
		}
		if len(kept) > 0 || len(original.Content) == 0 {
			m.Content = kept
			out.Messages = append(out.Messages, m)
		}
		// Messages moves system and developer content into top-level system;
		// those blocks cannot make an otherwise empty messages array usable.
		if len(kept) > 0 && (origin.API != "anthropic.messages" || m.Role == llm.RoleUser || m.Role == llm.RoleAssistant) {
			usableHistory = true
		}
	}
	if omittedReasoning && !usableHistory {
		return llm.Input{}, nil, &llm.Error{Provider: origin.Provider, Category: llm.ErrorUnsupported,
			Cause: errors.New("providercontent: reasoning omission would leave no usable input")}
	}
	if len(pending) != 0 {
		return llm.Input{}, nil, &llm.Error{Provider: origin.Provider, Category: llm.ErrorUnsupported,
			Cause: fmt.Errorf("providercontent: history has %d unresolved tool calls", len(pending))}
	}
	return out, changes, nil
}

func checkReferences(c llm.Content, origin content.Origin) error {
	if c.Block != nil {
		if err := checkBlock(*c.Block, origin); err != nil {
			return err
		}
	}
	if c.ToolResult != nil {
		for _, b := range c.ToolResult.Output.Content {
			if err := checkBlock(b, origin); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkBlock(b content.Block, origin content.Origin) error {
	var source *content.Source
	switch {
	case b.Image != nil:
		source = &b.Image.Source
	case b.Document != nil:
		source = &b.Document.Source
	case b.Audio != nil:
		source = &b.Audio.Source
	case b.Text != nil:
		for _, citation := range b.Text.Citations {
			if citation.Origin != origin || len(citation.Raw) == 0 {
				return errors.New("providercontent: citation requires its original replay data and destination")
			}
		}
	case b.Resource != nil:
		if b.Resource.Origin != (content.Origin{}) &&
			!sameFileScope(b.Resource.Origin, origin) {
			return errors.New("providercontent: resource requires its original provider, API, and scope")
		}
	}
	if source != nil && (source.ID != "" || source.URI != "") &&
		!sameFileScope(source.Origin, origin) {
		return errors.New("providercontent: file reference requires its original provider, API, and scope")
	}
	return nil
}

func sameFileScope(a, b content.Origin) bool {
	return a.Provider == b.Provider && a.API == b.API && a.Scope == b.Scope
}

// toolIDRemaps reserves every supported source ID before allocating any new
// one. A later original call must never be displaced by a generated ID.
func toolIDRemaps(input llm.Input) map[string]string {
	reserved := map[string]bool{}
	unsupported := map[string]bool{}
	for _, message := range input.Messages {
		for _, c := range message.Content {
			if c.ToolCall == nil || c.ToolCall.ID == "" {
				continue
			}
			id := c.ToolCall.ID
			if portableCallID(id) {
				reserved[id] = true
			} else {
				unsupported[id] = true
			}
		}
	}
	ids := make([]string, 0, len(unsupported))
	for id := range unsupported {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	remapped := make(map[string]string, len(ids))
	for _, id := range ids {
		for probe := 0; ; probe++ {
			seed := id
			if probe != 0 {
				seed += "\x00" + strconv.Itoa(probe)
			}
			hash := sha256.Sum256([]byte(seed))
			candidate := "call_" + hex.EncodeToString(hash[:16])
			if reserved[candidate] {
				continue
			}
			reserved[candidate] = true
			remapped[id] = candidate
			break
		}
	}
	return remapped
}

// portableCallID is Kernel's conservative subset, not a claim about every ID
// a provider accepts.
func portableCallID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
