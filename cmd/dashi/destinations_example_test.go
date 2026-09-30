package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi/llm"
)

// This is an offline registration and preparation workflow, with no Send.
func Example_configuredDestinations() {
	var settings preferenceFile
	err := json.Unmarshal([]byte(`{"providers": {
   "local": {"api":"openai.completions","endpoint":"http://127.0.0.1:11434/v1/chat/completions","models":["qwen2.5-coder:7b"],"default_model":"qwen2.5-coder:7b","credential_env":""},
   "team": {"api":"openai.responses","endpoint":"https://proxy.example/v1/responses","models":["acme/code-v1"],"default_model":"acme/code-v1","credential_env":"TEAM_LLM_KEY"}
 }}`), &settings)
	if err != nil {
		panic(err)
	}
	registry, err := mergeDestinations(settings)
	if err != nil {
		panic(err)
	}
	for _, id := range []string{"local", "team"} {
		selection, err := resolveSelection(registry, []selectionTier{{modelSelection: modelSelection{Provider: id}, Source: "example"}})
		if err != nil {
			panic(err)
		}
		client, err := newDestinationClient(selection.Destination, func(string) (string, bool) { return "fixture-key", true }, true)
		if err != nil {
			panic(err)
		}
		request, err := client.Prepare(context.Background(), llm.Input{Model: selection.Model, Messages: []llm.Message{llm.UserMessage(llm.Text("Prepare without dispatch."))}})
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s: %s; prepared, not dispatched\n", id, request.Snapshot().Input.Model)
	}
	// Output:
	// local: qwen2.5-coder:7b; prepared, not dispatched
	// team: acme/code-v1; prepared, not dispatched
}
