package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestOpenOffersPrefixedToolsOverStdio(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	st := statusOf(t, ts, "test")
	if st.State != StateConnected || st.Err != nil || st.Tools != 10 {
		t.Fatalf("status = %+v", st)
	}
	echo := find(t, ts, "mcp__test__echo")
	if echo.Definition.Kind != tool.KindLocal || echo.Definition.Annotations.ReadOnly != nil {
		t.Fatalf("definition = %+v", echo.Definition)
	}
	if err := echo.Definition.Validate(); err != nil {
		t.Fatal(err)
	}
	info, ok := ts.Info("mcp__test__echo")
	if !ok || info.Server != "test" || info.Remote != "echo" {
		t.Fatalf("info = %+v, %v", info, ok)
	}
	out, err := run(t, ts, "mcp__test__echo", `{"text":"hi"}`)
	if err != nil || out.State != tool.StateSucceeded || texts(out)[0] != "hi" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

// The test server marks echo read-only and idempotent. The runner repeats a
// call of unknown outcome for such tools, so the client must ignore both
// hints. An interrupted MCP call then ends the turn with its result unknown.
func TestServerHintsNeverMarkACallSafeToRepeat(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	a := find(t, ts, "mcp__test__echo").Definition.Annotations
	if a.ReadOnly != nil || a.Idempotent != nil {
		t.Fatalf("annotations = %+v", a)
	}
}

// The test server marks echo closed-world and add open-world. A false
// open-world hint would turn off checks of outside content, such as
// toolcheck.PromptInjection, so the client drops it; a missing hint counts
// as open. A true hint is kept.
func TestServerCannotMarkAToolClosedWorld(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	if ow := find(t, ts, "mcp__test__echo").Definition.Annotations.OpenWorld; ow != nil {
		t.Fatalf("echo open-world = %v; want nil", *ow)
	}
	if ow := find(t, ts, "mcp__test__add").Definition.Annotations.OpenWorld; ow == nil || !*ow {
		t.Fatalf("add open-world = %v; want true", ow)
	}
}

func TestToolNamesAreTheSameOnEveryOpen(t *testing.T) {
	a, _ := stdioServer(t)
	b, _ := stdioServer(t)
	var first []string
	for range 3 {
		ts := open(t, map[string]Server{"one": a, "two": b}, Options{})
		names := toolNames(t, ts)
		if first == nil {
			first = names
		}
		if !slices.Equal(first, names) {
			t.Fatalf("names changed:\n%v\n%v", first, names)
		}
	}
}

func TestUntrustedServerDoesNotStartWithoutApproval(t *testing.T) {
	tests := []struct {
		name    string
		approve func(context.Context, string, Server, string) (bool, error)
	}{
		{name: "nil callback"},
		{
			name: "declined",
			approve: func(context.Context, string, Server, string) (bool, error) {
				return false, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, log := stdioServer(t)
			srv.Untrusted = true
			ts := open(t, map[string]Server{"p": srv}, Options{Approve: tt.approve})
			if st := statusOf(t, ts, "p"); st.State != StateDeclined || st.Tools != 0 {
				t.Fatalf("status = %+v", st)
			}
			if got := readLog(log); got != "" {
				t.Fatalf("declined server ran: %q", got)
			}
		})
	}
}

func TestApprovedUntrustedServerStarts(t *testing.T) {
	srv, _ := stdioServer(t)
	srv.Untrusted = true
	ts := open(t, map[string]Server{"p": srv}, Options{
		Approve: func(context.Context, string, Server, string) (bool, error) {
			return true, nil
		},
	})
	if st := statusOf(t, ts, "p"); st.State != StateConnected {
		t.Fatalf("status = %+v", st)
	}
}

func TestApproveRunsInSortedOrderOneAtATime(t *testing.T) {
	servers := map[string]Server{}
	for _, name := range []string{"c", "a", "b"} {
		srv, _ := stdioServer(t)
		srv.Untrusted = true
		servers[name] = srv
	}
	trusted, _ := stdioServer(t)
	servers["user"] = trusted
	var mu sync.Mutex
	var order []string
	running := 0
	overlapped := false
	open(t, servers, Options{
		Approve: func(_ context.Context, name string, s Server, fp string) (bool, error) {
			mu.Lock()
			running++
			overlapped = overlapped || running > 1
			order = append(order, name)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			if fp != s.Hash() {
				t.Errorf("fingerprint does not match the entry for %s", name)
			}
			return true, nil
		},
	})
	if !slices.Equal(order, []string{"a", "b", "c"}) || overlapped {
		t.Fatalf("order = %v, overlapped = %v", order, overlapped)
	}
}

func TestApproveSeesRawEntry(t *testing.T) {
	srv, _ := stdioServer(t)
	srv.Untrusted = true
	srv.Args = []string{"${SECRET_TARGET}"}
	var got Server
	open(t, map[string]Server{"p": srv}, Options{
		Approve: func(_ context.Context, _ string, s Server, _ string) (bool, error) {
			got = s
			return false, nil
		},
	})
	if got.Args[0] != "${SECRET_TARGET}" {
		t.Fatalf("Approve got an expanded entry: %+v", got)
	}
}

// An entry with a command and a url is a stdio server. A command made of an
// unset variable fails it; it does not turn it into the http server.
func TestUnsetCommandDoesNotSwitchToHTTP(t *testing.T) {
	web, _ := httpServer(t, nil)
	ts := open(t, map[string]Server{
		"p": {Command: "${MCP_TEST_UNSET_BIN}", URL: web.URL},
	}, Options{})
	st := statusOf(t, ts, "p")
	if st.State != StateFailed || !strings.Contains(st.Err.Error(), "needs a command") {
		t.Fatalf("status = %+v", st)
	}
}

func TestApprovalErrorDeclinesOnlyThatServer(t *testing.T) {
	bad, _ := stdioServer(t)
	bad.Untrusted = true
	good, _ := stdioServer(t)
	ts := open(t, map[string]Server{"bad": bad, "good": good}, Options{
		Approve: func(context.Context, string, Server, string) (bool, error) {
			return false, os.ErrDeadlineExceeded
		},
	})
	if st := statusOf(t, ts, "bad"); st.State != StateDeclined || st.Err == nil {
		t.Fatalf("status = %+v", st)
	}
	if st := statusOf(t, ts, "good"); st.State != StateConnected {
		t.Fatalf("status = %+v", st)
	}
}

func TestBrokenServerIsFailedAndOthersStillWork(t *testing.T) {
	good, _ := stdioServer(t)
	ts := open(t, map[string]Server{
		"good":    good,
		"missing": {Command: "/no/such/program-xyz"},
		"empty":   {},
		"legacy":  {Type: "sse", URL: "https://x.test"},
	}, Options{})
	for _, name := range []string{"missing", "empty", "legacy"} {
		st := statusOf(t, ts, name)
		if st.State != StateFailed || st.Err == nil || st.Tools != 0 {
			t.Fatalf("%s status = %+v", name, st)
		}
	}
	if !strings.Contains(statusOf(t, ts, "missing").Err.Error(), "program-xyz") {
		t.Fatalf("error does not name the program: %v", statusOf(t, ts, "missing").Err)
	}
	if _, err := run(t, ts, "mcp__good__echo", `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
}

func TestSlowServerTimesOutAsFailed(t *testing.T) {
	ts := open(t, map[string]Server{
		"hang": {Command: "sleep", Args: []string{"30"}},
	}, Options{ConnectTimeout: 300 * time.Millisecond})
	st := statusOf(t, ts, "hang")
	if st.State != StateFailed || !strings.Contains(st.Err.Error(), "timed out") {
		t.Fatalf("status = %+v", st)
	}
}

func TestDisabledServerNeverStarts(t *testing.T) {
	srv, log := stdioServer(t)
	srv.Disabled = true
	srv.Untrusted = true
	asked := false
	ts := open(t, map[string]Server{"d": srv}, Options{
		Approve: func(context.Context, string, Server, string) (bool, error) {
			asked = true
			return true, nil
		},
	})
	if st := statusOf(t, ts, "d"); st.State != StateDisabled {
		t.Fatalf("status = %+v", st)
	}
	if asked || readLog(log) != "" {
		t.Fatal("a disabled server was prompted or started")
	}
}

func TestStatusCarriesConfigWarnings(t *testing.T) {
	srv, _ := stdioServer(t)
	srv.Warnings = []string{"from the loader"}
	srv.Args = []string{"${MCP_TEST_UNSET_VAR}"}
	ts := open(t, map[string]Server{"w": srv}, Options{})
	got := statusOf(t, ts, "w").Warnings
	if !contains(got, "from the loader") || !contains(got, "MCP_TEST_UNSET_VAR") {
		t.Fatalf("warnings = %v", got)
	}
}

func TestResultsMapToDashiContent(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})

	t.Run("image", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__image", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		blocks := out.Output.Content
		if len(blocks) != 2 || blocks[0].Kind != content.KindText || blocks[1].Kind != content.KindImage {
			t.Fatalf("blocks = %+v", blocks)
		}
		if blocks[1].Image.Source.MIMEType != "image/png" || len(blocks[1].Image.Source.Data) == 0 {
			t.Fatalf("image = %+v", blocks[1].Image)
		}
		if err := out.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("structured beside text", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__structured", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := texts(out); !slices.Equal(got, []string{"done", `{"ok":true}`}) {
			t.Fatalf("texts = %q", got)
		}
	})
	t.Run("structured already in text", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__add", `{"a":2,"b":3}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := texts(out); len(got) != 1 || !strings.Contains(got[0], `"sum":5`) {
			t.Fatalf("texts = %q", got)
		}
	})
	t.Run("error", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__fail", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != tool.StateFailed || texts(out)[0] != "it broke" {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("binary resource and link", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__blob", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		got := texts(out)
		if len(got) != 2 ||
			!strings.Contains(got[0], "file:///data.bin") ||
			!strings.Contains(got[0], "4 bytes") ||
			got[1] != "resource notes: file:///notes.txt" {
			t.Fatalf("texts = %q", got)
		}
		if !contains(statusOf(t, ts, "test").Warnings, "file:///data.bin") {
			t.Fatalf("warnings = %v", statusOf(t, ts, "test").Warnings)
		}
	})
	t.Run("bad arguments", func(t *testing.T) {
		out, err := run(t, ts, "mcp__test__add", `{"a":"x"}`)
		if err != nil || out.State != tool.StateFailed {
			t.Fatalf("out = %+v, err = %v; want a failed outcome", out, err)
		}
	})
}

func TestCancelStopsASlowCall(t *testing.T) {
	srv, log := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := runCtx(ctx, t, ts, "mcp__test__slow", `{}`)
		done <- err
	}()
	waitFor(t, "the call to start", func() bool {
		return strings.Contains(readLog(log), "call slow")
	})
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled call returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel was slow")
	}
	waitFor(t, "the server to see the cancel", func() bool {
		return strings.Contains(readLog(log), "cancelled slow")
	})
}

func TestNameCollisionKeepsTheFirstServerBySortedName(t *testing.T) {
	a, _ := stdioServer(t)
	b, _ := stdioServer(t)
	// Both names sanitize to "a_b", so every tool name collides.
	ts := open(t, map[string]Server{"a_b": b, "a b": a}, Options{})
	first := statusOf(t, ts, "a b")
	second := statusOf(t, ts, "a_b")
	if first.Tools != 10 || second.Tools != 0 {
		t.Fatalf("tools = %d and %d", first.Tools, second.Tools)
	}
	if !contains(second.Warnings, "dropped tool") {
		t.Fatalf("warnings = %v", second.Warnings)
	}
	info, _ := ts.Info("mcp__a_b__echo")
	if info.Server != "a b" {
		t.Fatalf("info = %+v", info)
	}
}

func TestRefreshPicksUpNewToolsWithoutRestarting(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	pid := func() string {
		out, err := run(t, ts, "mcp__test__pid", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		return texts(out)[0]
	}
	before := pid()
	if _, err := run(t, ts, "mcp__test__addtool", `{}`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the change notice", func() bool {
		return statusOf(t, ts, "test").Changed
	})
	if slices.Contains(toolNames(t, ts), "mcp__test__late") {
		t.Fatal("the snapshot changed before Refresh")
	}
	if err := ts.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, ts, "test")
	if st.Changed || st.Tools != 11 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := run(t, ts, "mcp__test__late", `{}`); err != nil {
		t.Fatal(err)
	}
	if pid() != before {
		t.Fatal("Refresh restarted a live server")
	}
}

func TestRefreshReconnectsAServerThatExited(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	if _, err := run(t, ts, "mcp__test__exit", `{}`); err == nil {
		t.Fatal("call to an exiting server succeeded")
	}
	waitFor(t, "the failed status", func() bool {
		return statusOf(t, ts, "test").State == StateFailed
	})
	_, err := run(t, ts, "mcp__test__echo", `{"text":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("err = %v, want a clear not-connected error", err)
	}
	if err := ts.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, ts, "test"); st.State != StateConnected || st.Err != nil {
		t.Fatalf("status = %+v", st)
	}
	if _, err := run(t, ts, "mcp__test__echo", `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshDoesNotPromptAgain(t *testing.T) {
	srv, _ := stdioServer(t)
	srv.Untrusted = true
	prompts := 0
	ts := open(t, map[string]Server{"p": srv}, Options{
		Approve: func(context.Context, string, Server, string) (bool, error) {
			prompts++
			return false, nil
		},
	})
	if err := ts.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, ts, "p"); st.State != StateDeclined || prompts != 1 {
		t.Fatalf("status = %+v, prompts = %d", st, prompts)
	}
}

func TestCloseEndsChildProcesses(t *testing.T) {
	srv, _ := stdioServer(t)
	ts, err := Open(t.Context(), Config{Servers: map[string]Server{"test": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, ts, "mcp__test__pid", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, r := range texts(out)[0] {
		pid = pid*10 + int(r-'0')
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	waitFor(t, "the child to exit", func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	})
}

func TestOpenReturnsAnErrorWhenContextEnds(t *testing.T) {
	srv, _ := stdioServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, Config{Servers: map[string]Server{"a": srv}}, Options{}); err == nil {
		t.Fatal("Open ignored a cancelled context")
	}
}

func TestHTTPServerReceivesConfiguredHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := ""
	srv, _ := httpServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = r.Header.Get("X-Token")
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	})
	t.Setenv("MCP_TEST_TOKEN", "abc")
	srv.Headers = map[string]string{"X-Token": "${MCP_TEST_TOKEN}"}
	ts := open(t, map[string]Server{"web": srv}, Options{})
	if _, err := run(t, ts, "mcp__web__echo", `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != "abc" {
		t.Fatalf("header = %q", seen)
	}
}

func TestSessionOutlivesTheConnectContext(t *testing.T) {
	srv, _ := httpServer(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	ts, err := Open(ctx, Config{Servers: map[string]Server{"web": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := run(t, ts, "mcp__web__echo", `{"text":"x"}`); err != nil {
		t.Fatalf("call after the Open context ended: %v", err)
	}
}

func TestRefreshNeverStartsAServerWhoseApprovalFailed(t *testing.T) {
	srv, log := stdioServer(t)
	srv.Untrusted = true
	ts := open(t, map[string]Server{"p": srv}, Options{
		Approve: func(context.Context, string, Server, string) (bool, error) {
			return false, os.ErrDeadlineExceeded
		},
	})
	if err := ts.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ts.Refresh(t.Context(), "p"); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, ts, "p"); st.State != StateDeclined || st.Tools != 0 {
		t.Fatalf("status = %+v", st)
	}
	if got := readLog(log); got != "" {
		t.Fatalf("Refresh started an unapproved server: %q", got)
	}
}

func TestFailedStatusIncludesServerStderr(t *testing.T) {
	ts := open(t, map[string]Server{
		"sad": {
			Command: "sh",
			Args:    []string{"-c", "echo 'missing API_KEY' >&2; exit 1"},
		},
	}, Options{})
	st := statusOf(t, ts, "sad")
	if st.State != StateFailed || !strings.Contains(st.Err.Error(), "missing API_KEY") {
		t.Fatalf("status = %+v", st)
	}
}

// A Refresh whose context has ended keeps a failed server's own error, so
// the cancel does not hide why it failed.
func TestCanceledRefreshKeepsTheFailure(t *testing.T) {
	ts := open(t, map[string]Server{
		"sad": {
			Command: "sh",
			Args:    []string{"-c", "echo 'missing API_KEY' >&2; exit 1"},
		},
	}, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ts.Refresh(ctx); err == nil {
		t.Fatal("Refresh ignored a cancelled context")
	}
	st := statusOf(t, ts, "sad")
	if st.State != StateFailed || !strings.Contains(st.Err.Error(), "missing API_KEY") {
		t.Fatalf("status = %+v", st)
	}
}

func TestStderrTailKeepsTheLastLines(t *testing.T) {
	var tl tail
	for i := range 100 {
		fmt.Fprintf(&tl, "line %d\n", i)
	}
	got := strings.Split(tl.text(), "\n")
	if len(got) != tailLines || got[len(got)-1] != "line 99" {
		t.Fatalf("tail = %q", got)
	}
	big := strings.Repeat("x", 10*tailBytes)
	tl.Write([]byte(big))
	if len(tl.buf) > tailBytes {
		t.Fatalf("buffer grew to %d bytes", len(tl.buf))
	}
}

func TestCallAfterCloseSaysTheToolsetIsClosed(t *testing.T) {
	srv, _ := stdioServer(t)
	ts, err := Open(t.Context(), Config{Servers: map[string]Server{"test": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	echo := find(t, ts, "mcp__test__echo")
	ts.Close()
	_, err = echo.Executor.Execute(t.Context(), tool.Call{ID: "c", Name: echo.Definition.Name, Args: []byte(`{"text":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "may have run") {
		t.Fatalf("err = %v", err)
	}
}

func TestStdioServerRunsInItsOwnProcessGroup(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	out, err := run(t, ts, "mcp__test__pid", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(texts(out)[0])
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if group != pid || group == syscall.Getpgrp() {
		t.Fatalf(
			"child group = %d, child = %d, host group = %d",
			group, pid, syscall.Getpgrp(),
		)
	}
}

func TestCloseEndsChildrenTheServerLeftBehind(t *testing.T) {
	srv, _ := stdioServer(t)
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	srv.Args = []string{"-c", fmt.Sprintf(
		"sleep 300 </dev/null >/dev/null 2>&1 & echo $! > %s; exec %s",
		pidFile, srv.Command,
	)}
	srv.Command = "sh"
	ts, err := Open(t.Context(), Config{Servers: map[string]Server{"test": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, ts, "test"); st.State != StateConnected {
		t.Fatalf("status = %+v", st)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Kill the sleep even when the test fails before Close.
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the left-behind child to exit", func() bool {
		// The child is not ours, so once killed it is gone, not a zombie.
		return syscall.Kill(pid, 0) == syscall.ESRCH
	})
}

func TestRefreshKeepsTheSnapshotInStepWhenContextEnds(t *testing.T) {
	srv, _ := stdioServer(t)
	ts := open(t, map[string]Server{"test": srv}, Options{})
	if _, err := run(t, ts, "mcp__test__exit", `{}`); err == nil {
		t.Fatal("call to an exiting server succeeded")
	}
	waitFor(t, "the failed status", func() bool {
		return statusOf(t, ts, "test").State == StateFailed
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ts.Refresh(ctx); err == nil {
		t.Fatal("Refresh ignored a cancelled context")
	}
	st := statusOf(t, ts, "test")
	if got := len(toolNames(t, ts)); got != st.Tools {
		t.Fatalf("snapshot has %d tools, status says %d", got, st.Tools)
	}
	if st.Tools != 0 {
		t.Fatalf("a server that did not reconnect offers %d tools", st.Tools)
	}
}

func TestRefreshAfterCloseSaysTheToolsetIsClosed(t *testing.T) {
	srv, _ := stdioServer(t)
	ts, err := Open(t.Context(), Config{Servers: map[string]Server{"test": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts.Close()
	err = ts.Refresh(t.Context())
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusAfterCloseReportsNoServerConnected(t *testing.T) {
	srv, _ := stdioServer(t)
	ts, err := Open(t.Context(), Config{Servers: map[string]Server{"test": srv}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, ts, "test"); st.State != StateConnected {
		t.Fatalf("status before Close = %+v", st)
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, ts, "test")
	if st.State != StateFailed || st.Err == nil || !strings.Contains(st.Err.Error(), "closed") {
		t.Fatalf("status after Close = %+v", st)
	}
}
