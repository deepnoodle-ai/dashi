package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type sessionPreferencePicker struct {
	*recorderUI
	id string
}

func (u *sessionPreferencePicker) Choose(_ context.Context, _ string, labels []string) (int, bool) {
	for i, label := range labels {
		if strings.Contains(label, u.id) {
			return i, true
		}
	}
	return 0, false
}

func TestSessionPickerRestoresPreferencesAndDispatchesSelectedRoute(t *testing.T) {
	for _, mode := range []string{"saved", "flags", "environment", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			var dispatched []string
			e, p := routedTestEngine(t, func(id string) (int, string) { dispatched = append(dispatched, id); return 200, id + " answer" })
			saved, err := e.cfg.Store.Create(e.cfg.Dir, "same/model")
			if err != nil {
				t.Fatal(err)
			}
			saved.NextTurn = json.RawMessage(`{"provider":"two","model":"same/model","temperature":0.25,"show_thinking":false}`)
			if err = e.cfg.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "fixture"}
			savedPreferences, err := reopenedPreferences(saved.NextTurn, "")
			if err != nil {
				t.Fatal(err)
			}
			p.launch.Controls, err = resolveControls([]generationTier{{generationPreferences: savedPreferences.generationPreferences, Source: "fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Open(saved); err != nil {
				t.Fatal(err)
			}
			if _, err = e.Prompt(t.Context(), "saved session seed"); err != nil {
				t.Fatal(err)
			}
			dispatched = nil
			other, err := e.cfg.Store.Create(e.cfg.Dir, "same/model")
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Open(other); err != nil {
				t.Fatal(err)
			}
			o := Options{}
			if mode == "flags" {
				o.Provider = "one"
				o.Controls.Temperature = preferenceField[float64]{Set: true, Value: 0}
			}
			if mode == "environment" {
				// The command line read DASHI_PROVIDER=one and DASHI_TEMPERATURE=0.5.
				o.Provider = "one"
				o.Controls.Temperature = preferenceField[float64]{Set: true, Value: 0.5}
			}
			user := preferenceFile{modelSelection: modelSelection{Provider: "one", Model: "same/model"}, Source: "user settings"}
			lookup := func(string) (string, bool) { return "", false }
			p.resolveSession = func(reopened preferenceFile) (launchPreferences, error) {
				return resolveLaunchPreferences(o, p.routes.registry, p.routes.registry, user, preferenceFile{}, reopened, lookup)
			}
			p.launch, err = p.resolveSession(preferenceFile{})
			if err != nil {
				t.Fatal(err)
			}
			ui := &sessionPreferencePicker{recorderUI: newRecorderUI(), id: saved.ID}
			ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
			if mode == "interactive" {
				ctrl.destinationModel(t.Context(), "one same/model", p)
				ctrl.generationControl(t.Context(), "/temperature", "0.75")
			}
			ctrl.sessions(t.Context())
			if e.Session().ID != saved.ID {
				t.Fatalf("session not selected: %s", ui.text())
			}
			wantRoute, wantTemperature := "two", 0.25
			switch mode {
			case "flags":
				wantRoute, wantTemperature = "one", 0
			case "environment":
				wantRoute, wantTemperature = "one", 0.5
			case "interactive":
				wantRoute, wantTemperature = "one", 0.75
			}
			agent := p.agent()
			if agent.Provider != wantRoute || agent.Settings.Temperature == nil || *agent.Settings.Temperature != wantTemperature || agent.Settings.ReasoningSummary == nil || *agent.Settings.ReasoningSummary {
				t.Fatalf("restored agent=%+v settings=%+v", agent, agent.Settings)
			}
			result, err := e.Prompt(t.Context(), "after session picker")
			if err != nil || result.Outcome.Reply != wantRoute+" answer" || len(dispatched) != 1 || dispatched[0] != wantRoute {
				t.Fatalf("dispatch=%v result=%+v error=%v", dispatched, result, err)
			}
			// The actual recorded turn retains the resolved controls, independently of convenience metadata.
			turnIndex := 0
			for turn, err := range e.handle().Turns(t.Context()) {
				turnIndex++
				if turnIndex == 1 {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				view, err := turn.View()
				if err != nil {
					t.Fatal(err)
				}
				if view.Agent.Settings.Temperature == nil || *view.Agent.Settings.Temperature != wantTemperature {
					t.Fatalf("recorded settings=%+v", view.Agent.Settings)
				}
			}
		})
	}
}

func TestInvalidSessionPreferencesKeepOpenSessionAndDefaults(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("session switch dispatched"); return 0, "" })
	original := e.Session().ID
	saved, err := e.cfg.Store.Create(e.cfg.Dir, "same/model")
	if err != nil {
		t.Fatal(err)
	}
	saved.NextTurn = json.RawMessage(`{"provider":"missing","model":"same/model"}`)
	if err = e.cfg.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
	ui := newRecorderUI()
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	ctrl.openSession(saved.ID)
	if e.Session().ID != original || p.agent().Provider != "one" || !strings.Contains(ui.text(), "cannot be selected") {
		t.Fatalf("session=%s agent=%+v UI=%s", e.Session().ID, p.agent(), ui.text())
	}
}

func TestInteractiveModelRetainsAllOverriddenSelectionSources(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("model preview dispatched"); return 0, "" })
	p.launch.Selection.Source = "flags"
	p.launch.Selection.Overridden = []selectionTier{{modelSelection: modelSelection{Provider: "two", Model: "same/model"}, Source: "project settings"}, {modelSelection: modelSelection{Provider: "one", Model: "same/model"}, Source: "user settings"}}
	ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
	ctrl.destinationModel(t.Context(), "two same/model", p)
	ctrl.destinationModel(t.Context(), "one same/model", p)
	raw, err := json.Marshal(e.inspectSettings(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"session change", "flags", "project settings", "user settings"} {
		if !strings.Contains(string(raw), source) {
			t.Fatalf("missing source %s: %s", source, raw)
		}
	}
}

// Each session switch lists the tiers of the selected session under a
// session change, not those of every session opened before it.
func TestSessionChangeOverriddenSourcesDoNotGrow(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("session switch dispatched"); return 0, "" })
	var ids []string
	for range 2 {
		s, err := e.cfg.Store.Create(e.cfg.Dir, "same/model")
		if err != nil {
			t.Fatal(err)
		}
		s.NextTurn = json.RawMessage(`{"provider":"two","model":"same/model","temperature":0.25}`)
		if err = e.cfg.Store.Save(s); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
	ctrl.destinationModel(t.Context(), "one same/model", p)
	ctrl.generationControl(t.Context(), "/temperature", "0.75")
	ctrl.openSession(ids[0])
	selection := len(p.launch.Selection.Overridden)
	temperature := len(p.launch.Controls.Sources["temperature"].Overridden)
	for _, id := range []string{ids[1], ids[0], ids[1]} {
		ctrl.openSession(id)
	}
	if e.Session().ID != ids[1] || p.launch.Selection.Source != "session change" {
		t.Fatalf("session=%s selection=%+v", e.Session().ID, p.launch.Selection)
	}
	if got := len(p.launch.Selection.Overridden); got != selection {
		t.Fatalf("selection overridden grew from %d to %d", selection, got)
	}
	if got := len(p.launch.Controls.Sources["temperature"].Overridden); got != temperature {
		t.Fatalf("temperature overridden grew from %d to %d", temperature, got)
	}
}
