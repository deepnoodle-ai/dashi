package providercontent

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// checkReplay recognizes source API shapes independently of the destination.
// These guards are deliberately closed: new private fields survive unchanged
// same-origin replay, but cannot be silently discarded or overlaid after edits.
func checkReplay(c, original llm.Content, role llm.Role, foreign bool) error {
	o := c.Replay
	if role != llm.RoleAssistant {
		return errors.New("providercontent: output replay requires assistant role")
	}
	if c.Kind == llm.ContentReasoning {
		if foreign {
			return errors.New("providercontent: reasoning requires its original destination")
		}
		return nil
	}
	if o.Origin.API == "google.generate_content" {
		return checkGeminiReplay(c, original, foreign)
	}
	fields, metadata, err := replayFields(o)
	if err != nil {
		return err
	}
	kind := stringField(fields, "type")
	if kind != o.Type {
		return errors.New("providercontent: replay type differs from its payload")
	}
	var known, edited, portable bool
	switch {
	case c.Block != nil && c.Block.Kind == content.KindText:
		key := "citations"
		switch {
		case o.Origin.API == "anthropic.messages" && kind == "text":
			known = only(fields, "type", "text", "citations")
		case o.Origin.API == "openai.responses" &&
			kind == "output_text":
			key = "annotations"
			known = only(
				fields,
				"type",
				"text",
				"annotations",
				"logprobs",
			) && empty(fields["logprobs"])
		default:
			return errors.New("providercontent: text replay has an unsupported source shape")
		}
		t := c.Block.Text
		var original string
		if err := json.Unmarshal(
			fields["text"],
			&original,
		); err != nil {
			return errors.New("providercontent: text replay requires original text")
		}
		if original != t.Text && len(t.Citations) > 0 {
			return errors.New("providercontent: edited text requires removing or rebuilding dependent citations without replay")
		}
		cs := make([]json.RawMessage, 0, len(t.Citations))
		for _, citation := range t.Citations {
			cs = append(cs, citation.Raw)
		}
		current, _ := json.Marshal(cs)
		edited = original != t.Text ||
			!(empty(fields[key]) &&
				len(cs) == 0 ||
				equalJSON(
					fields[key],
					current,
				))
		portable = len(t.Citations) == 0
	case c.ToolCall != nil:
		call := c.ToolCall
		var id, args json.RawMessage
		switch {
		case o.Origin.API == "anthropic.messages" &&
			kind == "tool_use":
			known = only(fields, "type", "id", "name", "input", "caller") &&
				anthropicDirectCaller(fields["caller"])
			id, args = fields["id"], fields["input"]
		case o.Origin.API == "openai.responses" &&
			kind == "function_call":
			known = only(
				fields,
				"type",
				"id",
				"call_id",
				"name",
				"arguments",
				"status",
			) && completed(fields["status"])
			id = fields["call_id"]
			var text string
			if err := json.Unmarshal(
				fields["arguments"],
				&text,
			); err != nil {
				return errors.New("providercontent: tool replay requires encoded arguments")
			}
			args = json.RawMessage(text)
		default:
			return errors.New("providercontent: tool replay has an unsupported source shape")
		}
		var originalID string
		if json.Unmarshal(
			id,
			&originalID,
		) != nil ||
			originalID == "" ||
			!json.Valid(args) ||
			stringField(
				fields,
				"name",
			) == "" {
			return errors.New("providercontent: tool replay requires original ID, name, and arguments")
		}
		edited = originalID != call.ID || stringField(
			fields,
			"name",
		) != call.Name || !equalJSON(
			args,
			call.Args,
		)
		portable = true
	case c.Refusal != nil && kind == "refusal":
		known = only(fields, "type", "refusal")
		edited = stringField(fields, "refusal") != *c.Refusal
	default:
		return errors.New("providercontent: replay attachment has no supported canonical conversion")
	}
	if metadata != nil {
		if stringField(metadata, "type") != "message" {
			return errors.New("providercontent: replay requires message metadata")
		}
		if oldRole := stringField(
			metadata,
			"role",
		); oldRole != "" && oldRole != string(role) {
			edited = true
		}
		known = known && only(
			metadata,
			"type",
			"id",
			"role",
			"status",
			"phase",
		) && completed(metadata["status"])
		// Phase carries output semantics. Preserve it for same-origin edits, but
		// do not discard it while moving to a model that cannot represent it.
		portable = portable && empty(metadata["phase"])
	}
	if foreign && (!known || !portable) {
		return errors.New("providercontent: private replay data has no lossless canonical conversion")
	}
	if edited && !known {
		return errors.New("providercontent: edited content has unfamiliar dependent replay fields")
	}
	return nil
}

// A direct caller adds no meaning beyond the canonical tool call. Other
// callers can depend on a provider code-execution run that we cannot discard.
func anthropicDirectCaller(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var caller map[string]json.RawMessage
	return json.Unmarshal(raw, &caller) == nil &&
		caller != nil &&
		only(caller, "type") &&
		stringField(caller, "type") == "direct"
}

// checkReasoningReplay classifies a complete, known source block before an
// explicit omission. A reasoning kind or matching type label alone is not
// sufficient: unfamiliar fields may carry dependencies we cannot discard.
func checkReasoningReplay(c llm.Content) error {
	o := c.Replay
	if o == nil || o.Partial || o.Version != 1 {
		return errors.New("providercontent: reasoning omission requires complete version 1 replay")
	}
	fields, metadata, err := replayFields(o)
	if err != nil || metadata != nil || stringField(
		fields,
		"type",
	) != o.Type {
		return errors.New("providercontent: reasoning replay requires a known source unit and matching type")
	}
	switch {
	case o.Origin.API == "anthropic.messages" &&
		o.Unit == llm.OpaqueUnitContentBlock &&
		o.Type == "thinking":
		if only(
			fields,
			"type",
			"thinking",
			"signature",
		) && stringField(
			fields,
			"signature",
		) != "" && validString(fields["thinking"]) {
			return nil
		}
	case o.Origin.API == "anthropic.messages" &&
		o.Unit == llm.OpaqueUnitContentBlock &&
		o.Type == "redacted_thinking":
		if only(
			fields,
			"type",
			"data",
		) && stringField(
			fields,
			"data",
		) != "" {
			return nil
		}
	case o.Origin.API == "openai.responses" &&
		o.Unit == llm.OpaqueUnitItem &&
		o.Type == "reasoning":
		if !only(
			fields,
			"type",
			"id",
			"status",
			"summary",
			"encrypted_content",
		) ||
			stringField(
				fields,
				"id",
			) == "" ||
			!completeReasoningStatus(fields["status"]) ||
			(len(fields["summary"]) == 0 &&
				len(fields["encrypted_content"]) == 0) ||
			!validReasoningSummary(fields["summary"]) ||
			(len(fields["encrypted_content"]) > 0 && stringField(
				fields,
				"encrypted_content",
			) == "") {
			break
		}
		return nil
	}
	return errors.New("providercontent: reasoning replay has an unsupported or unfamiliar source shape")
}

func validString(raw json.RawMessage) bool {
	var value *string
	return len(raw) > 0 && json.Unmarshal(
		raw,
		&value,
	) == nil && value != nil
}

func completeReasoningStatus(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == `"completed"`
}

func validReasoningSummary(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || parts == nil {
		return false
	}
	for _, part := range parts {
		if !only(
			part,
			"type",
			"text",
		) || stringField(
			part,
			"type",
		) != "summary_text" || !validString(part["text"]) {
			return false
		}
	}
	return true
}

func replayFields(
	o *llm.Opaque,
) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(o.Raw, &raw); err != nil || raw == nil {
		return nil, nil, errors.New("providercontent: replay requires an object")
	}
	switch {
	case o.Origin.API == "anthropic.messages" &&
		o.Unit == llm.OpaqueUnitContentBlock:
		return raw, nil, nil
	case o.Origin.API == "openai.responses" &&
		o.Unit == llm.OpaqueUnitItem:
		return raw, nil, nil
	case o.Origin.API == "openai.responses" &&
		o.Unit == llm.OpaqueUnitMessagePart:
		var message, part map[string]json.RawMessage
		if !only(
			raw,
			"message",
			"part",
		) || json.Unmarshal(
			raw["message"],
			&message,
		) != nil || json.Unmarshal(
			raw["part"],
			&part,
		) != nil || message == nil || part == nil {
			return nil, nil, errors.New("providercontent: replay requires message and part objects")
		}
		return part, message, nil
	}
	return nil, nil, errors.New("providercontent: replay API or structural unit is unsupported")
}

func only(fields map[string]json.RawMessage, allowed ...string) bool {
	for key := range fields {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func stringField(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func empty(raw json.RawMessage) bool {
	v := string(bytes.TrimSpace(raw))
	return v == "" || v == "null" || v == "[]" || v == `""`
}

func completed(raw json.RawMessage) bool {
	return empty(raw) || string(bytes.TrimSpace(raw)) == `"completed"`
}

func equalJSON(a, b json.RawMessage) bool {
	var left, right any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&left) == nil &&
		db.Decode(&right) == nil &&
		reflect.DeepEqual(
			left,
			right,
		)
}
