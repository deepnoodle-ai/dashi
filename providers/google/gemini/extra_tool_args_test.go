package gemini

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestTypedToolArgumentPolicyReachesGeminiWire(t *testing.T) {
	client, err := New(Config{
		APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, strict := range []bool{false, true} {
		name := "default"
		options := []tool.FuncOption(nil)
		if strict {
			name = "strict"
			options = append(options, tool.WithStrictArguments())
		}
		t.Run(name, func(t *testing.T) {
			typed, err := tool.Func(
				"lookup", "",
				func(context.Context, tool.Call, struct {
					Query  string `json:"query"`
					Nested struct {
						Label string `json:"label"`
					} `json:"nested"`
				}) (tool.Outcome, error) {
					return tool.Succeeded("ok"), nil
				},
				options...,
			)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := client.Prepare(t.Context(), llm.Input{
				Model:    "gemini-test",
				Messages: []llm.Message{llm.UserMessage(llm.Text("find"))},
				Tools:    []tool.Definition{typed.Definition},
			})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(prepared.Body(), &body); err != nil {
				t.Fatal(err)
			}
			config := body["tools"].([]any)[0].(map[string]any)
			declarations := config["functionDeclarations"].([]any)
			wireTool := declarations[0].(map[string]any)
			wireSchema := wireTool["parametersJsonSchema"]
			var expected any
			if err := json.Unmarshal(typed.Definition.Schema, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(wireSchema, expected) {
				t.Fatalf(
					"Gemini wire changed schema: got=%#v want=%#v",
					wireSchema,
					expected,
				)
			}
		})
	}
}
