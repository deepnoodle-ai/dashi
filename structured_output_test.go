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
		Revision:     "1",
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
	person, err := DecodeOutput[testPerson](Outcome{
		Stop:   Stop{Kind: StopCompleted},
		Output: &StructuredOutput{Value: json.RawMessage(`{"name":"Ada"}`)},
	})
	if err != nil || person.Name != "Ada" {
		t.Fatalf("person=%+v err=%v", person, err)
	}

	_, err = DecodeOutput[testPerson](Outcome{
		Stop: Stop{Kind: StopLimited, Reason: "model_attempts"},
	})
	if !errors.Is(err, ErrNoOutput) ||
		!strings.Contains(err.Error(), "limited: model_attempts") {
		t.Fatalf("limited err = %v", err)
	}
	_, err = DecodeOutput[testPerson](Outcome{Stop: Stop{Kind: StopCompleted}})
	if !errors.Is(err, ErrNoOutput) ||
		!strings.Contains(err.Error(), "without an output schema") {
		t.Fatalf("completed err = %v", err)
	}
	_, err = DecodeOutput[int](Outcome{
		Stop:   Stop{Kind: StopCompleted},
		Output: &StructuredOutput{Value: json.RawMessage(`{"name":"Ada"}`)},
	})
	if err == nil || errors.Is(err, ErrNoOutput) {
		t.Fatalf("mismatched type err = %v", err)
	}
}
