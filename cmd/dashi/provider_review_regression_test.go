package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
)

func TestExplicitEmptySelectorsCannotDispatchDefaults(t *testing.T) {
	for _, flag := range []string{"--model", "--provider", "--api-endpoint", "--workspace", "--env-file"} {
		for _, args := range [][]string{{flag + "="}, {flag, ""}, {flag, " "}} {
			if _, err := parseArgs(args); err == nil {
				t.Fatalf("empty explicit %s accepted", flag)
			}
		}
	}
}
func TestNativeGeminiFactsAgreeAcrossCLISurfaces(t *testing.T) {
	d := builtinDestinations()["google"]
	client, err := newDestinationClient(d, func(string) (string, bool) { return "fixture", true }, true)
	if err != nil {
		t.Fatal(err)
	}
	expected, known := client.ModelInfo(d.Definition.DefaultModel)
	if !known || expected.Capabilities.ContextWindow == 0 {
		t.Fatal("fixture has no qualified window")
	}
	var inventory bytes.Buffer
	if err := modelInventory(map[string]configuredDestination{"google": d}, nil, func(string) (string, bool) { return "fixture", true }, false, &inventory); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inventory.String(), expected.Evidence.Source) || !strings.Contains(inventory.String(), knownLimit(expected.Capabilities.ContextWindow)) {
		t.Fatalf("inventory=%s", inventory.String())
	}
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("inspection dispatched"); return 0, "" })
	p.routes.registry = map[string]configuredDestination{"google": d}
	p.routes.lookup = func(string) (string, bool) { return "fixture", true }
	p.routes.factory = newDestinationClient
	p.launch.Selection = resolvedSelection{Destination: d, Model: d.Definition.DefaultModel, Source: "fixture"}
	view := e.inspectSettings(t.Context())
	if view.Knowledge == nil || view.Knowledge.ContextWindow != expected.Capabilities.ContextWindow {
		t.Fatalf("inspection=%+v", view)
	}
	found := false
	for _, choice := range p.modelChoices() {
		if choice.model == d.Definition.DefaultModel {
			found = true
			if !strings.Contains(choice.detail, expected.Evidence.Source) {
				t.Fatalf("picker=%+v", choice)
			}
		}
	}
	if !found {
		t.Fatal("default absent from picker")
	}
	window, known := e.routeCompactor(p.agent()).Policy.Window()
	if !known || window != expected.Capabilities.ContextWindow {
		t.Fatalf("compaction window=%d known=%t", window, known)
	}
	custom := d
	custom.Definition.Endpoint = "https://proxy.example/models/{model}:generateContent"
	if _, known := custom.modelInfo(d.Definition.DefaultModel); known {
		t.Fatal("custom proxy inherited native evidence")
	}
}
func TestStartupAdjustmentsVisibleBeforeSend(t *testing.T) {
	ui := newRecorderUI()
	beforeSend := ""
	e, p := routedTestEngine(t, func(string) (int, string) { beforeSend = ui.text(); return 200, "answer" })
	e.cfg.Emit = ui.Emit
	p.launch.Controls.Settings.Temperature = new(3.0)
	if result, err := e.Prompt(t.Context(), "temperature from startup"); err != nil || result.Outcome.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("result=%+v %v", result, err)
	}
	if !strings.Contains(beforeSend, "temperature 3 → 2") || !strings.Contains(beforeSend, "Unverified support:") {
		t.Fatalf("before Send=%s", beforeSend)
	}
}
func TestPickerPrioritizesUsableBindingsAndConfirmsControlValues(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("preview dispatched"); return 0, "" })
	unavailable := p.routes.registry["one"]
	unavailable.Definition.CredentialEnv = "MISSING_KEY"
	p.routes.registry["one"] = unavailable
	choices := p.modelChoices()
	if len(choices) != 2 || choices[0].provider != "two" || !choices[0].available || choices[1].available {
		t.Fatalf("choices=%+v", choices)
	}
	ui := newRecorderUI()
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "fixture"}
	ctrl.generationControl(t.Context(), "/temperature", "0")
	ctrl.generationControl(t.Context(), "/thinking", "on")
	if !strings.Contains(ui.text(), "Next-turn temperature: 0") || !strings.Contains(ui.text(), "Next-turn public summaries: on") {
		t.Fatalf("confirmation=%s", ui.text())
	}
	if p.agent().Settings.Temperature == nil || p.agent().Settings.ReasoningSummary == nil {
		t.Fatal("controls did not apply")
	}
}
