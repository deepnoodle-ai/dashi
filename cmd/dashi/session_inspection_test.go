package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/wonton/tui"
)

func inspectionTestConfig(t *testing.T, script ...reply) appConfig {
	cfg := testConfig(t, script...)
	dir, err := canonicalWorkspace(cfg.Dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dir = dir
	return cfg
}

func recordedInspectionSession(t *testing.T, cfg appConfig, prompt string, interrupted bool) *sessionstore.Session {
	t.Helper()
	copyCfg := cfg
	started := make(chan struct{})
	if interrupted {
		copyCfg.Client = &fakeClient{script: []reply{blockUntilDone(started)}}
	} else {
		copyCfg.Client = &fakeClient{script: []reply{say("Recorded answer about " + prompt)}}
	}
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), copyCfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if interrupted {
		go func() { <-started; cancel() }()
	}
	_, err = a.eng.Prompt(ctx, prompt)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved, _ := a.eng.inspectionMetadata()
	a.eng.Close()
	return saved
}

func inspectionFiles(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = sha256.Sum256(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInspectionCommandsPreserveOwnedSessionAndFacts(t *testing.T) {
	cfg := inspectionTestConfig(t)
	aSaved := recordedInspectionSession(t, cfg, "current investigation", false)
	bSaved := recordedInspectionSession(t, cfg, "retry handler", true)
	completed, err := cfg.Store.Inspect(t.Context(), cfg.Dir, aSaved.ID)
	if err != nil || completed.Accounting.ModelCalls != 1 {
		t.Fatalf("completed accounting: %+v %v", completed.Accounting, err)
	}
	interrupted, err := cfg.Store.Inspect(t.Context(), cfg.Dir, bSaved.ID)
	if err != nil || interrupted.Accounting.ModelCalls != 0 || interrupted.Accounting.Spend.Estimated != 1 || interrupted.Accounting.Spend.CostUnknown != 1 {
		t.Fatalf("interrupted no-response accounting: %+v %v", interrupted.Accounting, err)
	}

	external, err := cfg.Store.Lock(bSaved)
	if err != nil {
		t.Fatal(err)
	}
	defer external()
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: aSaved.ID}); err != nil {
		t.Fatal(err)
	}
	before := inspectionFiles(t, cfg.Store.Root())
	a.ctrl.Input("/status " + bSaved.ID)
	a.ctrl.Wait()
	text := ui.text()
	for _, want := range []string{"Session " + bSaved.ID, "retry handler", "Recorded stop: interrupted", "worker liveness unknown", "dashi --resume " + bSaved.ID, "passing recovery checks"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, bad := range []string{"Press Enter", "/continue", "/done", "/discard"} {
		if strings.Contains(text, bad) {
			t.Fatalf("unowned guidance includes %q: %s", bad, text)
		}
	}
	if a.eng.Session().ID != aSaved.ID || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("inspection switched or sent")
	}
	a.ctrl.openSession(bSaved.ID)
	if a.eng.Session().ID != aSaved.ID {
		t.Fatal("busy reopen replaced current")
	}
	if !reflect.DeepEqual(before, inspectionFiles(t, cfg.Store.Root())) {
		t.Fatal("inspection/rejected reopen mutated saved facts")
	}
}

func TestStatusDuringWorkDoesNotSteerOrAuthorize(t *testing.T) {
	cfg := inspectionTestConfig(t)
	started := make(chan struct{})
	cfg.Client = &fakeClient{script: []reply{blockUntilDone(started)}}
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.Input("a running request")
	<-started
	a.ctrl.Input("/status")
	// Wait only for the read-only status, not for the live model call.
	for range 100 {
		time.Sleep(5 * time.Millisecond)
		if strings.Contains(ui.text(), "this process is running work") {
			break
		}
	}
	if !strings.Contains(ui.text(), "this process is running work") {
		t.Fatal(ui.text())
	}
	if a.ctrl.Mode() != ModeRunning {
		t.Fatal("status changed live mode")
	}
	a.ctrl.Interrupt()
	a.ctrl.Wait()
	view := a.eng.Last(t.Context()).View
	if len(view.Pending) > 0 && !strings.Contains(ui.text(), "Recorded snapshot") {
		t.Fatal("missing recorded snapshot")
	}
	for turn, err := range a.eng.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range turn.Steps {
			if step.Context != nil && step.Context.Steering {
				t.Fatal("status became steer")
			}
		}
	}
}

func TestInspectionOwnerGuidanceUsesExactPendingCallAndNoFalseRecovery(t *testing.T) {
	base := sessionstore.Inspection{Metadata: sessionstore.Session{ID: "target"}, Latest: &sessionstore.Execution{Readable: true, Next: dashi.NextFix, Pending: []sessionstore.PendingObservation{{ID: "effect-42", UnsafeEffect: true}}}}
	own := inspectionOwner{ID: "target", Owned: true, Mode: ModeIdle}
	got := inspectionGuidance(base, own)
	if !strings.Contains(got, "verify the exact external effect for call effect-42") || !strings.Contains(got, "/done effect-42") || strings.Contains(got, "Press Enter") {
		t.Fatal(got)
	}
	base.Latest.Readable = false
	got = inspectionGuidance(base, own)
	if strings.Contains(got, "/done") || strings.Contains(got, "/discard") || !strings.Contains(got, "No continuation") {
		t.Fatal(got)
	}
	base.Latest.Readable = true
	base.Latest.Pending = nil
	base.Latest.Next = dashi.NextRaiseAttempts
	if !strings.Contains(inspectionGuidance(base, own), "/continue") {
		t.Fatal("missing limit route")
	}
	own.Mode = ModeApproval
	if got = inspectionGuidance(base, own); strings.Contains(got, "/continue") || !strings.Contains(got, "No permission") {
		t.Fatal(got)
	}
}

func TestRenameCurrentLeaseIdleAndRelaunch(t *testing.T) {
	cfg := inspectionTestConfig(t, say("later answer"))
	saved := recordedInspectionSession(t, cfg, "original", false)
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: saved.ID}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.setMode(ModeRunning)
	a.ctrl.Input("/rename not allowed")
	a.ctrl.setMode(ModeIdle)
	if a.eng.Session().Title != "original" {
		t.Fatal("renamed while running")
	}
	a.ctrl.Input("/rename rate-limit 調査")
	a.ctrl.Wait()
	if a.eng.Session().Title != "rate-limit 調査" {
		t.Fatal(ui.text())
	}
	if _, err = a.eng.Prompt(t.Context(), "subsequent request"); err != nil {
		t.Fatal(err)
	}
	loaded, err := cfg.Store.LoadScoped(t.Context(), cfg.Dir, saved.ID)
	if err != nil || loaded.Title != "rate-limit 調査" {
		t.Fatalf("%+v %v", loaded, err)
	}
	a.eng.Close()
	if err = a.eng.RenameCurrent("unowned"); err == nil {
		t.Fatal("renamed without lease")
	}
	fresh := NewEngine(EngineConfig{Store: cfg.Store, Dir: cfg.Dir, Client: cfg.Client})
	unsaved, _ := cfg.Store.Create(cfg.Dir, "fixture")
	if err = fresh.Open(unsaved); err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err = fresh.RenameCurrent("empty"); err == nil || !strings.Contains(err.Error(), "first request") {
		t.Fatal(err)
	}
}

type inspectingChooser struct {
	*recorderUI
	selectID         string
	cancel           bool
	options, details []string
	beforeReply      func(context.Context)
}

func (u *inspectingChooser) ChooseSessions(ctx context.Context, options, details []string) (int, bool) {
	u.options = options
	u.details = details
	if u.beforeReply != nil {
		u.beforeReply(ctx)
	}
	if u.cancel {
		return 0, false
	}
	for i, s := range options {
		if strings.Contains(s, u.selectID) {
			return i, true
		}
	}
	return 0, false
}

func TestSearchCardsAndCanceledSelectionDoNotLoseDraft(t *testing.T) {
	cfg := inspectionTestConfig(t)
	saved := recordedInspectionSession(t, cfg, "find retry handler", false)
	ui := &inspectingChooser{recorderUI: newRecorderUI(), selectID: saved.ID, cancel: true}
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	a.openSession(cfg.Store, cfg.Dir, Options{})
	current := a.eng.Session().ID
	a.ctrl.Input("/sessions retry handler")
	a.ctrl.Wait()
	if len(ui.details) != 1 || !strings.Contains(ui.details[0], "Last request ("+session.TurnID(saved.ID, 1)) || !strings.Contains(ui.details[0], "Status: recorded completed") || !strings.Contains(ui.details[0], "Text match (") {
		t.Fatal(ui.details)
	}
	if a.eng.Session().ID != current || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("cancel switched or dispatched")
	}
	ui.cancel = false
	ctx, cancel := context.WithCancel(t.Context())
	ui.beforeReply = func(context.Context) { cancel() }
	a.ctrl.sessions(ctx, "retry handler")
	if a.eng.Session().ID != current {
		t.Fatal("late result switched after cancellation")
	}
	ui.beforeReply = nil
	a.ctrl.Input("/sessions retry handler")
	a.ctrl.Wait()
	if a.eng.Session().ID != saved.ID {
		t.Fatal(ui.text())
	}
}

func TestScopedReopenRejectsForeignWorkspaceBeforePreferences(t *testing.T) {
	cfg := inspectionTestConfig(t)
	foreign := cfg
	foreign.Dir, _ = canonicalWorkspace(t.TempDir(), "")
	saved := recordedInspectionSession(t, foreign, "foreign private title", false)
	if _, err := loadReopenedPreferences(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: saved.ID}); err == nil {
		t.Fatal("foreign preferences loaded")
	}
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	a.openSession(cfg.Store, cfg.Dir, Options{})
	old := a.eng.Session().ID
	a.ctrl.openSession(saved.ID)
	if a.eng.Session().ID != old || strings.Contains(ui.text(), "foreign private title") {
		t.Fatal(ui.text())
	}
}

func TestSessionLineCardsAndPrintCommandBoundary(t *testing.T) {
	cfg := inspectionTestConfig(t)
	saved := recordedInspectionSession(t, cfg, "rate-limit investigation", false)
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	a.openSession(cfg.Store, cfg.Dir, Options{})
	a.ctrl.Input("/sessions rate-limit")
	a.ctrl.Wait()
	a.ctrl.Input("/status " + saved.ID)
	a.ctrl.Wait()
	if !strings.Contains(out.String(), saved.ID) || !strings.Contains(out.String(), "Model money:") || !strings.Contains(out.String(), "dashi --resume") {
		t.Fatal(out.String())
	}
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Print: "/status " + saved.ID, PrintSet: true}, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires --plain") || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatalf("code %d: %s %s", code, &stdout, &stderr)
	}
}

func TestNativeSessionPickerKeepsSelectionAndDraft(t *testing.T) {
	cfg := inspectionTestConfig(t)
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	s.bind(a)
	a.openSession(cfg.Store, cfg.Dir, Options{})
	s.input = "unsubmitted draft"
	s.attachments.text = s.input
	s.picker = &pickerState{sessionCards: true, options: []string{"ID-A title", "ID-B title"}, details: []string{"details A", "details B"}, reply: make(chan int, 1)}
	s.pickerKey(tui.KeyEvent{Key: tui.KeyArrowDown})
	if s.picker.index != 1 {
		t.Fatal("selection did not move")
	}
	s.pickerKey(tui.KeyEvent{Key: tui.KeyPageDown})
	if s.picker.index != 1 {
		t.Fatal("card scroll changed selection")
	}
	s.pickerKey(tui.KeyEvent{Key: tui.KeyEscape})
	if s.input != "unsubmitted draft" || s.attachments.text != s.input {
		t.Fatal("cancel changed draft")
	}
}

func TestInspectionEndpointDoesNotConstructClientOrLeakCredentials(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("provider dispatch"); return 0, "" })
	d := p.routes.registry["one"]
	d.Definition.Endpoint = "https://name:password@one.test/responses?api_key=SECRET#fragment"
	p.routes.registry["one"] = d
	fingerprint, _ := d.fingerprint()
	p.routes.factory = func(configuredDestination, func(string) (string, bool), bool) (llm.Client, error) {
		t.Fatal("inspection constructed client")
		return nil, nil
	}
	ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
	report := sessionstore.Inspection{Metadata: *e.Session(), Latest: &sessionstore.Execution{Readable: true, Destination: sessionstore.Destination{Provider: "one", API: "openai.responses", Revision: cliRouteRevision + fingerprint}}, NextDestination: sessionstore.Destination{Provider: "one"}}
	got := ctrl.inspectionDestinations(report)
	for _, secret := range []string{"password", "api_key", "SECRET", "fragment", "name:"} {
		if strings.Contains(got, secret) {
			t.Fatal(got)
		}
	}
	if !strings.Contains(got, "unavailable (recorded binding") {
		t.Fatal(got)
	}
	report.Latest.Destination.Revision = "unknown"
	if got = ctrl.inspectionDestinations(report); !strings.Contains(got, "unavailable (recorded binding") {
		t.Fatal(got)
	}
}
