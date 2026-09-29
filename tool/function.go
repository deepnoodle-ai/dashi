// Typed tool construction adapted from Dive's tool.go (65c393f), Apache-2.0.

package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/wonton/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Func builds a local tool from typed Go arguments. The input schema is
// generated from T once, at construction, and both the returned Definition and
// argument validation use it; construct a new tool when the contract changes.
// T must be a struct or a pointer to one: each field is required unless its
// json tag has omitempty, and description and enum tags describe it to the
// model.
// Arguments that do not match the schema are refused before fn runs; see
// docs/guides/tools.md#defining-tools for the complete rules.
// Unknown keys in struct arguments are ignored by default. Pass
// WithStrictArguments to reject them, including in nested structs.
// Pass ReadOnly or Idempotent to declare the matching annotation; either one
// lets a runner repeat the call after a crash.
// Keys that differ only in case from declared fields are rejected because Go
// decoding could otherwise pass values to fn that differ from those validated
// against the schema. Duplicate object keys keep their last value.
// Call.Args retains the effective raw JSON in either mode; it is not a
// sanitized copy of the typed arguments.
// The callback also receives the stable call identity, suitable for downstream
// idempotency and reconciliation. An error from the callback is returned
// unchanged, so it follows the Executor contract: a plain error goes to the
// model as a failure, and one wrapped with Fatal stops the turn.
func Func[T any](
	name, description string,
	fn func(context.Context, Call, T) (Outcome, error),
	options ...FuncOption,
) (Tool, error) {
	if fn == nil {
		return Tool{}, errors.New("tool: nil function")
	}
	if err := checkArgumentsType(reflect.TypeFor[T]()); err != nil {
		return Tool{}, fmt.Errorf("tool: %q: %w", name, err)
	}
	var zero T
	var strictArguments, readOnly, idempotent bool
	for _, option := range options {
		strictArguments = strictArguments || option.strictArguments
		readOnly = readOnly || option.readOnly
		idempotent = idempotent || option.idempotent
	}
	s, err := schema.Generate(
		zero,
		schema.GenerateOptions{
			DisallowAdditionalProperties: strictArguments,
		},
	)
	if err != nil {
		return Tool{}, fmt.Errorf(
			"tool: %q: generate schema: %w",
			name,
			err,
		)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return Tool{}, err
	}
	raw, err = normalizeNullableSchema(raw)
	if err != nil {
		return Tool{}, fmt.Errorf(
			"tool: %q: normalize schema: %w",
			name,
			err,
		)
	}
	d := Definition{
		Name:        name,
		Description: description,
		Schema:      raw,
		Kind:        KindLocal,
	}
	if readOnly {
		d.Annotations.ReadOnly = new(true)
	}
	if idempotent {
		d.Annotations.Idempotent = new(true)
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
	executor := typedExecutor[T]{
		name:            name,
		schema:          compiled,
		strictArguments: strictArguments,
		fn:              fn,
	}
	return Tool{Definition: d, Executor: executor}, nil
}

// ValidateArguments applies the same schema and Go field-name policy used by
// Execute. The runner calls it before BeforeTool and after argument rewrites.
func (e typedExecutor[T]) ValidateArguments(args json.RawMessage) error {
	_, err := e.validatedArguments(args)
	return err
}

// validatedArguments returns the same last-key-wins JSON meaning used for
// schema validation and typed decoding, without changing the raw Call.Args.
func (e typedExecutor[T]) validatedArguments(
	args json.RawMessage,
) ([]byte, error) {
	canonical, err := canonjson.Canonicalize(args)
	if err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if err := e.schema.Validate(value); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if err := rejectCaseAliases(value, reflect.TypeFor[T](), "$"); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	return canonical, nil
}

// FuncOption configures a typed tool. Its zero value leaves the defaults
// unchanged.
type FuncOption struct {
	strictArguments bool
	readOnly        bool
	idempotent      bool
}

// WithStrictArguments rejects unknown keys in struct arguments instead of
// ignoring them. The generated schema is closed at the root and at nested
// struct locations, and direct execution uses the same policy.
func WithStrictArguments() FuncOption {
	return FuncOption{strictArguments: true}
}

// ReadOnly declares that the tool does not modify external state, by setting
// Annotations.ReadOnly. A runner may then repeat a call whose result a crash
// lost, and a read-only subagent may use the tool.
func ReadOnly() FuncOption {
	return FuncOption{readOnly: true}
}

// Idempotent declares that repeating a call has the same effect as making it
// once, by setting Annotations.Idempotent. A runner may then repeat a call
// whose result a crash lost.
func Idempotent() FuncOption {
	return FuncOption{idempotent: true}
}

// typedExecutor runs fn only for calls whose arguments satisfy the compiled
// schema and decode into T. Rejected calls return StateNotExecuted, never an
// error, because nothing ran and the result is therefore certain.
type typedExecutor[T any] struct {
	name            string
	schema          *jsonschema.Schema
	strictArguments bool
	fn              func(context.Context, Call, T) (Outcome, error)
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
	canonical, err := e.validatedArguments(call.Args)
	if err != nil {
		return notExecuted(err.Error()), nil
	}
	var input T
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	if e.strictArguments {
		decoder.DisallowUnknownFields()
	}
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

// checkArgumentsType requires a struct, or a pointer to one, other than
// time.Time: providers need an input schema with named properties, and strict
// modes reject open objects such as maps. It also refuses the json ",string"
// option at any depth, because the schema describes the plain value while
// decoding would expect it quoted, so every call would fail, and recursion
// that does not pass through a struct, which has no finite schema.
func checkArgumentsType(t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeFor[time.Time]() {
		return fmt.Errorf("arguments type %s must be a struct", t)
	}
	return checkFieldTypes(
		t,
		map[reflect.Type]bool{},
		map[reflect.Type]bool{},
	)
}

// checkFieldTypes refuses what the schema cannot describe. It records every
// type it visits, so recursive types terminate. Recursion through a struct,
// such as type Node struct{ Children []Node }, is supported. Recursion only
// through pointers, slices, arrays, or maps, such as type Nodes []Nodes, has
// no finite schema, so it is refused: chain holds the collection types seen
// since the last struct.
func checkFieldTypes(
	t reflect.Type,
	seen map[reflect.Type]bool,
	chain map[reflect.Type]bool,
) error {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		if chain[t] {
			return fmt.Errorf(
				"recursive type %s must recurse through a struct",
				t,
			)
		}
		chain[t] = true
		if seen[t] {
			return nil
		}
		seen[t] = true
		return checkFieldTypes(t.Elem(), seen, chain)
	case reflect.Struct:
	default:
		return nil
	}
	if seen[t] {
		return nil
	}
	seen[t] = true
	for field := range t.Fields() {
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.IsExported() {
			if err := checkNullableTag(t, field); err != nil {
				return err
			}
		}
		if slices.Contains(strings.Split(opts, ","), "string") {
			return fmt.Errorf(
				"field %s.%s: the json \",string\" option is not supported",
				t,
				field.Name,
			)
		}
		err := checkFieldTypes(field.Type, seen, map[reflect.Type]bool{})
		if err != nil {
			return err
		}
	}
	return nil
}

// checkNullableTag refuses tags that would make the generated schema disagree
// with encoding/json. Wonton otherwise silently ignores malformed values and
// emits no base type for interfaces and json.RawMessage.
func checkNullableTag(owner reflect.Type, field reflect.StructField) error {
	raw, ok := field.Tag.Lookup("nullable")
	if !ok {
		return nil
	}
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "-" {
		return nil
	}
	path := fmt.Sprintf("field %s.%s", owner, field.Name)
	if field.Anonymous && name == "" {
		base := field.Type
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if base.Kind() == reflect.Struct {
			return fmt.Errorf("%s: nullable tag on flattened field is unsupported", path)
		}
	}
	nullable, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid nullable tag %q", path, raw)
	}
	if nullable && field.Type.Kind() != reflect.Pointer {
		return fmt.Errorf("%s: nullable:true requires a pointer", path)
	}
	if !nullable && unconstrainedSchemaType(field.Type) {
		return fmt.Errorf(
			"%s: nullable:false cannot exclude null from an unconstrained schema",
			path,
		)
	}
	return nil
}

func unconstrainedSchemaType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Interface || t == reflect.TypeFor[json.RawMessage]()
}

// normalizeNullableSchema turns Wonton's nullable extension into standard JSON
// Schema before the same bytes are published and compiled. RawMessage maps
// preserve number and default values while descending through generated nodes.
func normalizeNullableSchema(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(`"nullable"`)) {
		return raw, nil
	}
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	if value, ok := node["nullable"]; ok {
		var nullable bool
		if err := json.Unmarshal(value, &nullable); err != nil {
			return nil, err
		}
		delete(node, "nullable")
		if nullable {
			if value, ok := node["type"]; ok {
				var base string
				if err := json.Unmarshal(value, &base); err != nil {
					return nil, err
				}
				union, err := json.Marshal([]string{base, "null"})
				if err != nil {
					return nil, err
				}
				node["type"] = union
			}
			if value, ok := node["enum"]; ok {
				var values []json.RawMessage
				if err := json.Unmarshal(value, &values); err != nil {
					return nil, err
				}
				if !slices.ContainsFunc(values, func(v json.RawMessage) bool {
					return bytes.Equal(bytes.TrimSpace(v), []byte("null"))
				}) {
					values = append(values, json.RawMessage("null"))
				}
				encoded, err := json.Marshal(values)
				if err != nil {
					return nil, err
				}
				node["enum"] = encoded
			}
		}
	}
	if value, ok := node["properties"]; ok {
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(value, &properties); err != nil {
			return nil, err
		}
		for name, property := range properties {
			var err error
			properties[name], err = normalizeNullableSchema(property)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", name, err)
			}
		}
		var err error
		node["properties"], err = json.Marshal(properties)
		if err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"items", "additionalProperties"} {
		value, ok := node[name]
		if !ok || len(value) == 0 || value[0] != '{' {
			continue
		}
		var err error
		node[name], err = normalizeNullableSchema(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return json.Marshal(node)
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
