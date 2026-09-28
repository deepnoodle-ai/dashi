package responses

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestPrepareForwardsNullableTypedToolSchema(t *testing.T) {
	typed, err := tool.Func(
		"find",
		"",
		func(_ context.Context, _ tool.Call, _ struct {
			Region *string `json:"region,omitempty"`
		}) (tool.Outcome, error) {
			return tool.Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Find"))},
			Tools:    []tool.Definition{typed.Definition},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(typed.Definition.Schema, &expected); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			Parameters map[string]any `json:"parameters"`
			Strict     bool           `json:"strict"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Strict ||
		!reflect.DeepEqual(body.Tools[0].Parameters, expected) {
		t.Fatalf("typed tool schema changed in request: %s", request.Body())
	}
}
