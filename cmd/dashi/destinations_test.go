package main

import (
	"strings"
	"testing"
)

func fixtureDestination(id, endpoint, key string) configuredDestination {
	return configuredDestination{ID: id, Definition: destinationDefinition{API: "openai.responses", Endpoint: endpoint, Models: []string{"acme/code-v1", "shared"}, DefaultModel: "acme/code-v1", CredentialEnv: key}, Source: "fixture"}
}
func TestDestinationRejectsUnsafeTransportBeforeCredentials(t *testing.T) {
	for _, endpoint := range []string{"http://remote.example/v1/responses", "http://localhost/v1/responses", "http://[::ffff:127.0.0.1]/v1/responses", "http://[::1%25lo0]/v1/responses", "https://user:secret@example.test/v1/responses", "https://example.test/v1/responses?key=secret"} {
		for _, value := range []string{"", "fixture-key"} {
			d := fixtureDestination("team", endpoint, "TEAM_LLM_KEY")
			lookups := 0
			_, err := d.credential(func(string) (string, bool) { lookups++; return value, true }, true)
			if err == nil || lookups != 0 {
				t.Fatalf("%q: err=%v lookups=%d", endpoint, err, lookups)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("rejected URL leaked: %v", err)
			}
		}
	}
}
func TestDestinationLiteralLoopbackAndKeylessCustom(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1/v1/responses", "http://127.5.6.7/v1/responses", "http://[::1]:1234/v1/responses", "https://proxy.example/v1/responses"} {
		d := fixtureDestination("team", endpoint, "TEAM_LLM_KEY")
		var names []string
		key, err := d.credential(func(name string) (string, bool) { names = append(names, name); return "fixture", true }, true)
		if err != nil || key != "fixture" || len(names) != 1 || names[0] != "TEAM_LLM_KEY" {
			t.Fatalf("%q: %q %v %v", endpoint, key, names, err)
		}
	}
	d := fixtureDestination("local", "http://localhost/v1/responses", "")
	key, err := d.credential(func(name string) (string, bool) { t.Fatalf("implicit credential lookup %s", name); return "", false }, true)
	if err != nil || key != "" {
		t.Fatalf("keyless: %q %v", key, err)
	}
}
func TestProjectBindingRequiresCompleteTrust(t *testing.T) {
	d := fixtureDestination("team", "https://proxy.example/v1/responses", "TEAM_LLM_KEY")
	user := map[string]configuredDestination{"team": d}
	d.Project = true
	if !d.trustedBy(user) {
		t.Fatal("same binding rejected")
	}
	d.Definition.CredentialEnv = "OTHER_KEY"
	if d.trustedBy(user) {
		t.Fatal("changed credential binding inherited trust")
	}
	_, err := d.credential(func(string) (string, bool) { t.Fatal("untrusted lookup"); return "", false }, false)
	if err == nil {
		t.Fatal("missing trust accepted")
	}
}
func TestSelectionDoesNotCombineTiersOrGuessModels(t *testing.T) {
	team := fixtureDestination("team", "https://proxy.example/v1/responses", "TEAM_LLM_KEY")
	local := fixtureDestination("local", "http://127.0.0.1/v1/responses", "")
	local.Definition.Models, local.Definition.DefaultModel = []string{"qwen:7b", "shared"}, "qwen:7b"
	registry := map[string]configuredDestination{"team": team, "local": local}
	result, err := resolveSelection(registry, []selectionTier{{modelSelection: modelSelection{Provider: "local"}, Source: "flag"}, {modelSelection: modelSelection{Provider: "team", Model: "acme/code-v1"}, Source: "user"}})
	if err != nil || result.Destination.ID != "local" || result.Model != "qwen:7b" || result.Source != "flag" || len(result.Overridden) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, model := range []string{"shared", "acme/code", "gpt-unknown"} {
		if _, err := resolveSelection(registry, []selectionTier{{modelSelection: modelSelection{Model: model}, Source: "flag"}}); err == nil {
			t.Fatalf("model %q accepted", model)
		}
	}
	result, err = resolveSelection(registry, []selectionTier{{modelSelection: modelSelection{Model: "acme/code-v1"}, Source: "environment"}})
	if err != nil || result.Destination.ID != "team" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
