package main

import (
	"slices"
	"strings"
	"testing"
)

// builtinPickerPreferences uses the built-in destinations with only an
// Anthropic key set, and selects a model that is not featured.
func builtinPickerPreferences(t *testing.T) *cliPreferences {
	t.Helper()
	_, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("picker dispatched"); return 0, "" })
	p.routes.registry = builtinDestinations()
	p.routes.lookup = func(name string) (string, bool) { return "fixture", name == "ANTHROPIC_API_KEY" }
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["anthropic"], Model: "claude-opus-4-8", Source: "fixture"}
	return p
}

func TestModelChoicesShowFeaturedModelsOfReadyDestinations(t *testing.T) {
	p := builtinPickerPreferences(t)
	menu := p.modelChoices(false)
	var models []string
	for _, choice := range menu.choices[:len(menu.choices)-1] {
		if choice.provider != "anthropic" {
			t.Fatalf("destination without a credential listed: %+v", choice)
		}
		models = append(models, choice.model)
	}
	want := append(slices.Clone(p.routes.registry["anthropic"].FeaturedModels), "claude-opus-4-8")
	if !slices.Equal(models, want) {
		t.Fatalf("models=%v want %v", models, want)
	}
	current := menu.choices[menu.selected]
	if current.model != "claude-opus-4-8" || !strings.Contains(current.note, "current") || !strings.HasPrefix(current.label, "anthropic  ") {
		t.Fatalf("selected=%+v", current)
	}
	if !strings.Contains(menu.choices[0].note, "1M") {
		t.Fatalf("no context window: %+v", menu.choices[0])
	}
	last := menu.choices[len(menu.choices)-1]
	if last.provider != "" || last.label != "All models…" {
		t.Fatalf("last row=%+v", last)
	}
	if len(menu.setup) != 1 || !strings.HasPrefix(menu.setup[0], "Set GEMINI_API_KEY (google), ") || !strings.HasSuffix(menu.setup[0], ", or OPENAI_API_KEY (openai) to add more models") {
		t.Fatalf("setup=%v", menu.setup)
	}
	for _, line := range menu.setup {
		if strings.Contains(line, "deepinfra") {
			t.Fatalf("destination without models has a setup line: %v", menu.setup)
		}
	}
}

func TestModelChoicesCompleteListShowsEveryModel(t *testing.T) {
	p := builtinPickerPreferences(t)
	menu := p.modelChoices(true)
	total := 0
	for _, d := range p.routes.registry {
		total += len(d.Definition.Models)
	}
	if len(menu.choices) != total || len(menu.setup) != 0 {
		t.Fatalf("choices=%d want %d, setup=%v", len(menu.choices), total, menu.setup)
	}
	if menu.choices[0].provider != "anthropic" || menu.choices[menu.selected].model != "claude-opus-4-8" {
		t.Fatalf("first=%+v selected=%+v", menu.choices[0], menu.choices[menu.selected])
	}
	last := menu.choices[len(menu.choices)-1]
	if last.provider == "anthropic" || !strings.Contains(last.note, "needs ") {
		t.Fatalf("destination that is not ready must sort last with its reason: %+v", last)
	}
}

func TestModelChoicesListUserDestinationsInFull(t *testing.T) {
	p := builtinPickerPreferences(t)
	user := p.routes.registry["anthropic"]
	user.FeaturedModels = nil // a destination from user settings
	p.routes.registry["anthropic"] = user
	listed := 0
	for _, choice := range p.modelChoices(false).choices {
		if choice.provider == "anthropic" {
			listed++
		}
	}
	if listed != len(user.Definition.Models) {
		t.Fatalf("listed=%d want %d", listed, len(user.Definition.Models))
	}
}

func TestModelChoicesNameOneMissingKey(t *testing.T) {
	p := builtinPickerPreferences(t)
	p.routes.lookup = func(name string) (string, bool) {
		return "fixture", name != "GEMINI_API_KEY" && name != "GOOGLE_API_KEY"
	}
	if setup := p.modelChoices(false).setup; !slices.Equal(setup, []string{"Set GEMINI_API_KEY to add google models"}) {
		t.Fatalf("setup=%v", setup)
	}
}
