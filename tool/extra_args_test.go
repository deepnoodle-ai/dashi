package tool

import (
	"context"
	"encoding/json"
	"testing"
)

type extraChild struct {
	Label string `json:"label"`
}

type extraInput struct {
	Query    string                `json:"query" enum:"yes,no"`
	Region   string                `json:"region,omitempty"`
	Nested   *extraChild           `json:"nested,omitempty"`
	Rows     []extraChild          `json:"rows,omitempty"`
	ByName   map[string]extraChild `json:"by_name,omitempty"`
	Counters map[string]int        `json:"counters,omitempty"`
}

func TestFuncIgnoresExtraStructKeysByDefault(t *testing.T) {
	var received extraInput
	var raw Call
	calls := 0
	fn, err := Func(
		"lookup",
		"",
		func(_ context.Context, call Call, input extraInput) (Outcome, error) {
			calls++
			received = input
			raw = call
			return Succeeded("ok"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	const args = `{"query":"yes","extra":1,"regoin":"us",` +
		`"nested":{"label":"a","extra":2},` +
		`"rows":[{"label":"b","extra":3}],` +
		`"by_name":{"one":{"label":"c","extra":4}},` +
		`"counters":{"one":5}}`
	call := Call{ID: "call-1", Name: "lookup", Args: []byte(args)}
	out, err := fn.Executor.Execute(t.Context(), call)
	if err != nil || out.State != StateSucceeded || calls != 1 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
	}
	if received.Query != "yes" || received.Region != "" ||
		received.Nested.Label != "a" ||
		len(received.Rows) != 1 || received.Rows[0].Label != "b" ||
		received.ByName["one"].Label != "c" || received.Counters["one"] != 5 {
		t.Fatalf("typed input lost declared values: %+v", received)
	}
	if string(raw.Args) != args {
		t.Fatalf("callback lost raw extras: %s", raw.Args)
	}
	raw.Args[0] = '['
	if string(call.Args) != args {
		t.Fatal("callback changed caller arguments")
	}
	invalid := Call{
		ID:   "call-2",
		Name: "lookup",
		Args: []byte(`{"query":"yes","counters":{"one":"five"}}`),
	}
	out, err = fn.Executor.Execute(t.Context(), invalid)
	if err != nil || out.State != StateNotExecuted || calls != 1 {
		t.Fatalf("declared map value bypassed validation: out=%+v err=%v", out, err)
	}

	assertExtraSchema(t, fn.Definition.Schema, false)
}

func TestFuncStrictArgumentsRejectsExtrasAtEveryStruct(t *testing.T) {
	calls := 0
	fn, err := Func(
		"lookup",
		"",
		func(context.Context, Call, extraInput) (Outcome, error) {
			calls++
			return Succeeded("ok"), nil
		},
		WithStrictArguments(),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertExtraSchema(t, fn.Definition.Schema, true)
	for _, args := range []string{
		`{"query":"yes","extra":1}`,
		`{"query":"yes","nested":{"label":"a","extra":1}}`,
		`{"query":"yes","rows":[{"label":"a","extra":1}]}`,
		`{"query":"yes","by_name":{"one":{"label":"a","extra":1}}}`,
		`{"query":"yes","regoin":"us"}`,
	} {
		out, err := fn.Executor.Execute(t.Context(), Call{
			ID: "call-1", Name: "lookup", Args: []byte(args),
		})
		if err != nil || out.State != StateNotExecuted || calls != 0 {
			t.Fatalf("%s: out=%+v err=%v calls=%d", args, out, err, calls)
		}
	}
	valid := Call{
		ID:   "call-1",
		Name: "lookup",
		Args: []byte(`{"query":"yes","counters":{"one":5}}`),
	}
	out, err := fn.Executor.Execute(t.Context(), valid)
	if err != nil || out.State != StateSucceeded || calls != 1 {
		t.Fatalf("valid strict call: out=%+v err=%v calls=%d", out, err, calls)
	}
}

func assertExtraSchema(t *testing.T, raw json.RawMessage, strict bool) {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	nested := properties["nested"].(map[string]any)
	rows := properties["rows"].(map[string]any)["items"].(map[string]any)
	byName := properties["by_name"].(map[string]any)
	byNameValue := byName["additionalProperties"].(map[string]any)
	for _, object := range []map[string]any{schema, nested, rows, byNameValue} {
		value, exists := object["additionalProperties"]
		if strict && (!exists || value != false) {
			t.Fatalf("strict struct schema is open: %s", raw)
		}
		if !strict && exists {
			t.Fatalf("default struct schema declares extra-key policy: %s", raw)
		}
	}
	counters := properties["counters"].(map[string]any)
	values := counters["additionalProperties"].(map[string]any)
	if values["type"] != "integer" {
		t.Fatalf("declared map value schema was lost: %s", raw)
	}
}
