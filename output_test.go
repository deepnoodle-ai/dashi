package dashi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type testPerson struct {
	Name string   `json:"name" description:"Full name"`
	Tags []string `json:"tags,omitempty"`
}

func TestMustOutputSchema(t *testing.T) {
	raw := MustOutputSchema[testPerson]()
	agent := Agent{
		ID:           "a",
		Version:      "1",
		Provider:     "p",
		API:          "api",
		Model:        "m",
		OutputSchema: raw,
	}
	if err := agent.Validate(); err != nil {
		t.Fatalf("generated schema rejected: %v", err)
	}
	var schema struct {
		Required             []string
		AdditionalProperties *bool `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" ||
		schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("schema = %s", raw)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("non-object type did not panic")
		}
	}()
	MustOutputSchema[[]string]()
}

func TestDecodeOutput(t *testing.T) {
	ada := &StructuredOutput{Value: json.RawMessage(`{"name":"Ada"}`)}
	completed := *testStop(StopCompleted, "")
	person, err := DecodeOutput[testPerson](outcomeOf(completed, ada))
	if err != nil || person.Name != "Ada" {
		t.Fatalf("person=%+v err=%v", person, err)
	}

	limited := *testStop(StopLimited, LimitModelCalls)
	_, err = DecodeOutput[testPerson](outcomeOf(limited, nil))
	if !errors.Is(err, ErrNoOutput) ||
		!strings.Contains(err.Error(), "limited: model_calls") {
		t.Fatalf("limited err = %v", err)
	}
	_, err = DecodeOutput[testPerson](outcomeOf(completed, nil))
	if !errors.Is(err, ErrNoOutput) ||
		!strings.Contains(err.Error(), "without an output schema") {
		t.Fatalf("completed err = %v", err)
	}
	_, err = DecodeOutput[int](outcomeOf(completed, ada))
	if err == nil || errors.Is(err, ErrNoOutput) {
		t.Fatalf("mismatched type err = %v", err)
	}
}

// outcomeOf builds the result Run returns, whose Stop and Output are promoted
// from the embedded TurnView.
func outcomeOf(stop Stop, output *StructuredOutput) RunResult {
	return RunResult{TurnView: TurnView{Stop: stop, Output: output}}
}
