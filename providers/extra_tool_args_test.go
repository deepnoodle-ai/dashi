package providers_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestTypedToolArgumentPolicyReachesProviderWire(t *testing.T) {
	type arguments struct {
		Query  string `json:"query"`
		Nested struct {
			Label string `json:"label"`
		} `json:"nested"`
	}
	routes := []struct {
		name string
		new  func(*testing.T) llm.Client
		at   func(map[string]any) (any, map[string]any)
	}{
		{
			name: "openai responses",
			new: func(t *testing.T) llm.Client {
				p, err := responses.New(responses.Config{
					APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust,
				})
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
			at: func(body map[string]any) (any, map[string]any) {
				tool := body["tools"].([]any)[0].(map[string]any)
				return tool["parameters"], tool
			},
		},
		{
			name: "openai completions",
			new: func(t *testing.T) llm.Client {
				p, err := completions.New(completions.Config{
					APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust,
				})
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
			at: func(body map[string]any) (any, map[string]any) {
				wireTool := body["tools"].([]any)[0].(map[string]any)
				function := wireTool["function"].(map[string]any)
				return function["parameters"], function
			},
		},
		{
			name: "anthropic messages",
			new: func(t *testing.T) llm.Client {
				p, err := messages.New(messages.Config{
					APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust,
				})
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
			at: func(body map[string]any) (any, map[string]any) {
				tool := body["tools"].([]any)[0].(map[string]any)
				return tool["input_schema"], tool
			},
		},
	}
	for _, route := range routes {
		for _, strict := range []bool{false, true} {
			name := route.name + "/default"
			options := []tool.FuncOption(nil)
			if strict {
				name = route.name + "/strict"
				options = append(options, tool.WithStrictArguments())
			}
			t.Run(name, func(t *testing.T) {
				definition, err := tool.Func(
					"lookup", "",
					func(context.Context, tool.Call, arguments) (tool.Outcome, error) {
						return tool.Succeeded("ok"), nil
					},
					options...,
				)
				if err != nil {
					t.Fatal(err)
				}
				input := llm.Input{
					Model:    "fixture",
					Messages: []llm.Message{llm.UserMessage(llm.Text("find"))},
					Tools:    []tool.Definition{definition.Definition},
				}
				prepared, err := route.new(t).Prepare(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				if err := json.Unmarshal(prepared.Body(), &body); err != nil {
					t.Fatal(err)
				}
				wireSchema, wireTool := route.at(body)
				var expected any
				if err := json.Unmarshal(
					definition.Definition.Schema,
					&expected,
				); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(wireSchema, expected) {
					t.Fatalf("wire schema changed: got=%#v want=%#v", wireSchema, expected)
				}
				if route.name == "openai responses" && wireTool["strict"] != false {
					t.Fatalf("provider sampling mode changed: %#v", wireTool)
				}
			})
		}
	}
}
