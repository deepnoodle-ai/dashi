package main

import (
	"reflect"
	"testing"
	"time"
)

// dashi run -c skips a newer session whose first turn never recorded, as
// a failed first attachment request leaves it, and reopens the latest
// session with turns.
func TestAutomationContinueSkipsSessionWithoutTurns(t *testing.T) {
	cfg := testConfig(t, say("first answer"), say("second answer"))
	if code, _, diag := runAutomationFixture(t, cfg, Options{Prompt: "first", Run: true}, ""); code != 0 {
		t.Fatal(diag)
	}
	saved, err := cfg.Store.Latest(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	orphan, err := cfg.Store.Create(cfg.Dir, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Store.Save(orphan); err != nil {
		t.Fatal(err)
	}
	code, out, diag := runAutomationFixture(t, cfg, Options{Prompt: "next", Run: true, Resume: true, OutputFormat: "json"}, "")
	if code != 0 || !reflect.DeepEqual(decodeResponses(t, "json", out), []responseMessage{{"second answer"}}) {
		t.Fatalf("code=%d out=%s diag=%s", code, out, diag)
	}
	if latest, err := cfg.Store.Latest(cfg.Dir); err != nil || latest.ID != saved.ID {
		t.Fatalf("continued %v, want %s", latest, saved.ID)
	}
}
