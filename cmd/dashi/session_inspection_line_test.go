package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Keeps the actual line-mode command route executable and captures its output
// when the local evidence directory is explicitly selected.
func TestSessionInspectionLineWorkflow(t *testing.T) {
	cfg := inspectionTestConfig(t)
	saved := recordedInspectionSession(t, cfg, "retry handler investigation", true)
	var out, stderr bytes.Buffer
	commands := "/sessions retry handler\n/status " + saved.ID + "\n/quit\n"
	if code := runLines(t.Context(), cfg, Options{Plain: true}, strings.NewReader(commands), &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, &stderr)
	}
	discovery := out.String()
	out.Reset()
	stderr.Reset()
	commands = "/status\n/rename recovered retry handler\n/status\n/quit\n"
	if code := runLines(t.Context(), cfg, Options{Plain: true, Resume: true, ResumeID: saved.ID}, strings.NewReader(commands), &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, &stderr)
	}
	reopened := out.String()
	if cfg.Client.(*fakeClient).sends != 0 || !strings.Contains(discovery, "worker liveness unknown") || !strings.Contains(reopened, "recovered retry handler") {
		t.Fatalf("%s\n%s", discovery, reopened)
	}
	check, err := cfg.Store.Inspect(t.Context(), cfg.Dir, saved.ID)
	if err != nil || check.IndexedTurns != 1 || check.Metadata.Title != "recovered retry handler" {
		t.Fatalf("%+v %v", check, err)
	}
	if directory := os.Getenv("DASHI_SESSION_EVIDENCE"); directory != "" {
		text := "$ dashi --plain\n" + discovery + "\n$ dashi --plain --resume " + saved.ID + "\n" + reopened + "\nFixture provider sends during both inspection invocations: 0\n"
		if err = os.WriteFile(filepath.Join(directory, "line-workflow.txt"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
