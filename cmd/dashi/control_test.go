package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// Task wakes start work at any time, also while Wait waits and when no
// other work runs. That is not a race, and Wait still waits for all work.
func TestControllerWaitWhileWorkStarts(t *testing.T) {
	c := &Controller{}
	var starts sync.WaitGroup
	starts.Go(func() {
		for range 200 {
			c.goWork(func() {})
		}
	})
	for range 200 {
		c.Wait()
	}
	starts.Wait()
	c.Wait()
	if n := c.work.Load(); n != 0 {
		t.Fatalf("work=%d after Wait", n)
	}
}

// /new starts from the launch defaults, not from the preferences of the
// session that was reopened before it.
func TestNewSessionDropsReopenedPreferences(t *testing.T) {
	e, p := routedTestEngine(t, func(id string) (int, string) { return 200, id + " answer" })
	saved, err := e.cfg.Store.Create(e.cfg.Dir, "same/model")
	if err != nil {
		t.Fatal(err)
	}
	saved.NextTurn = json.RawMessage(`{"provider":"two","model":"same/model","temperature":0.25}`)
	if err = e.cfg.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
	user := preferenceFile{modelSelection: modelSelection{Provider: "one", Model: "same/model"}, Source: "user settings"}
	lookup := func(string) (string, bool) { return "", false }
	p.resolveSession = func(reopened preferenceFile) (launchPreferences, error) {
		return resolveLaunchPreferences(Options{}, p.routes.registry, p.routes.registry, user, preferenceFile{}, reopened, lookup)
	}
	if p.launch, err = p.resolveSession(preferenceFile{}); err != nil {
		t.Fatal(err)
	}
	ui := newRecorderUI()
	ctrl := NewController(t.Context(), e, ui, e.cfg.Store, e.cfg.Dir)
	ctrl.openSession(saved.ID)
	if agent := p.agent(); e.Session().ID != saved.ID || agent.Provider != "two" || agent.Settings.Temperature == nil {
		t.Fatalf("reopened agent=%+v UI=%s", agent, ui.text())
	}
	ctrl.newSession(t.Context())
	if e.Session().ID == saved.ID {
		t.Fatalf("no new session: %s", ui.text())
	}
	if agent := p.agent(); agent.Provider != "one" || agent.Settings.Temperature != nil {
		t.Fatalf("new session kept reopened preferences: %+v", agent)
	}
}

// A session switch in line mode replays the scrollback, then goes live
// again, so the person's own prompts are not printed a second time.
func TestSessionSwitchKeepsLineUILive(t *testing.T) {
	cfg := testConfig(t, say("First answer."), say("Second answer."))
	var out bytes.Buffer
	u := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, u)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	u.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	u.SetLive()
	a.ctrl.newSession(t.Context())
	a.ctrl.Input("typed after new")
	a.ctrl.Wait()
	first := a.eng.Session().ID
	a.ctrl.newSession(t.Context())
	a.ctrl.openSession(first)
	a.ctrl.Input("typed after open")
	a.ctrl.Wait()
	got := out.String()
	if n := strings.Count(got, "typed after new"); n != 1 {
		t.Fatalf("prompt after /new printed %d times, want once (the replay):\n%s", n, got)
	}
	if strings.Contains(got, "typed after open") || !strings.Contains(got, "Second answer.") {
		t.Fatalf("prompt after /sessions printed again:\n%s", got)
	}
}
