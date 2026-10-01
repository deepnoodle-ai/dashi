package dashi

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validateToolCall checks the same effective arguments used by authorization
// and execution. No external schema references are loaded by this compiler.
func validateToolCall(def tool.Definition, call tool.Call) error {
	if err := call.Validate(); err != nil {
		return err
	}
	if def.Name != call.Name || def.Kind == tool.KindProviderTool {
		return errors.New("tool identity does not match executable definition")
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(def.Schema))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://dashi.invalid/agent-tool-schema.json"
	if err = compiler.AddResource(location, raw); err != nil {
		return err
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return err
	}
	args, err := jsonschema.UnmarshalJSON(bytes.NewReader(call.Args))
	if err != nil {
		return err
	}
	return schema.Validate(args)
}

// A typed executor can impose Go decoding rules that JSON Schema cannot
// express. Keep those checks on the same effective arguments as the runner's
// schema precheck, including arguments supplied by hook rewrites.
type argumentValidator interface {
	ValidateArguments(json.RawMessage) error
}

func validateBoundArguments(ex tool.Executor, args json.RawMessage) error {
	if validator, ok := ex.(argumentValidator); ok {
		return validator.ValidateArguments(args)
	}
	return nil
}
