package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type nullableChild struct {
	Label *string `json:"label"`
}

type NullableEmbedded struct {
	Label string `json:"label"`
}

type nullableArgs struct {
	Required *string            `json:"required" enum:"red,blue"`
	Optional *string            `json:"optional,omitempty"`
	Nested   *nullableChild     `json:"nested,omitempty"`
	List     *[]string          `json:"list,omitempty"`
	Values   *map[string]int    `json:"values,omitempty"`
	Items    []*string          `json:"items,omitempty"`
	ByName   map[string]*string `json:"by_name,omitempty"`
	Untyped  *any               `json:"untyped,omitempty"`
	Raw      *json.RawMessage   `json:"raw,omitempty"`
}

func TestFuncNullableSchemaValidationAndDecode(t *testing.T) {
	var received nullableArgs
	calls := 0
	fn, err := Func(
		"nullable",
		"",
		func(_ context.Context, _ Call, in nullableArgs) (Outcome, error) {
			calls++
			received = in
			return Succeeded("ok"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(fn.Definition.Schema, []byte(`"nullable"`)) {
		t.Fatalf("nonstandard nullable keyword: %s", fn.Definition.Schema)
	}
	var schema map[string]any
	if err := json.Unmarshal(fn.Definition.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	assertTypeUnion(t, properties["required"], "string")
	assertTypeUnion(t, properties["nested"], "object")
	assertTypeUnion(t, properties["list"], "array")
	assertTypeUnion(t, properties["values"], "object")
	assertTypeUnion(t, properties["items"].(map[string]any)["items"], "string")
	assertTypeUnion(
		t,
		properties["by_name"].(map[string]any)["additionalProperties"],
		"string",
	)
	for _, name := range []string{"untyped", "raw"} {
		if _, ok := properties[name].(map[string]any)["type"]; ok {
			t.Errorf("%s: unconstrained pointer acquired a type", name)
		}
	}
	enum := properties["required"].(map[string]any)["enum"].([]any)
	if len(enum) != 3 || enum[0] != "red" || enum[1] != "blue" || enum[2] != nil {
		t.Fatalf("nullable enum = %#v", enum)
	}

	tests := []struct {
		name  string
		args  string
		runs  bool
		check func(nullableArgs) bool
	}{
		{"required null", `{"required":null}`, true, func(v nullableArgs) bool {
			return v.Required == nil && v.Optional == nil
		}},
		{"required value", `{"required":"red"}`, true, func(v nullableArgs) bool {
			return v.Required != nil && *v.Required == "red"
		}},
		{"missing required", `{}`, false, nil},
		{"enum violation", `{"required":"green"}`, false, nil},
		{
			name: "optional nulls",
			args: `{"required":null,"optional":null,"nested":null,` +
				`"list":null,"values":null,"items":[null,"x"],` +
				`"by_name":{"a":null,"b":"y"},"untyped":null,"raw":null}`,
			runs: true,
			check: func(v nullableArgs) bool {
				return v.Optional == nil && v.Nested == nil && v.List == nil &&
					v.Values == nil && v.Items[0] == nil && *v.Items[1] == "x" &&
					v.ByName["a"] == nil && *v.ByName["b"] == "y" &&
					v.Untyped == nil && v.Raw == nil
			},
		},
		{
			name: "nested null",
			args: `{"required":null,"nested":{"label":null}}`,
			runs: true,
			check: func(v nullableArgs) bool {
				return v.Nested != nil && v.Nested.Label == nil
			},
		},
		{"nested missing required", `{"required":null,"nested":{}}`, false, nil},
		{
			name: "pointer containers with values",
			args: `{"required":null,"list":["a","b"],` +
				`"values":{"one":1,"two":2}}`,
			runs: true,
			check: func(v nullableArgs) bool {
				return v.List != nil && len(*v.List) == 2 &&
					(*v.List)[0] == "a" && (*v.List)[1] == "b" &&
					v.Values != nil && len(*v.Values) == 2 &&
					(*v.Values)["one"] == 1 && (*v.Values)["two"] == 2
			},
		},
		{
			name: "untyped values",
			args: `{"required":null,"untyped":{"k":1},"raw":{"k":1}}`,
			runs: true,
			check: func(v nullableArgs) bool {
				return v.Untyped != nil && v.Raw != nil
			},
		},
		{"wrong pointer type", `{"required":null,"optional":7}`, false, nil},
		{"wrong nested type", `{"required":null,"nested":7}`, false, nil},
		{"wrong list container", `{"required":null,"list":{}}`, false, nil},
		{"wrong map container", `{"required":null,"values":[]}`, false, nil},
		{"wrong list item", `{"required":null,"list":[7]}`, false, nil},
		{"wrong map item", `{"required":null,"values":{"one":"x"}}`, false, nil},
		{"wrong array item", `{"required":null,"items":[7]}`, false, nil},
		{"wrong map value", `{"required":null,"by_name":{"a":7}}`, false, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := calls
			out, err := fn.Executor.Execute(
				t.Context(),
				Call{ID: "call-1", Name: "nullable", Args: json.RawMessage(test.args)},
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.runs {
				if out.State != StateSucceeded ||
					calls != before+1 || !test.check(received) {
					t.Fatalf("out=%+v calls=%d decoded=%+v", out, calls-before, received)
				}
			} else if out.State != StateNotExecuted || calls != before {
				t.Fatalf("invalid input ran: out=%+v calls=%d", out, calls-before)
			}
		})
	}
}

func assertTypeUnion(t *testing.T, raw any, base string) {
	t.Helper()
	types := raw.(map[string]any)["type"].([]any)
	if len(types) != 2 || types[0] != base || types[1] != "null" {
		t.Fatalf("type = %#v, want [%s null]", types, base)
	}
}

func TestFuncNullableTagBoundaries(t *testing.T) {
	invalid := map[string]error{
		"scalar true": buildFunc[struct {
			Value string `json:"value" nullable:"true"`
		}](),
		"slice true": buildFunc[struct {
			Value []string `json:"value" nullable:"true"`
		}](),
		"map true": buildFunc[struct {
			Value map[string]string `json:"value" nullable:"true"`
		}](),
		"any true": buildFunc[struct {
			Value any `json:"value" nullable:"true"`
		}](),
		"raw true": buildFunc[struct {
			Value json.RawMessage `json:"value" nullable:"true"`
		}](),
		"pointer any false": buildFunc[struct {
			Value *any `json:"value" nullable:"false"`
		}](),
		"pointer raw false": buildFunc[struct {
			Value *json.RawMessage `json:"value" nullable:"false"`
		}](),
		"malformed": buildFunc[struct {
			Value *string `json:"value" nullable:"sometimes"`
		}](),
		"flattened embedded": buildFunc[struct {
			*NullableEmbedded `nullable:"false"`
		}](),
	}
	for name, err := range invalid {
		field := ".Value"
		if name == "flattened embedded" {
			field = ".NullableEmbedded"
		}
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: expected field-path error, got %v", name, err)
		}
	}

	calls := 0
	fn, err := Func(
		"nonnull",
		"",
		func(_ context.Context, _ Call, _ struct {
			Value *string `json:"value" nullable:"false"`
		}) (Outcome, error) {
			calls++
			return Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(fn.Definition.Schema, []byte(`"nullable"`)) ||
		!bytes.Contains(fn.Definition.Schema, []byte(`"type":"string"`)) {
		t.Fatalf("nullable:false schema = %s", fn.Definition.Schema)
	}
	for _, args := range []string{`{"value":null}`, `{"value":"yes"}`} {
		out, err := fn.Executor.Execute(
			t.Context(),
			Call{ID: "call-1", Name: "nonnull", Args: json.RawMessage(args)},
		)
		if err != nil {
			t.Fatal(err)
		}
		want := StateNotExecuted
		if strings.Contains(args, "yes") {
			want = StateSucceeded
		}
		if out.State != want {
			t.Errorf("%s: state %s, want %s", args, out.State, want)
		}
	}
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
}

func TestFuncRequiredPointerOverrideAndRootObject(t *testing.T) {
	type input struct {
		Value *string `json:"value,omitempty" required:"true"`
	}
	called := 0
	fn, err := Func(
		"required",
		"",
		func(_ context.Context, _ Call, value *input) (Outcome, error) {
			called++
			if value == nil || value.Value != nil {
				t.Fatalf("decoded value = %+v, want nonnil root and nil field", value)
			}
			return Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args string
		want State
	}{
		{`{}`, StateNotExecuted},
		{`null`, StateNotExecuted},
		{`{"value":null}`, StateSucceeded},
	} {
		out, err := fn.Executor.Execute(
			t.Context(),
			Call{
				ID:   "call-1",
				Name: "required",
				Args: json.RawMessage(test.args),
			},
		)
		if err != nil || out.State != test.want {
			t.Errorf("%s: outcome=%+v err=%v, want %s", test.args, out, err, test.want)
		}
	}
	if called != 1 {
		t.Fatalf("callback calls = %d, want 1", called)
	}
}

func TestFuncIgnoresSkippedFieldSubtree(t *testing.T) {
	type ignored struct {
		Nullable string `json:"nullable" nullable:"true"`
		Quoted   int    `json:"quoted,string"`
	}
	type input struct {
		Skip ignored `json:"-"`
		Keep *string `json:"keep,omitempty"`
	}
	called := false
	fn, err := Func(
		"ignored",
		"",
		func(_ context.Context, _ Call, in input) (Outcome, error) {
			called = true
			if in.Keep != nil {
				t.Fatalf("keep = %v, want nil", *in.Keep)
			}
			return Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatalf("ignored subtree rejected: %v", err)
	}
	if bytes.Contains(fn.Definition.Schema, []byte(`"nullable"`)) ||
		bytes.Contains(fn.Definition.Schema, []byte(`"quoted"`)) {
		t.Fatalf("ignored field appeared in schema: %s", fn.Definition.Schema)
	}
	out, err := fn.Executor.Execute(
		t.Context(),
		Call{ID: "call-1", Name: "ignored", Args: json.RawMessage(`{"keep":null}`)},
	)
	if err != nil || out.State != StateSucceeded || !called {
		t.Fatalf("out=%+v err=%v called=%v", out, err, called)
	}
}
