// Package outputschema validates object-root JSON Schema contracts shared by
// direct model calls and structured agent turns.
package outputschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	MaxSchemaBytes = 32 << 10
	MaxValueBytes  = 256 << 10
)

// Compile admits a bounded, self-contained, object-root JSON Schema. Local
// $defs work; external resources cannot be fetched.
func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("output schema must be 1-%d bytes", MaxSchemaBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("output schema must be a JSON Schema object")
	}
	var rootType string
	if err := json.Unmarshal(object["type"], &rootType); err != nil || rootType != "object" {
		return nil, errors.New("output schema root must have type object")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://dashi.invalid/output-schema.json"
	if err := compiler.AddResource(location, value); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	return schema, nil
}

// Validate checks a single, bounded JSON object against an admitted schema.
func Validate(schema *jsonschema.Schema, raw json.RawMessage) error {
	if schema == nil || len(raw) == 0 || len(raw) > MaxValueBytes {
		return errors.New("structured output is missing or too large")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("structured output is invalid JSON: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("structured output must be an object")
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("structured output does not satisfy schema: %w", err)
	}
	return nil
}
