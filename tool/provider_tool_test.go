package tool

import (
	"encoding/json"
	"testing"
)

func TestDefinitionValidateProviderToolRequiresBoundOptions(t *testing.T) {
	tests := []struct {
		name       string
		definition Definition
		valid      bool
	}{
		{
			name: "provider tool without schema",
			definition: Definition{
				Name: "web_search",
				Kind: KindProviderTool,
				ProviderTool: &ProviderToolSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
					Options:  json.RawMessage(`{"max_uses":5}`),
				},
			},
			valid: true,
		},
		{
			name: "provider tool with serialized missing schema",
			definition: Definition{
				Name:   "web_search",
				Kind:   KindProviderTool,
				Schema: json.RawMessage(`null`),
				ProviderTool: &ProviderToolSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
				},
			},
			valid: true,
		},
		{
			name: "provider tool with schema",
			definition: Definition{
				Name:   "web_search",
				Kind:   KindProviderTool,
				Schema: json.RawMessage(`{"type":"object"}`),
				ProviderTool: &ProviderToolSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
				},
			},
		},
		{
			name: "local with provider tool options",
			definition: Definition{
				Name:   "web_search",
				Schema: json.RawMessage(`{"type":"object"}`),
				ProviderTool: &ProviderToolSpec{
					Provider: "anthropic",
					API:      "anthropic.messages",
					Type:     "web_search_20250305",
				},
			},
		},
		{
			name: "local with null schema",
			definition: Definition{
				Name:   "lookup",
				Kind:   KindLocal,
				Schema: json.RawMessage(`null`),
			},
		},
		{
			name: "local without schema",
			definition: Definition{
				Name: "lookup",
				Kind: KindLocal,
			},
		},
		{
			name: "malformed options",
			definition: Definition{
				Name: "web_search",
				Kind: KindProviderTool,
				ProviderTool: &ProviderToolSpec{
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
				t.Fatalf(
					"definition validity=%t want=%t: %v",
					err == nil,
					tt.valid,
					err,
				)
			}
		})
	}
}

func TestProviderToolDefinitionValidatesAfterJSONRoundTrip(t *testing.T) {
	definition := Definition{
		Name: "web_search",
		Kind: KindProviderTool,
		ProviderTool: &ProviderToolSpec{
			Provider: "anthropic",
			API:      "anthropic.messages",
			Type:     "web_search_20250305",
		},
	}
	raw, err := json.Marshal(definition.Clone())
	if err != nil {
		t.Fatal(err)
	}
	var saved Definition
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if string(saved.Schema) != "null" {
		t.Fatalf("saved schema=%s; want null", saved.Schema)
	}
	if err := saved.Validate(); err != nil {
		t.Fatalf("saved provider tool definition became invalid: %v", err)
	}
}

func TestDefinitionCloneCopiesProviderToolOptions(t *testing.T) {
	definition := Definition{
		Name: "web_search",
		Kind: KindProviderTool,
		ProviderTool: &ProviderToolSpec{
			Provider: "anthropic",
			API:      "anthropic.messages",
			Type:     "web_search_20250305",
			Options:  json.RawMessage(`{"max_uses":5}`),
		},
	}
	clone := definition.Clone()
	clone.ProviderTool.Options[0] = '['
	if !json.Valid(definition.ProviderTool.Options) {
		t.Fatal("provider tool options shared caller bytes")
	}
}
