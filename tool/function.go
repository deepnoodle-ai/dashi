// Typed tool construction adapted from Dive's tool.go (65c393f), Apache-2.0.

package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/deepnoodle-ai/wonton/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Func builds a local tool from typed Go arguments. The input schema is
// generated from T once, at construction, and both the returned Definition and
// argument validation use it; construct a new tool when the contract changes.
// T must be a struct: each field is required unless its json tag has
// omitempty, and description and enum tags describe it to the model.
// Arguments that do not match the schema are refused before fn runs; see
// docs/guides/model-calls.md#defining-tools for the complete rules.
// The callback also receives the stable call identity, suitable for downstream
// idempotency and reconciliation. An error from the callback is returned
// unchanged, so it follows the Executor contract: a plain error goes to the
// model as a failure, and one wrapped with Fatal stops the turn.
func Func[T any](
	name, description string,
	fn func(context.Context, Call, T) (Outcome, error),
) (Tool, error) {
	if fn == nil {
		return Tool{}, errors.New("tool: nil function")
	}
	var zero T
	s, err := schema.Generate(
		zero,
		schema.GenerateOptions{
			DisallowAdditionalProperties: true,
		},
	)
	if err != nil {
		return Tool{}, fmt.Errorf(
			"tool: %q: generate schema: %w",
			name,
			err,
		)
	}
	if s.Type != schema.Object {
		return Tool{}, fmt.Errorf(
			"tool: %q: arguments type %T must be a struct",
			name,
			zero,
		)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return Tool{}, err
	}
	d := Definition{
		Name:        name,
		Description: description,
		Schema:      raw,
		Kind:        KindLocal,
	}
	if err = d.Validate(); err != nil {
		return Tool{}, err
	}
	compiled, err := compileSchema(raw)
	if err != nil {
		return Tool{}, fmt.Errorf(
			"tool: %q: compile schema: %w",
			name,
			err,
		)
	}
	executor := typedExecutor[T]{name: name, schema: compiled, fn: fn}
	return Tool{Definition: d, Executor: executor}, nil
}

// typedExecutor runs fn only for calls whose arguments satisfy the compiled
// schema and decode into T. Rejected calls return StateNotExecuted, never an
// error, because nothing ran and the result is therefore certain.
type typedExecutor[T any] struct {
	name   string
	schema *jsonschema.Schema
	fn     func(context.Context, Call, T) (Outcome, error)
}

// Execute passes fn a clone of call, so the callback cannot alter the
// caller's arguments. An outcome from fn that fails validation is returned as
// a fatal ErrInvalidOutcome error, since the callback, not the model, is at
// fault.
func (e typedExecutor[T]) Execute(
	ctx context.Context,
	call Call,
) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return notExecuted(err.Error()), nil
	}
	if err := call.Validate(); err != nil {
		return notExecuted(err.Error()), nil
	}
	if call.Name != e.name {
		return notExecuted("tool name does not match executor"), nil
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(call.Args))
	if err != nil {
		return notExecuted("invalid input: " + err.Error()), nil
	}
	if err := e.schema.Validate(value); err != nil {
		return notExecuted("invalid input: " + err.Error()), nil
	}
	var input T
	decoder := json.NewDecoder(bytes.NewReader(call.Args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return notExecuted("invalid input: " + err.Error()), nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return notExecuted("unexpected trailing input"), nil
	}
	outcome, err := e.fn(ctx, call.Clone(), input)
	if err != nil {
		return Outcome{}, err
	}
	if err = outcome.Validate(); err != nil {
		return Outcome{}, Fatal(fmt.Errorf(
			"%w: %w",
			ErrInvalidOutcome,
			err,
		))
	}
	return outcome, nil
}

// compileSchema compiles a generated schema. Generated schemas are
// self-contained, and compilation never reads files or URLs: the loader
// registers no schemes, so any external $ref fails.
func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://dashi.invalid/tool-schema.json"
	if err := c.AddResource(location, value); err != nil {
		return nil, err
	}
	return c.Compile(location)
}

// notExecuted reports that a call was rejected before running, with reason as
// its text output.
func notExecuted(reason string) Outcome {
	return Outcome{State: StateNotExecuted, Output: new(TextOutput(reason))}
}
