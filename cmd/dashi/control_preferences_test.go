package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestControlsPersistAndInspectionNeverDispatchesOrExportsSecrets(t *testing.T) {
	calls := 0
	e, p := routedTestEngine(t, func(string) (int, string) { calls++; return 200, "answer" })
	p.guidance, p.guidanceSet = "PRIVATE_PROMPT_TEXT", true
	p.routes.lookup = func(string) (string, bool) { return "PRIVATE_CREDENTIAL_VALUE", true }
	ui := &recorderUI{}
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	for _, pair := range [][2]string{{"/temperature", "0"}, {"/thinking", "on"}, {"/max-tokens", "1234"}, {"/effort", "omit"}} {
		ctrl.generationControl(t.Context(), pair[0], pair[1])
	}
	if calls != 0 {
		t.Fatal("control preview dispatched")
	}
	view := e.inspectSettings(t.Context())
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if view.Requested.Temperature == nil || *view.Requested.Temperature != 0 || view.Effective == nil || view.Effective.ReasoningSummary == nil || !*view.Effective.ReasoningSummary || view.Requested.MaxTokens != 1234 || view.PromptSource != "--system-prompt" {
		t.Fatalf("inspection=%s", raw)
	}
	for _, secret := range []string{"PRIVATE_PROMPT_TEXT", "PRIVATE_CREDENTIAL_VALUE", "Authorization", "Preparation preview."} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("inspection exported %s", secret)
		}
	}
	if calls != 0 {
		t.Fatal("inspection dispatched")
	}
	data, err := os.ReadFile(p.writer.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if json.Unmarshal(data, &saved) != nil || string(saved["temperature"]) != "0" || string(saved["show_thinking"]) != "true" || string(saved["max_tokens"]) != "1234" || string(saved["thinking_effort"]) != `""` {
		t.Fatalf("saved=%s", data)
	}
	ctrl.generationControl(t.Context(), "/thinking", "off")
	if p.agent().Settings.ReasoningSummary == nil || *p.agent().Settings.ReasoningSummary {
		t.Fatal("off did not override on")
	}
	ctrl.generationControl(t.Context(), "/temperature", "omit")
	if p.agent().Settings.Temperature != nil {
		t.Fatal("temperature omission lost")
	}
	restored, err := reopenedPreferences(p.metadata(), "")
	if err != nil || !restored.ShowThinking.Set || restored.ShowThinking.Value || !restored.Temperature.Null || !restored.Effort.Set {
		t.Fatalf("reopen=%+v %v", restored, err)
	}
}

func TestChangingControlDoesNotInventUnsetThinking(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("preview dispatched"); return 0, "" })
	ctrl := NewController(t.Context(), e, &recorderUI{}, e.cfg.Store, e.cfg.Dir)
	ctrl.generationControl(t.Context(), "/temperature", "0")
	if p.agent().Settings.ReasoningSummary != nil {
		t.Fatal("unset thinking gained explicit off")
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(p.metadata(), &saved); err != nil {
		t.Fatal(err)
	}
	if _, exists := saved["show_thinking"]; exists {
		t.Fatal("metadata invented a thinking default")
	}
}

func TestFailedPreferenceSaveKeepsActiveSelection(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("selection preview dispatched"); return 0, "" })
	p.routes.user = p.routes.registry
	if err := os.WriteFile(p.writer.path, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := &recorderUI{}
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	ctrl.destinationModel(t.Context(), "two same/model", p)
	if p.agent().Provider != "two" || !strings.Contains(ui.text(), "user preferences were not saved") {
		t.Fatalf("selection=%+v UI=%s", p.agent(), ui.text())
	}
	data, _ := os.ReadFile(p.writer.path)
	if string(data) != `{broken` {
		t.Fatal("corrupt preferences overwritten")
	}
}

func TestCancelSettingsDialogEndsCommand(t *testing.T) {
	e, _ := routedTestEngine(t, func(string) (int, string) { t.Fatal("dialog dispatched"); return 0, "" })
	ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
	started, finished := make(chan struct{}), make(chan struct{})
	ctrl.async(func(ctx context.Context) { close(started); <-ctx.Done(); close(finished) })
	<-started
	if ctrl.Interrupt() {
		t.Fatal("dialog interrupt requested process exit")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("dialog context was not cancelled")
	}
	ctrl.Wait()
	if ctrl.Mode() != ModeIdle {
		t.Fatal("dialog left controller busy")
	}
}

// modelPickerUI picks rows by label, one answer per picker it is shown.
type modelPickerUI struct {
	*recorderUI
	answers []string
	shown   [][]string
	footers []string
}

func (u *modelPickerUI) ChooseModel(_ context.Context, _ string, options, _ []string, footer string, _ int) (int, bool) {
	u.shown, u.footers = append(u.shown, options), append(u.footers, footer)
	want := u.answers[0]
	u.answers = u.answers[1:]
	for i, option := range options {
		if strings.HasSuffix(option, want) {
			return i, true
		}
	}
	return 0, false
}

func TestModelPickerOpensTheCompleteList(t *testing.T) {
	p := builtinPickerPreferences(t)
	e, _ := routedTestEngine(t, func(string) (int, string) { t.Fatal("picker dispatched"); return 0, "" })
	ui := &modelPickerUI{recorderUI: newRecorderUI(), answers: []string{"All models…", "claude-haiku-4-5-20251001"}}
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	choice, ok := ctrl.pickModel(t.Context(), p, false)
	if !ok || choice.provider != "anthropic" || choice.model != "claude-haiku-4-5-20251001" {
		t.Fatalf("choice=%+v ok=%t", choice, ok)
	}
	if len(ui.shown) != 2 || len(ui.shown[1]) <= len(ui.shown[0]) || !strings.Contains(ui.footers[0], "Set GEMINI_API_KEY") || ui.footers[1] != "" {
		t.Fatalf("shown=%v footers=%q", ui.shown, ui.footers)
	}
}

func TestModelPickerWithoutTerminalPrintsNotes(t *testing.T) {
	p := builtinPickerPreferences(t)
	e, _ := routedTestEngine(t, func(string) (int, string) { t.Fatal("picker dispatched"); return 0, "" })
	ui := newRecorderUI()
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	if _, ok := ctrl.pickModel(t.Context(), p, false); ok {
		t.Fatal("a UI without a picker chose a model")
	}
	text := ui.text()
	if !strings.Contains(text, "claude-opus-4-8 — 1M · current") || !strings.Contains(text, "Set GEMINI_API_KEY (google), ") || strings.Contains(text, "claude-opus-4-5-20251101") {
		t.Fatalf("text=%s", text)
	}
}
