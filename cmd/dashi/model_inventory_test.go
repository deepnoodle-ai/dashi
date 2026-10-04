package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineInventoryTrustBeforeCredentialLookup(t *testing.T) {
	local := configuredDestination{ID: "local", Definition: destinationDefinition{API: "openai.completions", Endpoint: "http://127.0.0.1:11434/v1/chat/completions", Models: []string{"qwen2.5-coder:7b"}, DefaultModel: "qwen2.5-coder:7b"}}
	team := configuredDestination{ID: "team", Project: true, Definition: destinationDefinition{API: "openai.responses", Endpoint: "https://proxy.test/responses", Models: []string{"acme/code-v1"}, DefaultModel: "acme/code-v1", CredentialEnv: "TEAM_KEY"}}
	registry := map[string]configuredDestination{"local": local, "team": team}
	var out bytes.Buffer
	calls := 0
	lookup := func(name string) (string, bool) { calls++; return "NEVER_EXPORT_SECRET", true }
	if err := modelInventory(registry, nil, lookup, false, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(out.String(), "needs project binding trust") || !strings.Contains(out.String(), "acme/code-v1") || !strings.Contains(out.String(), "?") || strings.Contains(out.String(), "NEVER_EXPORT_SECRET") {
		t.Fatalf("calls=%d output=%s", calls, out.String())
	}
	out.Reset()
	if err := modelInventory(registry, nil, lookup, true, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "acme/code-v1") || !strings.Contains(out.String(), "qwen2.5-coder:7b") || calls != 0 {
		t.Fatalf("availability = %s", out.String())
	}
	out.Reset()
	trusted := team
	trusted.Project = false
	if err := modelInventory(registry, map[string]configuredDestination{"team": trusted}, lookup, true, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), "acme/code-v1") || strings.Contains(out.String(), "NEVER_EXPORT_SECRET") {
		t.Fatalf("trusted inventory = %s calls=%d", out.String(), calls)
	}
}

func TestTokenCount(t *testing.T) {
	for n, want := range map[int]string{0: "?", 512: "512", 64000: "64K", 65536: "66K", 200000: "200K", 1000000: "1M", 1048576: "1M", 2000000: "2M", 1500000: "1.5M"} {
		if got := tokenCount(n); got != want {
			t.Errorf("tokenCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDetectionSkipsUntrustedProjectBinding(t *testing.T) {
	registry := builtinDestinations()
	anthropic := registry["anthropic"]
	anthropic.Project = true
	registry["anthropic"] = anthropic
	var read []string
	selection := detectedSelection(registry, nil, func(name string) (string, bool) {
		read = append(read, name)
		if name == "OPENAI_API_KEY" {
			return "fixture", true
		}
		return "", false
	})
	if selection.Provider != "openai" || strings.Contains(strings.Join(read, ","), "ANTHROPIC") {
		t.Fatalf("selection=%+v read=%v", selection, read)
	}
}

func TestModelsCommandStartsWithoutProviderClient(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".dashi"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"providers":{"local":{"api":"openai.completions","endpoint":"http://127.0.0.1:11434/v1/chat/completions","models":["qwen2.5-coder:7b"],"default_model":"qwen2.5-coder:7b","credential_env":""}}}`
	if err := os.WriteFile(filepath.Join(home, ".dashi", "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := runModels(Options{Models: true, Available: true}, home, workspace, &out, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "qwen2.5-coder:7b") {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
}

func TestModelsRejectsUnsafeTransportBeforeLookup(t *testing.T) {
	d := configuredDestination{ID: "team", Definition: destinationDefinition{API: "openai.responses", Endpoint: "http://remote.test/responses", Models: []string{"exact"}, DefaultModel: "exact", CredentialEnv: "TEAM_KEY"}}
	calls := 0
	var out bytes.Buffer
	err := modelInventory(map[string]configuredDestination{"team": d}, nil, func(string) (string, bool) { calls++; return "", false }, true, &out)
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
