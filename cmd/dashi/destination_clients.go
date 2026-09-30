package main

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/deepinfra"
	"github.com/deepnoodle-ai/dashi/providers/google/gemini"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/mistral"
	"github.com/deepnoodle-ai/dashi/providers/ollama"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/providers/openrouter"
)

func builtinDestinations() map[string]configuredDestination {
	out := make(map[string]configuredDestination)
	for _, binding := range []struct {
		id, api, endpoint string
		aliases           []string
	}{
		{"anthropic", messages.API, "https://api.anthropic.com/v1/messages", []string{"ANTHROPIC_API_KEY"}},
		{"google", "google.generate_content", "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
		{"openai", responses.API, "https://api.openai.com/v1/responses", []string{"OPENAI_API_KEY"}},
		{"grok", responses.API, grok.DefaultEndpoint, []string{"XAI_API_KEY", "GROK_API_KEY"}},
		{"mistral", completions.API, mistral.DefaultEndpoint, []string{"MISTRAL_API_KEY"}},
		{"deepinfra", completions.API, deepinfra.DefaultEndpoint, []string{"DEEP_INFRA_API_KEY", "DEEPINFRA_API_KEY", "DEEPINFRA_TOKEN"}},
		{"meta", responses.API, meta.DefaultEndpoint, []string{"MODEL_API_KEY", "META_API_KEY"}},
		{"openrouter", completions.API, openrouter.DefaultEndpoint, []string{"OPENROUTER_API_KEY"}},
		{"ollama", messages.API, "http://127.0.0.1:11434/v1/messages", []string{"OLLAMA_API_KEY"}},
	} {
		d := configuredDestination{ID: binding.id, Source: "built-in", BuiltinAliases: binding.aliases, Definition: destinationDefinition{API: binding.api, Endpoint: binding.endpoint}}
		for _, entry := range catalog.Entries(binding.id) {
			if slices.Contains(entry.APIs, binding.api) {
				d.Definition.Models = append(d.Definition.Models, entry.Model)
			}
		}
		d.Definition.DefaultModel, _ = catalog.Default(binding.id)
		out[d.ID] = d
	}
	return out
}

// mergeDestinations replaces whole definitions. Native identities may register
// models only on their native binding; custom endpoints need a distinct name.
func mergeDestinations(files ...preferenceFile) (map[string]configuredDestination, error) {
	registry := builtinDestinations()
	for _, file := range files {
		for id, definition := range file.Providers {
			d := configuredDestination{ID: id, Definition: definition, Source: file.Source, Project: file.Project}
			if builtin, ok := builtinDestinations()[id]; ok {
				if definition.API != builtin.Definition.API || definition.Endpoint != builtin.Definition.Endpoint || definition.CredentialEnv != builtin.BuiltinAliases[0] {
					return nil, fmt.Errorf("destination %q: a changed native binding needs a custom identity", id)
				}
				// Native identities retain their documented credential aliases.
				d.BuiltinAliases = slices.Clone(builtin.BuiltinAliases)
			}
			if err := d.validate(); err != nil {
				return nil, err
			}
			registry[id] = d
		}
	}
	return registry, nil
}
func detectedSelection(registry, user map[string]configuredDestination, lookup func(string) (string, bool)) selectionTier {
	for _, id := range []string{"anthropic", "google", "openai", "grok", "mistral", "deepinfra", "meta"} {
		d := registry[id]
		if len(d.Definition.Models) == 0 || !d.trustedBy(user) {
			continue
		}
		for _, name := range d.BuiltinAliases {
			if key, ok := lookup(name); ok && key != "" {
				return selectionTier{modelSelection: modelSelection{Provider: id}, Source: "credential detection"}
			}
		}
	}
	return selectionTier{modelSelection: modelSelection{Provider: "openai"}, Source: "default"}
}

// newDestinationClient runs binding checks before credential lookup or adapter
// construction. Custom routes never enter a native wrapper's key defaults.
func newDestinationClient(d configuredDestination, lookup func(string) (string, bool), trusted bool) (llm.Client, error) {
	key, err := d.credential(lookup, trusted)
	if err != nil {
		return nil, err
	}
	transport := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var client llm.Client
	if len(d.BuiltinAliases) != 0 {
		switch d.ID {
		case "grok":
			client, err = grok.New(grok.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "mistral":
			client, err = mistral.New(mistral.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "meta":
			client, err = meta.New(meta.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "deepinfra":
			client, err = deepinfra.New(deepinfra.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "openrouter":
			client, err = openrouter.New(openrouter.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "ollama":
			if key == "" {
				key = "ollama"
			}
			client, err = ollama.New(ollama.Config{APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		}
	}
	if client == nil && err == nil {
		switch d.Definition.API {
		case responses.API:
			client, err = responses.New(responses.Config{Provider: d.ID, APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case completions.API:
			client, err = completions.New(completions.Config{Provider: d.ID, APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case messages.API:
			client, err = messages.New(messages.Config{Provider: d.ID, APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		case "google.generate_content":
			client, err = gemini.New(gemini.Config{Provider: d.ID, APIKey: key, Endpoint: d.Definition.Endpoint, Client: transport})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("destination %q: adapter configuration rejected", d.ID)
	}
	if client == nil {
		return nil, fmt.Errorf("destination %q: unsupported api", d.ID)
	}
	return client, nil
}
