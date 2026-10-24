package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerationZeroFalseAndOmissionOverrideLowerTiers(t *testing.T) {
	var high, low generationPreferences
	if err := json.Unmarshal([]byte(`{"thinking_effort":"","show_thinking":false,"temperature":0,"max_tokens":0}`), &high); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"thinking_effort":"high","show_thinking":true,"temperature":0.7,"max_tokens":999}`), &low); err != nil {
		t.Fatal(err)
	}
	result, err := resolveControls([]generationTier{{high, "flag"}, {low, "user"}})
	if err != nil || result.ShowThinking || result.Settings.Reasoning != nil || result.Settings.Temperature == nil || *result.Settings.Temperature != 0 || result.Settings.MaxTokens != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := result.Sources["temperature"]; got.Winning != "flag" || len(got.Overridden) != 1 || got.Overridden[0] != "user" {
		t.Fatalf("source=%+v", got)
	}
	if err := json.Unmarshal([]byte(`{"temperature":null}`), &high); err != nil {
		t.Fatal(err)
	}
	result, err = resolveControls([]generationTier{{high, "project"}, {low, "user"}})
	if err != nil || result.Settings.Temperature != nil {
		t.Fatalf("temperature omission=%+v %v", result, err)
	}
}
func TestSettingsRejectInlineCredentialsWithoutEcho(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	data := `{"providers":{"team":{"api":"openai.responses","endpoint":"https://proxy.example/v1/responses","models":["acme/code-v1"],"default_model":"acme/code-v1","api_key":"private-fixture-key"}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readPreferenceFile(path, false)
	if err == nil || strings.Contains(err.Error(), "private-fixture-key") {
		t.Fatalf("unsafe settings: %v", err)
	}
}
func TestSettingsRetainSelectionAndExplicitControlPresence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	data := `{"provider":"team","model":"acme/code-v1","show_thinking":false,"temperature":null,"unrelated":{"secret":"hidden"},"providers":{"team":{"api":"openai.responses","endpoint":"https://proxy.example/v1/responses","models":["acme/code-v1"],"default_model":"acme/code-v1","credential_env":"TEAM_KEY"}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	prefs, err := readPreferenceFile(path, true)
	if err != nil || prefs.Provider != "team" || !prefs.ShowThinking.Set || prefs.ShowThinking.Value || !prefs.Temperature.Null || !prefs.Project || len(prefs.Providers) != 1 {
		t.Fatalf("prefs=%+v err=%v", prefs, err)
	}
}

func TestParseFeaturedModelsIgnoresTheWrongShape(t *testing.T) {
	for _, raw := range []string{`[]`, `{"anthropic":"claude-opus-5-5"}`, `{"anthropic":[1]}`, `"x"`} {
		if lists, problem := parseFeaturedModels(json.RawMessage(raw)); lists != nil || problem == "" {
			t.Fatalf("%s: lists=%v problem=%q", raw, lists, problem)
		}
	}
	for _, raw := range []string{``, `null`} {
		if lists, problem := parseFeaturedModels(json.RawMessage(raw)); lists != nil || problem != "" {
			t.Fatalf("%s: lists=%v problem=%q", raw, lists, problem)
		}
	}
}

func TestMalformedFeaturedModelsNeverFailStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"featured_models":{"anthropic":"claude-opus-5-5"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreferenceFile(path, false); err != nil {
		t.Fatal(err)
	}
}
