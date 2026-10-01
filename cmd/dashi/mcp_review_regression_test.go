package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPPipedStartupSignalChild(t *testing.T) {
	if os.Getenv("DASHI_MCP_SIGNAL_CHILD") != "1" {
		return
	}
	cfg := testConfig(t, say("PROMPT_MUST_NOT_RUN"))
	cfg.HomeDir = os.Getenv("DASHI_MCP_SIGNAL_HOME")
	code := runLines(context.Background(), cfg, Options{Plain: true}, os.Stdin, os.Stdout, os.Stderr)
	if code != 1 || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatalf("code=%d sends=%d", code, cfg.Client.(*fakeClient).sends)
	}
}
func TestMCPPipedStartupSignalCancelsDiscoveryAndSuppressesPrompt(t *testing.T) {
	home := t.TempDir()
	entry, log := mcpTestEntry(t)
	entry["env"].(map[string]string)["DASHI_MCP_TEST_START_DELAY"] = "30000"
	writeMCPConfig(t, filepath.Join(home, ".dashi", "settings.json"), map[string]any{"docs": entry})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMCPPipedStartupSignalChild$", "-test.v")
	cmd.Env = append(os.Environ(), "DASHI_MCP_SIGNAL_CHILD=1", "DASHI_MCP_SIGNAL_HOME="+home)
	cmd.Stdin = strings.NewReader("work\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	mcpWaitLog(t, log, "pid ")
	// Existing stdio shutdown allows two seconds before SIGTERM; the race
	// helper adds a final one-second exit delay. Discovery itself must cancel.
	started := time.Now()
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("signal did not cancel startup promptly")
	}
	if err != nil || time.Since(started) > 4*time.Second || strings.Contains(out.String(), "PROMPT_MUST_NOT_RUN") {
		t.Fatalf("exit=%v elapsed=%v output=%s", err, time.Since(started), out.String())
	}
	mcpAssertReaped(t, log)
}

func mcpRunEnvironment(t *testing.T, home, dir string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Chdir(dir)
	for _, name := range []string{"DASHI_PROVIDER", "DASHI_MODEL", "DASHI_THINKING_EFFORT", "DASHI_SHOW_THINKING", "DASHI_TEMPERATURE", "DASHI_MAX_TOKENS"} {
		t.Setenv(name, "")
	}
	// Controls treat a present empty value as invalid, so retain no override.
	for _, name := range []string{"DASHI_THINKING_EFFORT", "DASHI_SHOW_THINKING", "DASHI_TEMPERATURE", "DASHI_MAX_TOKENS"} {
		os.Unsetenv(name)
	}
	old := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = old })
}
func TestMCPActualStartupIsolatesFailedUserSettings(t *testing.T) {
	for _, failure := range []string{"malformed", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			mcpRunEnvironment(t, home, dir)
			path := filepath.Join(home, ".dashi", "settings.json")
			os.MkdirAll(filepath.Dir(path), 0700)
			if failure == "malformed" {
				os.WriteFile(path, []byte(`{"provider":"UNSAFE_PARTIAL",`), 0600)
			} else {
				os.Mkdir(path, 0700)
			}
			docs, log := mcpTestEntry(t)
			project, blockedLog := mcpTestEntry(t)
			extra := filepath.Join(dir, "tools.json")
			writeMCPConfig(t, extra, map[string]any{"docs": docs})
			writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"project": project})
			t.Setenv("OPENAI_API_KEY", "NO_AUTOMATIC_ROUTE")
			var out, stderr bytes.Buffer
			code := run(t.Context(), []string{"--plain", "--mcp-config", extra}, strings.NewReader("/help\n/mcp\nwork\n/quit\n"), &out, &stderr)
			if code != 0 || !strings.Contains(stderr.String(), "source ignored") || !strings.Contains(out.String(), "docs: connected") || !strings.Contains(out.String(), "project: blocked") || !strings.Contains(out.String(), "also works while a turn runs") || !strings.Contains(out.String(), "select an explicit destination") || strings.Contains(out.String(), "── turn 1 ·") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
			}
			mcpAssertReaped(t, log)
			if _, err := os.Stat(blockedLog); !os.IsNotExist(err) {
				t.Fatal("project child started without consent")
			}
			out.Reset()
			stderr.Reset()
			code = run(t.Context(), []string{"--plain", "--provider", "UNSAFE_PARTIAL", "--mcp-config", extra}, strings.NewReader("work\n"), &out, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "not configured") {
				t.Fatalf("failed source reconstructed: code=%d err=%s", code, stderr.String())
			}
		})
	}
}

func TestMCPFailedProjectPreferencesRequireExplicitRouteButAllowValidDispatch(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	mcpRunEnvironment(t, home, dir)
	var sends atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"type":"response.completed","response":{"object":"response","status":"completed","model":"fixture-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Valid route dispatched."}]}]}}` + "\n\n"))
	}))
	defer endpoint.Close()
	definition := destinationDefinition{API: "openai.responses", Endpoint: endpoint.URL, Models: []string{"fixture-model"}, DefaultModel: "fixture-model"}
	raw, _ := json.Marshal(map[string]any{"provider": "local", "providers": map[string]destinationDefinition{"local": definition}})
	userPath := filepath.Join(home, ".dashi", "settings.json")
	os.MkdirAll(filepath.Dir(userPath), 0700)
	os.WriteFile(userPath, raw, 0600)
	projectPath := filepath.Join(dir, ".dashi", "settings.json")
	os.MkdirAll(projectPath, 0700)
	docs, log := mcpTestEntry(t)
	extra := filepath.Join(dir, "tools.json")
	writeMCPConfig(t, extra, map[string]any{"docs": docs})
	var out, stderr bytes.Buffer
	code := run(t.Context(), []string{"-p", "work", "--mcp-config", extra}, strings.NewReader(""), &out, &stderr)
	if code != 1 || sends.Load() != 0 || !strings.Contains(stderr.String(), "select an explicit destination") {
		t.Fatalf("default silently resumed: code=%d sends=%d err=%s", code, sends.Load(), stderr.String())
	}
	mcpAssertReaped(t, log)
	for _, selector := range [][]string{{"--provider", "local"}, {"--model", "fixture-model"}} {
		out.Reset()
		stderr.Reset()
		args := append([]string{"-p", "work", "--mcp-config", extra}, selector...)
		if code = run(t.Context(), args, strings.NewReader(""), &out, &stderr); code != 0 || !strings.Contains(out.String(), "Valid route dispatched.") {
			t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
		}
	}
	out.Reset()
	stderr.Reset()
	if code = run(t.Context(), []string{"--resume", "-p", "work", "--mcp-config", extra}, strings.NewReader(""), &out, &stderr); code != 0 || !strings.Contains(out.String(), "Valid route dispatched.") {
		t.Fatalf("recorded valid route unavailable: code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if sends.Load() != 3 {
		t.Fatalf("sends=%d", sends.Load())
	}
	out.Reset()
	stderr.Reset()
	code = run(t.Context(), []string{"-p", "/help", "--provider", "unknown"}, strings.NewReader(""), &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "also works while a turn runs") || sends.Load() != 3 {
		t.Fatal("local help attempted live route setup")
	}
}

func TestMCPIsolationRetainsStrictDestinationAndControlValidation(t *testing.T) {
	for _, body := range []string{`{"providers":{"bad":{"api":"openai.responses","endpoint":"https://example.test","models":["x"],"default_model":"x","api_key":"SECRET"}}}`, `{"max_tokens":-1}`} {
		t.Run(body, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			mcpRunEnvironment(t, home, dir)
			path := filepath.Join(home, ".dashi", "settings.json")
			os.MkdirAll(filepath.Dir(path), 0700)
			os.WriteFile(path, []byte(body), 0600)
			docs, log := mcpTestEntry(t)
			extra := filepath.Join(dir, "tools.json")
			writeMCPConfig(t, extra, map[string]any{"docs": docs})
			var out, stderr bytes.Buffer
			code := run(t.Context(), []string{"--provider", "openai", "-p", "work", "--mcp-config", extra}, strings.NewReader(""), &out, &stderr)
			if code != 1 || strings.Contains(stderr.String(), "SECRET") || strings.Contains(stderr.String(), "source ignored") {
				t.Fatalf("invalid provider/control ignored: code=%d err=%s", code, stderr.String())
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("strict validation happened after MCP dispatch")
			}
		})
	}
}
