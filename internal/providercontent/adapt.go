// Package providercontent adapts selected history during offline preparation.
// It owns shared replay, reference, and pairing checks; it does not dispatch,
// fetch files, rewrite saved history, or synthesize tool outcomes.
package providercontent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// AdaptOptions describes a destination capability that cannot be inferred from
// API alone. Chat Completions plaintext reasoning support is provider-specific.
type AdaptOptions struct {
	// ReplayPlainReasoning confirms that this destination can encode
	// caller-authored reasoning without a private replay attachment.
	ReplayPlainReasoning bool
}

// Adapt returns owned messages and decisions at original input positions.
// Validate input before calling Adapt.
// Provider encoders remain responsible for representing canonical values.
func Adapt(
	input llm.Input,
	origin content.Origin,
	options ...AdaptOptions,
) (llm.Input, []llm.ContentChange, error) {
	if len(options) > 1 {
		return llm.Input{}, nil, errors.New(
			"providercontent: one adaptation option is supported",
		)
	}
	var capability AdaptOptions
	if len(options) > 0 {
		capability = options[0]
	}
	remappedIDs := toolIDRemaps(input)
	var omittableProviderTool map[providerToolPosition]bool
	if input.ContentPolicy.ForeignProviderToolActivity == llm.ProviderToolOmit {
		omittableProviderTool = completedForeignProviderTool(input, origin)
	}
	out := input
	out.Messages = make([]llm.Message, 0, len(input.Messages))
	var changes []llm.ContentChange
	omittedReasoning := false
	omittedProviderTool := false
	// omittedMessage is set while messages emptied by omission separate the
	// last kept message from the next one.
	omittedMessage := false
	usableHistory := false
	calls := map[string]bool{}
	pending := map[string]bool{}
	providerTool := newProviderToolHistory()
	for mi, original := range input.Messages {
		m := original.Clone()
		kept := make([]llm.Content, 0, len(m.Content))
		for ci := range m.Content {
			c := &m.Content[ci]
			fail := func(reason string) (llm.Input, []llm.ContentChange, error) {
				return llm.Input{}, nil, &llm.Error{
					Provider: origin.Provider,
					Category: llm.ErrorUnsupported,
					Cause: fmt.Errorf(
						"providercontent: message %d content %d: %s",
						mi,
						ci,
						reason,
					),
				}
			}
			record := func(action llm.ContentChangeAction, reason string) {
				changes = append(
					changes,
					llm.ContentChange{
						MessageIndex: mi,
						ContentIndex: ci,
						Action:       action,
						Reason:       reason,
					},
				)
			}
			remapID := func(id *string) {
				toID, ok := remappedIDs[*id]
				if !ok {
					return
				}
				fromID := *id
				*id = toID
				changes = append(
					changes,
					llm.ContentChange{
						MessageIndex: mi,
						ContentIndex: ci,
						Action:       llm.ContentChangeRemapToolID,
						Reason:       "tool ID is outside Dashi's portable subset",
						FromID:       fromID,
						ToID:         toID,
					},
				)
			}
			if c.ToolCall != nil {
				remapID(&c.ToolCall.ID)
			}
			if c.ToolResult != nil {
				remapID(&c.ToolResult.CallID)
			}
			if omittableProviderTool[providerToolPosition{mi, ci}] {
				record(
					llm.ContentChangeOmitProviderToolActivity,
					"completed provider tool activity belongs to another destination",
				)
				omittedProviderTool = true
				continue
			}
			if err := checkReferences(*c, origin); err != nil {
				return fail(err.Error())
			}
			if c.Opaque != nil && c.Opaque.Origin != origin {
				return fail("opaque content requires its original destination")
			}
			if c.ProviderToolUse != nil || c.ProviderToolResult != nil ||
				c.ProviderToolApprovalRequest != nil ||
				c.ProviderToolApprovalResponse != nil {
				if err := providerTool.check(
					*c,
					m.Role,
					origin,
					mi,
				); err != nil {
					return fail(err.Error())
				}
				kept = append(kept, *c)
				continue
			}
			if c.Kind == llm.ContentReminder {
				if m.Role != llm.RoleUser ||
					len(m.Content) != 1 {
					return fail("reminder requires its own user-role message")
				}
				reminder := *c.Reminder
				m.Role = llm.ReminderPreparedRole(
					reminder,
					origin,
				)
				*c = llm.Text(llm.FormatReminder(reminder))
				reason := "typed runtime context rendered for destination"
				if reminder.Tier == llm.ReminderOperator &&
					m.Role == llm.RoleUser {
					reason = "destination cannot preserve operator authority at this history position; rendered as user context"
				}
				if llm.ReminderEscapes(reminder) {
					reason += "; reminder tags in content escaped"
				}
				changes = append(
					changes,
					llm.ContentChange{
						MessageIndex: mi,
						ContentIndex: ci,
						Action:       llm.ContentChangeRenderReminder,
						Reason:       reason,
						FromRole:     llm.RoleUser,
						ToRole:       m.Role,
					},
				)
			}
			if c.Kind == llm.ContentReasoning {
				if m.Role != llm.RoleAssistant {
					return fail("reasoning requires assistant role")
				}
				plainReplay := c.Replay == nil &&
					origin.API == "openai.completions" &&
					capability.ReplayPlainReasoning
				unreplayable := !plainReplay &&
					(c.Replay == nil ||
						c.Replay.Origin != origin ||
						origin.API == "openai.completions")
				if unreplayable {
					if input.ContentPolicy.UnreplayableReasoning != llm.ReasoningOmit {
						return fail(
							"reasoning requires its original destination or an explicit omission policy",
						)
					}
					if c.Replay != nil {
						if err := checkReasoningReplay(*c); err != nil {
							return fail(err.Error())
						}
					}
					record(
						llm.ContentChangeOmitReasoning,
						"reasoning cannot be replayed to this destination",
					)
					omittedReasoning = true
					continue
				}
			}
			if c.Replay != nil {
				foreign := c.Replay.Origin != origin
				if err := checkReplay(
					*c,
					original.Content[ci],
					m.Role,
					foreign,
				); err != nil {
					return fail(err.Error())
				}
				if foreign {
					c.Replay = nil
					record(
						llm.ContentChangeStripReplay,
						"incompatible replay origin; recognized canonical content is portable",
					)
				}
			}
			if c.ToolCall != nil {
				id := original.Content[ci].ToolCall.ID
				if m.Role != llm.RoleAssistant ||
					calls[id] ||
					id == "" ||
					!utf8.ValidString(id) {
					return fail("tool call requires assistant role and a unique nonempty UTF-8 ID")
				}
				calls[id], pending[id] = true, true
			}
			if c.ToolResult != nil {
				id := original.Content[ci].ToolResult.CallID
				if m.Role != llm.RoleUser ||
					!pending[id] ||
					!utf8.ValidString(id) {
					return fail("tool result requires user role and an unanswered earlier call")
				}
				delete(pending, id)
				if c.ToolResult.IsError {
					switch origin.API {
					case "openai.responses":
						record(
							llm.ContentChangeEncodeToolError,
							"Responses represents the tool error flag with a visible failure prefix",
						)
					case "openai.completions":
						record(
							llm.ContentChangeEncodeToolError,
							"Chat Completions represents the tool error flag with a visible failure prefix",
						)
					}
				}
			}
			if m.Role == llm.RoleDeveloper &&
				origin.API == "anthropic.messages" {
				record(
					llm.ContentChangeConvertRole,
					"Messages encodes developer instructions as system content",
				)
				changes[len(changes)-1].FromRole = llm.RoleDeveloper
				changes[len(changes)-1].ToRole = llm.RoleSystem
			}
			if reason, unknown := detectMIMETypes(c); reason != "" {
				record(llm.ContentChangeDetectMIMEType, reason)
				if unknown {
					record(
						llm.ContentChangeUnknownMIMEType,
						"inline data could not be identified and was sent as "+
							content.UnknownMIMEType+"; set Source.MIMEType",
					)
				}
			}
			kept = append(kept, *c)
		}
		switch {
		case len(kept) == 0 && len(original.Content) > 0:
			// Omission emptied this message, so it leaves the wire history.
			omittedMessage = true
		case omittedMessage && joinable(out.Messages, m.Role, kept, origin):
			// Dropping the messages between them left two user or two
			// assistant messages in a row. Join them so the roles still
			// alternate, as Messages and Gemini expect. The earlier
			// message's Source and Context stay; encoders send neither.
			last := &out.Messages[lastTurn(out.Messages, origin)]
			last.Content = append(last.Content, kept...)
			omittedMessage = false
		default:
			m.Content = kept
			out.Messages = append(out.Messages, m)
			// Messages moves system and developer content out of the
			// history, so it does not separate the messages around it.
			omittedMessage = omittedMessage && hoistsSystem(origin) &&
				(m.Role == llm.RoleSystem || m.Role == llm.RoleDeveloper)
		}
		// Messages moves system and developer content into top-level system;
		// those blocks cannot make an otherwise empty messages array usable.
		if len(kept) > 0 &&
			(origin.API != "anthropic.messages" ||
				m.Role == llm.RoleUser ||
				m.Role == llm.RoleAssistant) {
			usableHistory = true
		}
	}
	if (omittedReasoning || omittedProviderTool) && !usableHistory {
		return llm.Input{}, nil, &llm.Error{
			Provider: origin.Provider,
			Category: llm.ErrorUnsupported,
			Cause: errors.New(
				"providercontent: content omission would leave no usable input",
			),
		}
	}
	if len(pending) != 0 {
		return llm.Input{}, nil, &llm.Error{
			Provider: origin.Provider,
			Category: llm.ErrorUnsupported,
			Cause: fmt.Errorf(
				"providercontent: history has %d unresolved tool calls",
				len(pending),
			),
		}
	}
	for _, from := range providerTool.pending {
		if !providerToolTail(input.Messages, from) {
			return llm.Input{}, nil, &llm.Error{
				Provider: origin.Provider,
				Category: llm.ErrorUnsupported,
				Cause: errors.New(
					"providercontent: unfinished provider tool call requires a paused or mixed-tool tail",
				),
			}
		}
	}
	return out, changes, nil
}

// joinable reports whether content for role may join the last kept user or
// assistant message. Only user and assistant messages join. A message with
// tool results stays separate: Chat Completions sends results as their own
// tool messages and rejects a user message that mixes them with other
// content.
func joinable(
	messages []llm.Message,
	role llm.Role,
	next []llm.Content,
	origin content.Origin,
) bool {
	i := lastTurn(messages, origin)
	if i < 0 || len(next) == 0 {
		return false
	}
	last := messages[i]
	if last.Role != role || len(last.Content) == 0 ||
		(role != llm.RoleUser && role != llm.RoleAssistant) {
		return false
	}
	return !slices.ContainsFunc(last.Content, isToolResult) &&
		!slices.ContainsFunc(next, isToolResult)
}

// lastTurn is the index of the last kept message, skipping system and
// developer messages where the destination moves them out of the history.
// Elsewhere they keep their position, and joining across them would reorder
// what the model reads.
func lastTurn(messages []llm.Message, origin content.Origin) int {
	i := len(messages) - 1
	for hoistsSystem(origin) && i >= 0 &&
		(messages[i].Role == llm.RoleSystem ||
			messages[i].Role == llm.RoleDeveloper) {
		i--
	}
	return i
}

// hoistsSystem reports whether the destination sends system and developer
// history as top-level system content.
func hoistsSystem(origin content.Origin) bool {
	return origin.API == "anthropic.messages"
}

func isToolResult(c llm.Content) bool {
	return c.ToolResult != nil
}

type providerToolHistory struct {
	calls   map[string]bool
	results map[string]bool
	pending map[string]int
	// Answered IDs stay in the map so a later request cannot reuse them.
	approvals map[string]bool
}

func newProviderToolHistory() providerToolHistory {
	return providerToolHistory{
		calls:     map[string]bool{},
		results:   map[string]bool{},
		pending:   map[string]int{},
		approvals: map[string]bool{},
	}
}

func (history *providerToolHistory) check(
	c llm.Content,
	role llm.Role,
	origin content.Origin,
	messageIndex int,
) error {
	var artifactOrigin content.Origin
	switch {
	case c.ProviderToolUse != nil:
		artifactOrigin = c.ProviderToolUse.Origin
	case c.ProviderToolResult != nil:
		artifactOrigin = c.ProviderToolResult.Origin
	case c.ProviderToolApprovalRequest != nil:
		artifactOrigin = c.ProviderToolApprovalRequest.Origin
	case c.ProviderToolApprovalResponse != nil:
		artifactOrigin = c.ProviderToolApprovalResponse.Origin
	}
	if artifactOrigin != origin {
		return errors.New("provider tool activity requires its original destination")
	}
	if c.ProviderToolApprovalResponse != nil {
		response := c.ProviderToolApprovalResponse
		if role != llm.RoleUser ||
			!history.approvals[response.RequestID] {
			return errors.New("provider tool approval requires a preceding request")
		}
		history.approvals[response.RequestID] = false
		return nil
	}
	if role != llm.RoleAssistant || c.Replay == nil ||
		c.Replay.Origin != origin || c.Replay.Partial {
		return errors.New("provider tool output requires assistant role and original replay data")
	}
	switch {
	case c.ProviderToolUse != nil:
		use := c.ProviderToolUse
		if history.calls[use.ID] {
			return errors.New("provider tool use ID must be unique")
		}
		history.calls[use.ID] = true
		if use.Status != "completed" && use.Status != "failed" {
			history.pending[use.ID] = messageIndex
		}
	case c.ProviderToolResult != nil:
		result := c.ProviderToolResult
		if !history.calls[result.CallID] ||
			history.results[result.CallID] {
			return errors.New("provider tool result requires an earlier use")
		}
		history.results[result.CallID] = true
		delete(history.pending, result.CallID)
	case c.ProviderToolApprovalRequest != nil:
		request := c.ProviderToolApprovalRequest
		if _, seen := history.approvals[request.ID]; seen {
			return errors.New("provider tool approval request ID must be unique")
		}
		history.approvals[request.ID] = true
	}
	return nil
}

// DuplicateToolID reports a tool call, provider tool use, or provider tool
// approval request whose ID an earlier one of the same kind already used.
// Adapt rejects such history, so a decoder rejects the response that would
// create it.
func DuplicateToolID(parts []llm.Content) error {
	calls := map[string]bool{}
	uses := map[string]bool{}
	approvals := map[string]bool{}
	check := func(seen map[string]bool, kind, id string) error {
		if seen[id] {
			return fmt.Errorf("duplicate %s ID %q", kind, id)
		}
		seen[id] = true
		return nil
	}
	for _, c := range parts {
		var err error
		switch {
		case c.ToolCall != nil:
			err = check(calls, "tool call", c.ToolCall.ID)
		case c.ProviderToolUse != nil:
			err = check(uses, "provider tool use", c.ProviderToolUse.ID)
		case c.ProviderToolApprovalRequest != nil:
			err = check(approvals, "provider tool approval request", c.ProviderToolApprovalRequest.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func providerToolTail(messages []llm.Message, from int) bool {
	for _, message := range messages[from+1:] {
		if message.Role != llm.RoleUser {
			return false
		}
		for _, part := range message.Content {
			if part.ToolResult == nil &&
				part.ProviderToolApprovalResponse == nil {
				return false
			}
		}
	}
	return true
}

type providerToolPosition struct {
	message int
	content int
}

type providerToolKey struct {
	origin content.Origin
	id     string
}

type providerToolPair struct {
	use    providerToolPosition
	result providerToolPosition
	status string
	valid  bool
	paired bool
}

// Omission is decided for complete exchanges before adapting any message.
// This keeps a foreign use from being dropped ahead of a later pending result,
// and a foreign approval request from being dropped while still unanswered.
func completedForeignProviderTool(
	input llm.Input,
	origin content.Origin,
) map[providerToolPosition]bool {
	pairs := map[providerToolKey]providerToolPair{}
	// An approval request pairs with its response the way a use pairs with
	// its result. Only an answered request is complete.
	approvals := map[providerToolKey]providerToolPair{}
	for mi, message := range input.Messages {
		for ci, part := range message.Content {
			position := providerToolPosition{mi, ci}
			switch {
			case part.ProviderToolApprovalRequest != nil:
				request := part.ProviderToolApprovalRequest
				if request.Origin == origin {
					continue
				}
				key := providerToolKey{request.Origin, request.ID}
				_, duplicate := approvals[key]
				approvals[key] = providerToolPair{
					use: position,
					valid: !duplicate &&
						message.Role == llm.RoleAssistant &&
						validForeignReplay(
							part.Replay,
							request.Origin,
						),
				}
			case part.ProviderToolApprovalResponse != nil:
				response := part.ProviderToolApprovalResponse
				if response.Origin == origin {
					continue
				}
				key := providerToolKey{response.Origin, response.RequestID}
				pair, found := approvals[key]
				if !found {
					continue
				}
				pair.valid = pair.valid && !pair.paired &&
					message.Role == llm.RoleUser &&
					part.Replay == nil
				pair.paired = true
				pair.result = position
				approvals[key] = pair
			case part.ProviderToolUse != nil:
				use := part.ProviderToolUse
				if use.Origin == origin {
					continue
				}
				key := providerToolKey{use.Origin, use.ID}
				_, duplicate := pairs[key]
				pairs[key] = providerToolPair{
					use:    position,
					status: use.Status,
					valid: !duplicate &&
						message.Role == llm.RoleAssistant &&
						validForeignReplay(
							part.Replay,
							use.Origin,
						),
				}
			case part.ProviderToolResult != nil:
				result := part.ProviderToolResult
				if result.Origin == origin {
					continue
				}
				key := providerToolKey{result.Origin, result.CallID}
				pair, found := pairs[key]
				if !found {
					continue
				}
				pair.valid = pair.valid && !pair.paired &&
					message.Role == llm.RoleAssistant &&
					validForeignReplay(
						part.Replay,
						result.Origin,
					)
				pair.paired = true
				pair.result = position
				pairs[key] = pair
			}
		}
	}
	omittable := map[providerToolPosition]bool{}
	for _, pair := range pairs {
		complete := pair.paired || pair.status == "completed" ||
			pair.status == "failed"
		if !pair.valid || !complete {
			continue
		}
		omittable[pair.use] = true
		if pair.paired {
			omittable[pair.result] = true
		}
	}
	for _, pair := range approvals {
		if !pair.valid || !pair.paired {
			continue
		}
		omittable[pair.use] = true
		omittable[pair.result] = true
	}
	return omittable
}

func validForeignReplay(
	replay *llm.Opaque,
	origin content.Origin,
) bool {
	return replay != nil && replay.Origin == origin && !replay.Partial
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
	if c.ProviderToolUse != nil {
		for _, b := range c.ProviderToolUse.Output {
			if err := checkBlock(b, origin); err != nil {
				return err
			}
		}
	}
	if c.ProviderToolResult != nil {
		for _, b := range c.ProviderToolResult.Output {
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
	case b.Video != nil:
		source = &b.Video.Source
	case b.Text != nil:
		for _, citation := range b.Text.Citations {
			if citation.Origin != origin ||
				len(citation.Raw) == 0 {
				return errors.New(
					"providercontent: citation requires its original replay data and destination",
				)
			}
		}
	case b.Resource != nil:
		if b.Resource.Origin != (content.Origin{}) &&
			!sameFileScope(b.Resource.Origin, origin) {
			return errors.New(
				"providercontent: resource requires its original provider, API, and scope",
			)
		}
	}
	if source != nil && (source.ID != "" || source.URI != "") &&
		!sameFileScope(source.Origin, origin) {
		return errors.New(
			"providercontent: file reference requires its original provider, API, and scope",
		)
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
	ids := slices.Sorted(maps.Keys(unsupported))
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

// portableCallID is Dashi's conservative subset, not a claim about every ID
// a provider accepts.
func portableCallID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
