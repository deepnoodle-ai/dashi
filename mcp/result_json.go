package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConvertResultJSON converts a saved complete MCP result envelope. The caller
// selects this format from admitted tool identity; JSON shape cannot establish
// provenance. It requires a content array and uses SDK polymorphic decoding for
// blocks while preserving exact structured JSON numbers. Arrays of blocks and
// ordinary application JSON are separate formats.
//
// Input-required responses return ErrInputRequired before completed-content
// validation. Keep the original journal for that host interaction and for safe
// settlement of unusable completed work; see ConvertResult. Bound input size
// before calling. Returned errors never quote raw JSON or SDK payload values.
func ConvertResultJSON(raw json.RawMessage) (tool.Outcome, []string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return tool.Outcome{}, nil, errors.New("mcp: result requires one JSON object")
	}
	if value, ok := envelope["resultType"]; ok {
		var typ string
		if err := json.Unmarshal(value, &typ); err != nil || isNull(value) {
			return tool.Outcome{}, nil, errors.New("mcp: result type requires a string")
		}
		switch typ {
		case "input_required":
			return tool.Outcome{}, nil, ErrInputRequired
		case "complete":
		default:
			return tool.Outcome{}, nil, errors.New("mcp: result type is unsupported")
		}
	}
	if value, ok := envelope["isError"]; ok {
		var failed bool
		if err := json.Unmarshal(value, &failed); err != nil || isNull(value) {
			return tool.Outcome{}, nil, errors.New("mcp: isError requires a boolean")
		}
	}
	var blocks []json.RawMessage
	value, ok := envelope["content"]
	if !ok || isNull(value) || json.Unmarshal(value, &blocks) != nil {
		return tool.Outcome{}, nil, errors.New("mcp: completed result requires a content array")
	}
	for i, block := range blocks {
		if err := validateBlockJSON(block); err != nil {
			return tool.Outcome{}, nil, fmt.Errorf(
				"mcp: content block %d: %w", i, err,
			)
		}
	}
	var structured any
	if value, ok := envelope["structuredContent"]; ok {
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&structured); err != nil {
			return tool.Outcome{}, nil, errors.New(
				"mcp: structured content requires a JSON value",
			)
		}
		// Preserve an explicitly supplied null as well as exact numbers.
		if structured == nil {
			structured = json.RawMessage("null")
		}
		delete(envelope, "structuredContent")
	}
	// The SDK decodes StructuredContent through float64. Remove that field
	// before its polymorphic block decoding, then restore our exact value.
	wire, err := json.Marshal(envelope)
	if err != nil {
		return tool.Outcome{}, nil, errors.New("mcp: result requires valid JSON fields")
	}
	var res sdk.CallToolResult
	if err := json.Unmarshal(wire, &res); err != nil {
		// Locate a malformed SDK block without exposing its decoder's error
		// text, which can contain untrusted input. Valid results decode once.
		for i, block := range blocks {
			one := append([]byte(`{"content":[`), block...)
			one = append(one, ']', '}')
			var entry sdk.CallToolResult
			if json.Unmarshal(one, &entry) != nil {
				return tool.Outcome{}, nil, fmt.Errorf(
					"mcp: content block %d requires valid MCP fields", i,
				)
			}
		}
		return tool.Outcome{}, nil, errors.New("mcp: result requires valid MCP content")
	}
	res.StructuredContent = structured
	return ConvertResult(&res)
}

// validateBlockJSON checks required fields the SDK's wire structs otherwise
// turn into zero values. Polymorphic decoding and base64 handling stay in SDK.
func validateBlockJSON(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("content entry requires an object")
	}
	var typ string
	if err := json.Unmarshal(fields["type"], &typ); err != nil {
		return errors.New("content type requires a string")
	}
	switch typ {
	case "text":
		return requireString(fields, "text")
	case "image", "audio":
		if err := requireString(fields, "data"); err != nil {
			return err
		}
		return requireString(fields, "mimeType")
	case "resource_link":
		if err := requireString(fields, "name"); err != nil {
			return err
		}
		return requireString(fields, "uri")
	case "resource":
		var resource map[string]json.RawMessage
		if err := json.Unmarshal(fields["resource"], &resource); err != nil ||
			resource == nil {
			return errors.New("resource content requires an object")
		}
		if err := requireString(resource, "uri"); err != nil {
			return err
		}
		_, text := resource["text"]
		_, blob := resource["blob"]
		if text == blob {
			return errors.New("resource requires exactly one of text or blob")
		}
		if text {
			return requireString(resource, "text")
		}
		return requireString(resource, "blob")
	case "tool_use", "tool_result":
		return errors.New("sampling content is not a tool result block")
	}
	return errors.New("unsupported tool result content type")
}

func requireString(fields map[string]json.RawMessage, name string) error {
	value, ok := fields[name]
	var text string
	if !ok || isNull(value) || json.Unmarshal(value, &text) != nil {
		return fmt.Errorf("%s requires a string", name)
	}
	return nil
}

func isNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
