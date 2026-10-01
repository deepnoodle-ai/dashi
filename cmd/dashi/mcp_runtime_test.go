package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPServerProcess hosts real documentation tools in a child process.
func TestMCPServerProcess(t *testing.T) {
	if os.Getenv("DASHI_MCP_TEST_PROCESS") != "1" {
		return
	}
	log := func(text string) {
		f, e := os.OpenFile(os.Getenv("DASHI_MCP_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e == nil {
			fmt.Fprintln(f, text)
			f.Close()
		}
	}
	log("pid " + strconv.Itoa(os.Getpid()))
	if delay, err := strconv.Atoi(os.Getenv("DASHI_MCP_TEST_START_DELAY")); err == nil && delay > 0 {
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	if os.Getenv("DASHI_MCP_TEST_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "SECRET_CONFIG_TOKEN provider setup failed")
		os.Exit(1)
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "documentation-fixture", Version: "1"}, nil)
	for _, name := range []string{"search", "fetch", "cwd", "slow", "exit"} {
		s.AddTool(&sdk.Tool{Name: name, Description: "Documentation fixture " + name, InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}}, func(ctx context.Context, r *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			log("call " + r.Params.Name)
			text := ""
			switch r.Params.Name {
			case "search":
				text = "Found: guide.md — durable Session recovery"
			case "fetch":
				data, e := os.ReadFile(os.Args[len(os.Args)-1])
				if e != nil {
					return nil, e
				}
				text = string(data)
			case "cwd":
				text, _ = os.Getwd()
			case "slow":
				<-ctx.Done()
				log("slow canceled")
				return nil, ctx.Err()
			case "exit":
				fmt.Fprintln(os.Stderr, "SECRET_CONFIG_TOKEN disconnected")
				os.Exit(1)
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil
		})
	}
	_ = s.Run(context.Background(), &sdk.StdioTransport{})
	os.Exit(0)
}
func mcpTestEntry(t *testing.T) (map[string]any, string) {
	t.Helper()
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	log := filepath.Join(t.TempDir(), "process.log")
	return map[string]any{"command": binary, "args": []string{"-test.run=^TestMCPServerProcess$", "--", "guide.md"}, "env": map[string]string{"DASHI_MCP_TEST_PROCESS": "1", "DASHI_MCP_TEST_LOG": log}}, log
}
func writeMCPConfig(t *testing.T, path string, servers map[string]any) {
	t.Helper()
	data, e := json.Marshal(map[string]any{"mcpServers": servers})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func mcpFind(t *testing.T, m *mcpRuntime, name string) tool.Tool {
	t.Helper()
	tools, e := m.Tools(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	for _, tl := range tools {
		if tl.Definition.Name == name {
			return tl
		}
	}
	t.Fatalf("missing tool %s", name)
	return tool.Tool{}
}
func mcpExecute(ctx context.Context, tl tool.Tool) (tool.Outcome, error) {
	return tl.Executor.Execute(ctx, tool.Call{ID: "fixture", Name: tl.Definition.Name, Args: []byte(`{}`)})
}
func mcpAssertReaped(t *testing.T, log string) {
	t.Helper()
	data, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		t.Fatal(string(data))
	}
	pid, e := strconv.Atoi(fields[1])
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Kill(pid, 0); !errors.Is(e, syscall.ESRCH) {
		t.Fatalf("child %d remains: %v", pid, e)
	}
}
func mcpWaitLog(t *testing.T, log, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(log)
		if strings.Contains(string(data), text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %q in %s", text, data)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMCPRealDocumentationWorkflowWithFailureIsolationAndCallApproval(t *testing.T) {
	home, dir, invocation := t.TempDir(), t.TempDir(), t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	docs, docsLog := mcpTestEntry(t)
	broken, brokenLog := mcpTestEntry(t)
	blocked, blockedLog := mcpTestEntry(t)
	broken["env"].(map[string]string)["DASHI_MCP_TEST_FAIL"] = "1"
	writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": docs, "broken": broken, "blocked": blocked})
	if e := os.WriteFile(filepath.Join(dir, "guide.md"), []byte("Resume a recorded Session with its original Turn facts."), 0600); e != nil {
		t.Fatal(e)
	}
	client := &fakeClient{script: []reply{callTool("search-1", "mcp__docs__search", `{"query":"recovery"}`), callTool("fetch-1", "mcp__docs__fetch", `{"path":"guide.md"}`), say("The recorded guide explains recovery.")}}
	ui := newRecorderUI()
	store := sessionstore.NewStore(t.TempDir())
	a, e := buildApp(t.Context(), appConfig{Dir: dir, HomeDir: home, InvocationDir: invocation, Model: "fixture", Client: client, Store: store}, ui)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.eng.Close(); a.wiring.Close() })
	if _, e = a.openSession(store, dir, Options{}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.eng.Prompt(t.Context(), "too early"); e == nil || store.Saved(a.eng.Session()) {
		t.Fatal("admitted work before startup")
	}
	var order []string
	e = a.eng.cfg.MCP.Initialize(t.Context(), func(ctx context.Context, r mcpStartupRequest) (bool, error) {
		order = append(order, r.Name)
		for _, log := range []string{docsLog, brokenLog, blockedLog} {
			if _, e := os.Stat(log); !os.IsNotExist(e) {
				t.Fatalf("server started before all consent: %s", log)
			}
		}
		if !strings.Contains(r.Review, dir) || strings.Contains(r.Review, "SECRET_CONFIG_TOKEN") {
			t.Fatal(r.Review)
		}
		return r.Name != "blocked", nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Join(order, ",") != "blocked,broken,docs" {
		t.Fatal(order)
	}
	res, e := a.eng.Prompt(t.Context(), "Find the recovery guide and explain it.")
	if e != nil || res.Outcome.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("res=%+v e=%v", res, e)
	}
	if len(ui.approvals) != 2 || ui.approvals[0].Tool != "mcp__docs__search" || ui.approvals[1].Tool != "mcp__docs__fetch" {
		t.Fatalf("approvals=%+v", ui.approvals)
	}
	data, _ := os.ReadFile(docsLog)
	if strings.Count(string(data), "call search") != 1 || strings.Count(string(data), "call fetch") != 1 {
		t.Fatal(string(data))
	}
	input, _ := json.Marshal(client.inputAt(2))
	if !strings.Contains(string(input), "original Turn facts") {
		t.Fatalf("real document absent: %s", input)
	}
	inventory := a.eng.cfg.MCP.Inventory()
	for _, part := range []string{"docs: connected", "broken: failed", "blocked: blocked", "mcp__docs__fetch → fetch", dir} {
		if !strings.Contains(inventory, part) {
			t.Fatalf("missing %s in %s", part, inventory)
		}
	}
	if strings.Contains(inventory, "SECRET_CONFIG_TOKEN") || strings.Contains(ui.text(), "SECRET_CONFIG_TOKEN") {
		t.Fatal("secret reached diagnostics")
	}
	if _, e = os.Stat(blockedLog); !os.IsNotExist(e) {
		t.Fatal("declined child started")
	}
	if e = a.wiring.Close(); e != nil {
		t.Fatal(e)
	}
	if e = a.wiring.Close(); e != nil {
		t.Fatal(e)
	}
	mcpAssertReaped(t, docsLog)
	mcpAssertReaped(t, brokenLog)
	t.Log("Sorted project consent, partial failure isolation, separately approved search/fetch, actual guide text in request, process shutdown passed.")
}
func TestMCPUnattendedStartupNeverInheritsYesToolConsent(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": entry})
	a, e := buildApp(t.Context(), appConfig{Dir: dir, HomeDir: home}, &printUI{yes: true, err: os.Stderr})
	if e != nil {
		t.Fatal(e)
	}
	defer a.wiring.Close()
	if e = a.initializeMCP(t.Context(), false); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(log); !os.IsNotExist(e) {
		t.Fatal("--yes started project child")
	}
	if !strings.Contains(a.eng.cfg.MCP.Inventory(), "blocked") {
		t.Fatal("missing blocked state")
	}
}
func TestMCPDisconnectedDiagnosticsAndCancellationCauses(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(home, ".dashi", "settings.json"), map[string]any{"docs": entry})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	defer m.Close()
	if e := m.Initialize(t.Context(), nil); e != nil {
		t.Fatal(e)
	}
	_, e := mcpExecute(t.Context(), mcpFind(t, m, "mcp__docs__exit"))
	if e == nil || !strings.Contains(e.Error(), "may have run") || strings.Contains(e.Error(), "SECRET_CONFIG_TOKEN") || strings.Contains(e.Error(), "EOF") || strings.Contains(e.Error(), "check server configuration") {
		t.Fatalf("e=%v", e)
	}
	_, e = mcpExecute(t.Context(), mcpFind(t, m, "mcp__docs__fetch"))
	if e == nil || strings.Contains(e.Error(), "SECRET_CONFIG_TOKEN") {
		t.Fatalf("disconnected e=%v", e)
	}
	original := tool.Fatal(context.Canceled)
	safe := mcpSafeExecutor{next: executorFunc(func(context.Context, tool.Call) (tool.Outcome, error) { return tool.Outcome{}, original }), name: "mcp__docs__fetch"}
	_, e = safe.Execute(t.Context(), tool.Call{})
	if !errors.Is(e, context.Canceled) || !errors.Is(e, tool.ErrFatal) {
		t.Fatalf("cause lost: %v", e)
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	mcpAssertReaped(t, log)
}

type executorFunc func(context.Context, tool.Call) (tool.Outcome, error)

func (f executorFunc) Execute(ctx context.Context, c tool.Call) (tool.Outcome, error) {
	return f(ctx, c)
}
func TestMCPCancelRunningCallReachesServerAndClosesChild(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(home, ".dashi", "settings.json"), map[string]any{"docs": entry})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	defer m.Close()
	if e := m.Initialize(t.Context(), nil); e != nil {
		t.Fatal(e)
	}
	slow := mcpFind(t, m, "mcp__docs__slow")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, e := mcpExecute(ctx, slow); done <- e }()
	mcpWaitLog(t, log, "call slow")
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("e=%v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call cancellation stuck")
	}
	mcpWaitLog(t, log, "slow canceled")
	if e := m.Close(); e != nil {
		t.Fatal(e)
	}
	mcpAssertReaped(t, log)
}
func TestMCPHTTPConsentPrecedesRequestsAndRedirectsNeverFollow(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests, targets atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targets.Add(1); w.WriteHeader(500) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			defer server.Close()
			home, dir := t.TempDir(), t.TempDir()
			writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": map[string]any{"url": server.URL + "/mcp?secret=SECRET", "headers": map[string]string{"Authorization": "${TOKEN}"}}})
			m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
			defer m.Close()
			e := m.Initialize(t.Context(), func(ctx context.Context, r mcpStartupRequest) (bool, error) {
				if requests.Load() != 0 || !strings.Contains(r.Review, "${TOKEN}") || strings.Contains(r.Review, "secret=") {
					t.Fatal(r.Review)
				}
				return true, nil
			})
			if e != nil {
				t.Fatal(e)
			}
			if requests.Load() != 1 || targets.Load() != 0 {
				t.Fatalf("requests=%d targets=%d", requests.Load(), targets.Load())
			}
			if strings.Contains(m.Inventory(), "SECRET") {
				t.Fatal("query credential leaked")
			}
		})
	}
}

func TestMCPCloseCancelsInitializationBeforeDispatch(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": entry})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	requested := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.Initialize(t.Context(), func(ctx context.Context, r mcpStartupRequest) (bool, error) {
			close(requested)
			<-ctx.Done()
			return false, ctx.Err()
		})
	}()
	<-requested
	if m.Ready() {
		t.Fatal("ready during startup")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("server started after cancellation")
	}
	if err := m.Initialize(t.Context(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed runtime reopened: %v", err)
	}
}
func TestMCPRejectedURLsDoNotApproveExpandHeadersOrRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, raw := range []string{"https://user:SECRET@example.com/mcp", "https://${MCP_AUTHORITY}/mcp", "${SCHEME}://example.com/mcp", "http://localhost/mcp", "http://example.com/mcp"} {
		for _, headers := range []bool{false, true} {
			if !headers && (raw == "http://localhost/mcp" || raw == "http://example.com/mcp") {
				continue
			}
			home, dir := t.TempDir(), t.TempDir()
			entry := map[string]any{"url": raw}
			if headers {
				entry["headers"] = map[string]string{"Authorization": "${UNSET_CREDENTIAL}"}
			}
			writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": entry})
			var lookups []string
			selection := selectMCP(home, dir, dir, nil, os.ReadFile, func(name string) (string, bool) { lookups = append(lookups, name); return "", false })
			if len(selection.config.Servers) != 0 {
				t.Fatalf("unsafe entry survived: %s", raw)
			}
			m := newMCPRuntime(dir, selection)
			if err := m.Initialize(t.Context(), func(context.Context, mcpStartupRequest) (bool, error) {
				t.Fatal("unsafe endpoint prompted")
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(lookups) != 1 || lookups[0] != "MCP_TIMEOUT" || requests.Load() != 0 {
				t.Fatalf("lookups=%v requests=%d", lookups, requests.Load())
			}
			m.Close()
		}
	}
}

func TestMCPHTTPHeadersPathExpansionAndFixedCatalog(t *testing.T) {
	t.Setenv("DASHI_MCP_HTTP_TOKEN", "SECRET_HTTP_TOKEN")
	t.Setenv("DASHI_MCP_HTTP_PATH", "mcp")
	server := sdk.NewServer(&sdk.Implementation{Name: "http-docs", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "HTTP documentation bytes"}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer SECRET_HTTP_TOKEN" {
			t.Errorf("incorrect configured path/header")
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	home, dir := t.TempDir(), t.TempDir()
	writeMCPConfig(t, filepath.Join(dir, ".mcp.json"), map[string]any{"docs": map[string]any{"url": endpoint.URL + "/${DASHI_MCP_HTTP_PATH}", "headers": map[string]string{"Authorization": "Bearer ${DASHI_MCP_HTTP_TOKEN}"}}})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	defer m.Close()
	if err := m.Initialize(t.Context(), func(ctx context.Context, r mcpStartupRequest) (bool, error) {
		if requests.Load() != 0 || !strings.Contains(r.Review, "${DASHI_MCP_HTTP_TOKEN}") || strings.Contains(r.Review, "SECRET_HTTP_TOKEN") {
			t.Fatal(r.Review)
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Inventory(), "docs: connected") {
		t.Fatal(m.Inventory())
	}
	out, err := mcpExecute(t.Context(), mcpFind(t, m, "mcp__docs__read"))
	if err != nil || out.Output.Content[0].Text.Text != "HTTP documentation bytes" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	before, _ := m.Tools(t.Context())
	server.AddTool(&sdk.Tool{Name: "later", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(m.Inventory(), "Catalog changed") {
		if time.Now().After(deadline) {
			t.Fatal("missing catalog-change notice")
		}
		time.Sleep(5 * time.Millisecond)
	}
	after, _ := m.Tools(t.Context())
	if len(before) != 1 || len(after) != 1 {
		t.Fatal("catalog mutated during run")
	}
	if strings.Contains(m.Inventory(), "SECRET_HTTP_TOKEN") {
		t.Fatal("HTTP credential leaked")
	}
}
func TestMCPHTTPAuthenticationGapHasSafeReason(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "SECRET_AUTH_RESPONSE", 401) }))
	defer endpoint.Close()
	home, dir := t.TempDir(), t.TempDir()
	writeMCPConfig(t, filepath.Join(home, ".dashi", "settings.json"), map[string]any{"docs": map[string]any{"url": endpoint.URL}})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	defer m.Close()
	if err := m.Initialize(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	inventory := m.Inventory()
	if !strings.Contains(inventory, "authentication required or refused") || strings.Contains(inventory, "SECRET_AUTH_RESPONSE") {
		t.Fatal(inventory)
	}
}

func TestMCPCollisionInventoryNamesDroppedToolAndWinner(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	a, _ := mcpTestEntry(t)
	b, _ := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(home, ".dashi", "settings.json"), map[string]any{"a.b": a, "a_b": b})
	m := newMCPRuntime(dir, selectMCP(home, dir, dir, nil, os.ReadFile, os.LookupEnv))
	defer m.Close()
	if err := m.Initialize(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	inventory := m.Inventory()
	if !strings.Contains(inventory, "Dropped remote tool fetch: mcp__a_b__fetch already belongs to a.b.fetch") {
		t.Fatal(inventory)
	}
	tools, _ := m.Tools(t.Context())
	if len(tools) != 5 {
		t.Fatalf("duplicate tools exposed: %d", len(tools))
	}
}
