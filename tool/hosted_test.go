package tool

import (
	"encoding/json"
	"testing"
)

func TestDefinitionValidateHostedRequiresBoundOptions(t *testing.T) {
	tests := []struct {
		name       string
		definition Definition
		valid      bool
	}{
		{
			name: "hosted without schema",
			definition: Definition{
				Name: "web_search",
				Kind: KindProviderHosted,
				Hosted: &HostedSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
					Options:  json.RawMessage(`{"max_uses":5}`),
				},
			},
			valid: true,
		},
		{
			name: "hosted with schema",
			definition: Definition{
				Name:   "web_search",
				Kind:   KindProviderHosted,
				Schema: json.RawMessage(`{"type":"object"}`),
				Hosted: &HostedSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
				},
			},
		},
		{
			name: "local with hosted options",
			definition: Definition{
				Name:   "web_search",
				Schema: json.RawMessage(`{"type":"object"}`),
				Hosted: &HostedSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
				},
			},
		},
		{
			name: "malformed options",
			definition: Definition{
				Name: "web_search",
				Kind: KindProviderHosted,
				Hosted: &HostedSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
					Options:  json.RawMessage(`[]`),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.definition.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("definition validity=%t want=%t: %v", err == nil, tt.valid, err)
			}
		})
	}
}

func TestDefinitionCloneCopiesHostedOptions(t *testing.T) {
	definition := Definition{
		Name: "web_search",
		Kind: KindProviderHosted,
		Hosted: &HostedSpec{
			Provider: "anthropic",
			API:      "anthropic.messages",
			Type:     "web_search_20250305",
			Options:  json.RawMessage(`{"max_uses":5}`),
		},
	}
	clone := definition.Clone()
	clone.Hosted.Options[0] = '['
	if !json.Valid(definition.Hosted.Options) {
		t.Fatal("hosted options shared caller bytes")
	}
}
