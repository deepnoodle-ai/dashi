package providers_test

import (
	"bytes"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

func TestContextIdentityDoesNotEnterProviderBodies(t *testing.T) {
	providers := cases(t)
	providers = append(providers, providerCase{name: "openai-completions", new: func(endpoint string) llm.Client {
		client, err := completions.New(completions.Config{Provider: "openai", APIKey: "test", Endpoint: endpoint, CapabilityPolicy: llm.CapabilityAdjust})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}})
	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("http://localhost:1")
			input := modelInput()
			plain, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			input.Messages[0].Context = &llm.MessageContext{Origin: "runtime", Name: "private-slot-name"}
			identified, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(plain.Body(), identified.Body()) {
				t.Fatalf("metadata changed wire body: %s", identified.Body())
			}
			if identified.Input().Messages[0].Context == nil || identified.Input().Messages[0].Context.Name != "private-slot-name" {
				t.Fatal("original request lost metadata")
			}
		})
	}
}
