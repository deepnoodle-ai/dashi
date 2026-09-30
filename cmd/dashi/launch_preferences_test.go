package main

import (
	"strings"
	"testing"
)

func TestLaunchPairAndPerFieldPrecedence(t *testing.T) {
	registry := builtinDestinations()
	local := configuredDestination{ID: "local", Definition: destinationDefinition{API: "openai.completions", Endpoint: "http://127.0.0.1:11434/v1/chat/completions", Models: []string{"qwen/exact"}, DefaultModel: "qwen/exact"}}
	registry["local"] = local
	user := preferenceFile{modelSelection: modelSelection{Provider: "openai", Model: "gpt-6-luna"}, generationPreferences: generationPreferences{Temperature: preferenceField[float64]{Set: true, Value: 0.7}, ShowThinking: preferenceField[bool]{Set: true, Value: true}}, Source: "user file"}
	project := preferenceFile{generationPreferences: generationPreferences{MaxTokens: preferenceField[int]{Set: true, Value: 123}}, Source: "project file"}
	reopened := preferenceFile{modelSelection: modelSelection{Provider: "openai", Model: "gpt-6-astra"}, generationPreferences: generationPreferences{ShowThinking: preferenceField[bool]{Set: true, Value: true}, Temperature: preferenceField[float64]{Set: true, Value: 0.4}}}
	environment := map[string]string{"DASHI_MODEL": "gpt-6-luna", "DASHI_SHOW_THINKING": "false", "DASHI_THINKING_EFFORT": "omit"}
	lookup := func(name string) (string, bool) { value, present := environment[name]; return value, present }
	o := Options{Provider: "local", Controls: generationPreferences{Temperature: preferenceField[float64]{Set: true, Value: 0}}}
	plan, err := resolveLaunchPreferences(o, registry, registry, user, project, reopened, lookup, map[string]string{"DASHI_SHOW_THINKING": "/explicit.env"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selection.Destination.ID != "local" || plan.Selection.Model != "qwen/exact" || plan.Selection.Source != "flags" {
		t.Fatalf("pair split across tiers: %+v", plan.Selection)
	}
	if plan.Controls.Settings.Temperature == nil || *plan.Controls.Settings.Temperature != 0 || plan.Controls.ShowThinking || plan.Controls.Settings.Reasoning != nil || plan.Controls.Settings.MaxTokens != 123 {
		t.Fatalf("controls = %+v", plan.Controls)
	}
	if !strings.Contains(plan.Controls.Sources["show_thinking"].Winning, "/explicit.env") || plan.Controls.Sources["temperature"].Winning != "flags" {
		t.Fatalf("sources=%+v", plan.Controls.Sources)
	}
	if len(plan.Controls.Sources["temperature"].Overridden) != 2 {
		t.Fatal("overridden sources lost")
	}
}

func TestEndpointOverrideRequiresCustomIdentityAndSafeTransport(t *testing.T) {
	registry := builtinDestinations()
	registry["team"] = configuredDestination{ID: "team", Definition: destinationDefinition{API: "openai.responses", Endpoint: "https://team.test/responses", Models: []string{"exact"}, DefaultModel: "exact", CredentialEnv: "TEAM_KEY"}}
	for _, o := range []Options{
		{Model: "exact", APIEndpoint: "https://other.test/responses"},
		{Provider: "openai", APIEndpoint: "https://other.test/responses"},
		{Provider: "team", APIEndpoint: "http://remote.test/responses"},
	} {
		if _, err := resolveLaunchPreferences(o, registry, registry, preferenceFile{}, preferenceFile{}, preferenceFile{}, func(string) (string, bool) { return "", false }, nil); err == nil {
			t.Fatalf("unsafe override accepted: %+v", o)
		}
	}
}
