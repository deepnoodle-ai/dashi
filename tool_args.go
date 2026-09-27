package kernel

import (
	"bytes"
	"errors"

	"github.com/deepnoodle-ai/kernel/tool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validateToolCall checks the same effective arguments used by authorization
// and execution. No external schema references are loaded by this compiler.
func validateToolCall(def tool.Definition, call tool.Call) error {
	if err := call.Validate(); err != nil {
		return err
	}
	if def.Name != call.Name || def.Kind == tool.KindProviderHosted {
		return errors.New("tool identity does not match executable definition")
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(def.Schema))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://kernel.invalid/agent-tool-schema.json"
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
