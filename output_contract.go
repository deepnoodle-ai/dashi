package dashi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/outputschema"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func validateOutputToolChoice(settings llm.Settings) error {
	if choice := settings.ToolChoice; choice != nil &&
		(choice.Mode == llm.ToolChoiceNone ||
			(choice.Mode == llm.ToolChoiceTool &&
				choice.Name != SubmitOutputTool)) {
		return errors.New("dashi: output schema requires the submission tool to remain available")
	}
	return nil
}

const (
	// SubmitOutputTool is reserved for a turn that declares OutputSchema. Its
	// argument object is the candidate final value, not an application tool.
	SubmitOutputTool    = "dashi_submit_output"
	maxOutputValueBytes = outputschema.MaxValueBytes
)

func compileOutputSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	schema, err := outputschema.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("dashi: %w", err)
	}
	return schema, nil
}

func validateOutputValue(
	schema *jsonschema.Schema,
	raw json.RawMessage,
) error {
	if err := outputschema.Validate(schema, raw); err != nil {
		return fmt.Errorf("dashi: %w", err)
	}
	return nil
}

func parseOutputText(
	text string,
	schema *jsonschema.Schema,
) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		first, body, ok := strings.Cut(trimmed, "\n")
		if !ok || (first != "```" && first != "```json") || !strings.HasSuffix(
			body,
			"```",
		) {
			return nil, errors.New("dashi: structured output must be one JSON object")
		}
		trimmed = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	if len(trimmed) > maxOutputValueBytes {
		return nil, errors.New("dashi: structured output is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("dashi: structured output is not valid JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("dashi: structured output must contain one JSON value")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("dashi: structured output must be an object")
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
