package providercontent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Gemini's content blocks have no type key. Preserve private fields only when
// the canonical value still agrees with the saved source part. An ID edit may
// overlay raw replay only after the original call is verified and the complete
// function-call shape is recognized.
func checkGeminiReplay(c, original llm.Content, foreign bool) error {
	o := c.Replay
	if foreign {
		return errors.New(
			"providercontent: Gemini replay requires its original destination",
		)
	}
	if o.Unit != llm.OpaqueUnitContentBlock {
		return errors.New("providercontent: Gemini replay requires a content block")
	}
	if o.Version != 1 || o.Partial || !utf8.Valid(o.Raw) {
		return errors.New(
			"providercontent: Gemini replay requires complete version 1 UTF-8 data",
		)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(o.Raw, &fields); err != nil || fields == nil {
		return errors.New("providercontent: Gemini replay requires an object")
	}
	switch {
	case c.Block != nil &&
		c.Block.Kind == content.KindText &&
		o.Type == "text":
		if len(c.Block.Text.Citations) != 0 ||
			original.Block == nil ||
			original.Block.Kind != content.KindText {
			return errors.New(
				"providercontent: Gemini text replay requires uncited text",
			)
		}
		var saved string
		if raw, exists := fields["text"]; exists {
			if !validString(raw) {
				return errors.New(
					"providercontent: Gemini text replay requires original text",
				)
			}
			_ = json.Unmarshal(raw, &saved)
		}
		if saved == "" && stringField(
			fields,
			"thoughtSignature",
		) == "" {
			return errors.New(
				"providercontent: Gemini text replay requires a text source part",
			)
		}
		if saved != c.Block.Text.Text ||
			saved != original.Block.Text.Text {
			return errors.New(
				"providercontent: edited Gemini text requires removing replay",
			)
		}
		if string(bytes.TrimSpace(fields["thought"])) == "true" ||
			geminiJSONObject(fields["functionCall"]) {
			return errors.New(
				"providercontent: Gemini text replay has a different source kind",
			)
		}
		return nil
	case c.ToolCall != nil && o.Type == "function_call":
		if original.ToolCall == nil {
			return errors.New(
				"providercontent: Gemini tool replay requires an original call",
			)
		}
		var saved map[string]json.RawMessage
		if err := json.Unmarshal(
			fields["functionCall"],
			&saved,
		); err != nil || saved == nil {
			return errors.New(
				"providercontent: Gemini tool replay requires functionCall",
			)
		}
		// Gemini omits args, or sends null, for a call with no arguments; the
		// decoded call has an empty object.
		args := saved["args"]
		if trimmed := bytes.TrimSpace(args); len(trimmed) == 0 ||
			string(trimmed) == "null" {
			args = json.RawMessage(`{}`)
		}
		var name, id string
		if !validString(saved["name"]) ||
			json.Unmarshal(
				saved["name"],
				&name,
			) != nil || name == "" ||
			!utf8.ValidString(name) ||
			!geminiJSONObject(args) ||
			!equalJSON(args, c.ToolCall.Args) ||
			!equalJSON(args, original.ToolCall.Args) ||
			name != c.ToolCall.Name ||
			name != original.ToolCall.Name {
			return errors.New(
				"providercontent: edited Gemini tool replay requires original name and arguments",
			)
		}
		if raw, exists := saved["id"]; exists {
			if !validString(raw) {
				return errors.New(
					"providercontent: Gemini tool replay requires a string ID",
				)
			}
			_ = json.Unmarshal(raw, &id)
		}
		if !utf8.ValidString(id) {
			return errors.New("providercontent: Gemini tool replay requires UTF-8 ID")
		}
		if id != "" && id != original.ToolCall.ID {
			return errors.New(
				"providercontent: Gemini tool replay ID differs from the original call",
			)
		}
		if id != "" && c.ToolCall.ID == original.ToolCall.ID {
			return nil
		}
		// An ID-less signed part can be replayed byte for byte. The canonical
		// call ID is only Dashi's pairing identity; it must not be injected
		// into the provider-signed functionCall.
		if id == "" && c.ToolCall.ID == original.ToolCall.ID {
			return nil
		}
		outer, outerOK := geminiStrictObject(o.Raw)
		inner, innerOK := geminiStrictObject(fields["functionCall"])
		if !outerOK || !innerOK || !only(outer, "functionCall") ||
			!only(inner, "id", "name", "args") {
			return errors.New(
				"providercontent: edited Gemini tool ID requires unsigned recognized replay",
			)
		}
		return nil
	}
	return errors.New(
		"providercontent: Gemini replay attachment has no supported canonical conversion",
	)
}

func geminiJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func geminiStrictObject(
	raw json.RawMessage,
) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		if _, exists := fields[name]; exists {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, false
	}
	return fields, true
}
