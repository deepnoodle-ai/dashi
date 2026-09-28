package dashi

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/wonton/schema"
)

// ErrNoOutput reports that a turn has no structured output to decode.
// DecodeOutput wraps it with the reason.
var ErrNoOutput = errors.New("dashi: no structured output")

// MustOutputSchema returns a JSON Schema for T, for Agent.OutputSchema or
// Start.OutputSchema. It follows tool.Func's rules: T should be a struct;
// fields are required unless tagged omitempty, `description` and `enum` tags
// are read, and properties not in T are rejected. The schema also passes the
// check the runner applies to an output schema, so a T that cannot be one,
// such as a slice or a string, fails here rather than when the turn starts.
// Like regexp.MustCompile, it panics when T is unusable, a programming error
// found on first use.
func MustOutputSchema[T any]() json.RawMessage {
	var zero T
	generated, err := schema.Generate(
		zero,
		schema.GenerateOptions{DisallowAdditionalProperties: true},
	)
	if err != nil {
		panic(fmt.Sprintf("dashi: output schema for %T: %v", zero, err))
	}
	raw, err := json.Marshal(generated)
	if err != nil {
		panic(fmt.Sprintf("dashi: output schema for %T: %v", zero, err))
	}
	if _, err := compileOutputSchema(raw); err != nil {
		panic(fmt.Sprintf("dashi: output schema for %T: %v", zero, err))
	}
	return raw
}

// DecodeOutput decodes a turn's structured output into T. The value was
// validated against the turn's output schema before it was recorded, so a
// decoding error means T does not match that schema. When the turn has no
// output, the error wraps ErrNoOutput and says why: the turn stopped without
// completing (waiting, limited, refused, failed, and so on, with the stop
// reason), or it completed without an output schema.
func DecodeOutput[T any](o Outcome) (T, error) {
	var value T
	if o.Output == nil {
		if o.Stop.Kind != StopCompleted {
			reason := string(o.Stop.Kind)
			if o.Stop.Reason != "" {
				reason += ": " + o.Stop.Reason
			}
			return value, fmt.Errorf("%w: turn stopped %s", ErrNoOutput, reason)
		}
		return value, fmt.Errorf(
			"%w: turn completed without an output schema",
			ErrNoOutput,
		)
	}
	if err := json.Unmarshal(o.Output.Value, &value); err != nil {
		return value, fmt.Errorf("dashi: decode structured output: %w", err)
	}
	return value, nil
}
