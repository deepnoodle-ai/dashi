package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/deepnoodle-ai/kernel/internal/canonjson"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func validateOutputToolChoice(settings llm.Settings) error {
	if choice := settings.ToolChoice; choice != nil && (choice.Mode == llm.ToolChoiceNone || (choice.Mode == llm.ToolChoiceTool && choice.Name != SubmitOutputTool)) {
		return errors.New("kernel: output schema requires the submission tool to remain available")
	}
	return nil
}

const (
	// SubmitOutputTool is reserved for a turn that declares OutputSchema. Its
	// argument object is the candidate final value, not an application tool.
	SubmitOutputTool     = "kernel_submit_output"
	maxOutputSchemaBytes = 32 << 10
	maxOutputValueBytes  = 256 << 10
)

func compileOutputSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > maxOutputSchemaBytes {
		return nil, fmt.Errorf("kernel: output schema must be 1-%d bytes", maxOutputSchemaBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("kernel: output schema must be a JSON Schema object")
	}
	// Tool declarations and the public turn result carry an object. Requiring
	// the root type prevents a provider from receiving an unrepresentable
	// boolean, scalar, or array function-argument schema.
	var rootType string
	if err := json.Unmarshal(object["type"], &rootType); err != nil || rootType != "object" {
		return nil, errors.New("kernel: output schema root must have type object")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("kernel: invalid output schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	// No network or file fetches are permitted while admitting or replaying a
	// turn. Local $defs remain available through the registered resource.
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://kernel.invalid/output-schema.json"
	if err := compiler.AddResource(location, value); err != nil {
		return nil, fmt.Errorf("kernel: invalid output schema: %w", err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("kernel: invalid output schema: %w", err)
	}
	return schema, nil
}

func validateOutputValue(schema *jsonschema.Schema, raw json.RawMessage) error {
	if schema == nil || len(raw) == 0 || len(raw) > maxOutputValueBytes {
		return errors.New("kernel: structured output is missing or too large")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("kernel: structured output is invalid JSON: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("kernel: structured output must be an object")
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("kernel: structured output does not satisfy schema: %w", err)
	}
	return nil
}

func parseOutputText(text string, schema *jsonschema.Schema) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		first, body, ok := strings.Cut(trimmed, "\n")
		if !ok || (first != "```" && first != "```json") || !strings.HasSuffix(body, "```") {
			return nil, errors.New("kernel: structured output must be one JSON object")
		}
		trimmed = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	if len(trimmed) > maxOutputValueBytes {
		return nil, errors.New("kernel: structured output is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("kernel: structured output is not valid JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("kernel: structured output must contain one JSON value")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("kernel: structured output must be an object")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := validateOutputValue(schema, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// sameJSON reports whether a and b are single JSON values with the same
// meaning; see package canonjson.
func sameJSON(a, b json.RawMessage) bool {
	return canonjson.Equal(a, b)
}
