package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	dashimcp "github.com/deepnoodle-ai/dashi/mcp"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type mcpStartupRequest struct{ Name, Hash, Workspace, Review string }
type mcpStartupFunc func(context.Context, mcpStartupRequest) (bool, error)

// mcpRuntime is one run's frozen configuration and forwarding tool snapshot.
// It builds without effects. Initialize publishes tools before turn admission.
type mcpRuntime struct {
	dir                    string
	selection              mcpSelection
	mu                     sync.Mutex
	started, ready, closed bool
	done                   chan struct{}
	cancel                 context.CancelFunc
	initErr                error
	startupStates          map[string]string
	startupFailed          map[string]bool
	set                    *dashimcp.Toolset
	tools                  []tool.Tool
}

func newMCPRuntime(dir string, selection mcpSelection) *mcpRuntime {
	return &mcpRuntime{dir: dir, selection: selection, ready: len(selection.entries) == 0, done: make(chan struct{}), startupStates: map[string]string{}, startupFailed: map[string]bool{}}
}
func (m *mcpRuntime) Initialize(ctx context.Context, approve mcpStartupFunc) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return context.Canceled
	}
	if m.started {
		done := m.done
		m.mu.Unlock()
		select {
		case <-done:
			m.mu.Lock()
			err := m.initErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	m.started, m.cancel = true, cancel
	m.mu.Unlock()
	defer cancel()
	set, err := dashimcp.Open(ctx, m.selection.config, dashimcp.Options{Dir: m.dir, ConnectTimeout: m.selection.timeout,
		HTTPClient: func(string, dashimcp.Server) *http.Client {
			return &http.Client{Transport: &mcpRedirectTransport{next: http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		},
		Approve: func(ctx context.Context, name string, s dashimcp.Server, hash string) (bool, error) {
			allowed := false
			var err error
			if approve != nil {
				allowed, err = approve(ctx, mcpStartupRequest{Name: name, Hash: hash, Workspace: m.dir, Review: m.startupReview(name, s)})
			}
			m.mu.Lock()
			if !allowed || err != nil {
				m.startupStates[name] = "blocked"
			}
			m.mu.Unlock()
			return allowed, err
		},
	})
	var entries []tool.Tool
	if err == nil {
		entries, err = set.Tools(ctx)
		if err == nil {
			err = ctx.Err()
		}
	}
	m.mu.Lock()
	if m.closed && err == nil {
		err = context.Canceled
	}
	if err == nil {
		m.set = set
		for _, status := range set.Status() {
			if status.State == dashimcp.StateFailed {
				m.startupFailed[status.Name] = true
			}
		}
		for _, entry := range entries {
			entry.Definition = entry.Definition.Clone()
			entry.Executor = mcpSafeExecutor{next: entry.Executor, name: entry.Definition.Name}
			m.tools = append(m.tools, entry)
		}
		m.ready = true
	}
	m.initErr = err
	m.mu.Unlock()
	if err != nil && set != nil {
		set.Close()
	}
	m.mu.Lock()
	close(m.done)
	m.mu.Unlock()
	return err
}
func (m *mcpRuntime) Ready() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready && !m.closed
}
func (m *mcpRuntime) Tools(context.Context) ([]tool.Tool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.tools)
	for i := range out {
		out[i].Definition = out[i].Definition.Clone()
	}
	return out, nil
}
func (m *mcpRuntime) Close() error {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	started, done := m.started, m.done
	m.mu.Unlock()
	if started {
		<-done
	}
	m.mu.Lock()
	set := m.set
	m.mu.Unlock()
	if set != nil {
		return set.Close()
	}
	return nil
}
func (m *mcpRuntime) startupReview(name string, s dashimcp.Server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MCP server %s\nSource: %s (project)\n", workspace.PrintableText(name), workspace.PrintableText(s.Source))
	if mcpServerTransport(s) == "stdio" {
		fmt.Fprintf(&b, "Transport: stdio\nCommand: %s\n", mcpSafeCommand(s, s.Command))
		for _, arg := range s.Args {
			fmt.Fprintf(&b, "Argument: %s\n", mcpSafeCommand(s, arg))
		}
		fmt.Fprintf(&b, "Directory: %s\nThe child inherits the process environment.\n", workspace.PrintableText(m.dir))
	} else {
		fmt.Fprintf(&b, "Transport: streamable HTTP\nDestination: %s\n", mcpEndpoint(s.URL))
	}
	for _, bindings := range []struct {
		label  string
		values map[string]string
	}{{"Environment", s.Env}, {"Header", s.Headers}} {
		for _, key := range slices.Sorted(maps.Keys(bindings.values)) {
			fmt.Fprintf(&b, "%s %s: %s\n", bindings.label, workspace.PrintableText(key), mcpBindingReview(bindings.values[key]))
		}
	}
	fmt.Fprintln(&b, "Consent starts this server for this Dashi run only. Tool calls still require approval.")
	return b.String()
}
func mcpEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid endpoint"
	}
	return workspace.PrintableText(u.Scheme + "://" + u.Host)
}
func (m *mcpRuntime) Inventory() string {
	if m == nil {
		return "MCP: no configured servers."
	}
	m.mu.Lock()
	set, started, ready := m.set, m.started, m.ready
	tools := slices.Clone(m.tools)
	startupStates, startupFailed := maps.Clone(m.startupStates), maps.Clone(m.startupFailed)
	m.mu.Unlock()
	statuses := map[string]dashimcp.ServerStatus{}
	if set != nil {
		for _, s := range set.Status() {
			statuses[s.Name] = s
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "MCP servers (timeout %s; fixed catalog for this Dashi run):\n", m.selection.timeout)
	if len(m.selection.entries) == 0 {
		fmt.Fprintln(&b, "  None configured.")
	}
	for _, name := range slices.Sorted(maps.Keys(m.selection.entries)) {
		e := m.selection.entries[name]
		s := e.server
		state, reason := e.state, e.reason
		if state == "" && startupStates[name] == "blocked" {
			state, reason = "blocked", "project startup was not approved"
		}
		status := statuses[name]
		if state == "" {
			if status.State != "" {
				state = string(status.State)
				if state == string(dashimcp.StateDeclined) {
					state = "blocked"
					reason = "project startup was not approved"
				}
				if status.Err != nil {
					reason = mcpDiagnostic(status.Err)
					if startupFailed[name] {
						reason = mcpStartupDiagnostic(status.Err)
					}
				}
			} else if started && !ready {
				state = "starting"
			} else {
				state = "starting"
			}
		}
		transport := mcpServerTransport(s)
		scope := "trusted"
		if s.Untrusted {
			scope = "project"
		}
		fmt.Fprintf(&b, "  %s: %s; %s; %s; %d tools\n    Source: %s\n", workspace.PrintableText(name), state, scope, workspace.PrintableText(transport), status.Tools, workspace.PrintableText(s.Source))
		if transport == "stdio" {
			fmt.Fprintf(&b, "    Directory: %s\n", workspace.PrintableText(m.dir))
		} else if s.URL != "" && e.state == "" {
			fmt.Fprintf(&b, "    Destination: %s\n", mcpEndpoint(s.URL))
		}
		if reason != "" {
			fmt.Fprintf(&b, "    %s\n", reason)
		}
		for _, source := range e.ignored {
			fmt.Fprintf(&b, "    Ignored conflicting source: %s\n", workspace.PrintableText(source))
		}
		for _, warning := range status.Warnings {
			if safe := mcpWarning(set, warning); safe != "" {
				fmt.Fprintf(&b, "    %s\n", safe)
			}
		}
		if status.Changed {
			fmt.Fprintln(&b, "    Catalog changed; restart between turns to load it.")
		}
		if set != nil {
			for _, tl := range tools {
				info, ok := set.Info(tl.Definition.Name)
				if ok && info.Server == name {
					fmt.Fprintf(&b, "    %s → %s\n", workspace.PrintableText(tl.Definition.Name), workspace.PrintableText(info.Remote))
				}
			}
		}
	}
	for _, warning := range m.selection.warnings {
		fmt.Fprintf(&b, "  Warning: %s\n", warning)
	}
	return strings.TrimSpace(b.String())
}

var mcpMissingVariable = regexp.MustCompile(`^variable [A-Za-z_][A-Za-z0-9_]* is not set$`)

func mcpDiagnostic(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "connection or call canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "connection or call timed out"
	}
	// Match known protocol/status terms, never print the underlying exception.
	text := strings.ToLower(err.Error())
	// The module marks a lost response explicitly. Preserve that uncertainty
	// before classifying incidental status words from the wrapped exception.
	if strings.Contains(text, "the call may have run") {
		return "connection closed; the call may have run"
	}
	if strings.Contains(text, "401") || strings.Contains(text, "403") || strings.Contains(text, "unauthorized") || strings.Contains(text, "forbidden") {
		return "authentication required or refused; OAuth is unsupported; use caller-supplied headers"
	}
	if strings.Contains(text, "timed out") || strings.Contains(text, "timeout") {
		return "connection or call timed out"
	}
	if strings.Contains(text, "closed") || strings.Contains(text, "connection lost") {
		return "connection closed; the call may have run"
	}
	return "connection failed; check server configuration"
}

type mcpSafeError struct {
	message string
	cause   error
}

func (e mcpSafeError) Error() string { return e.message }
func (e mcpSafeError) Unwrap() error { return e.cause }

type mcpSafeExecutor struct {
	next tool.Executor
	name string
}

func (e mcpSafeExecutor) Execute(ctx context.Context, call tool.Call) (tool.Outcome, error) {
	out, err := e.next.Execute(ctx, call)
	if err != nil {
		return out, mcpSafeError{message: "MCP " + workspace.PrintableText(e.name) + ": " + mcpDiagnostic(err), cause: err}
	}
	return out, nil
}

// The SDK can try legacy initialization after failed discovery. A redirect
// poisons this connection so that fallback makes no additional wire request.
// CheckRedirect also refuses redirects at the http.Client boundary.
type mcpRedirectTransport struct {
	next    http.RoundTripper
	mu      sync.Mutex
	refused bool
}

func (t *mcpRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	refused := t.refused
	t.mu.Unlock()
	if refused {
		return nil, errors.New("MCP redirect refused; restart with the configured endpoint")
	}
	response, err := t.next.RoundTrip(r)
	if err == nil && response.StatusCode >= 300 && response.StatusCode < 400 {
		t.mu.Lock()
		t.refused = true
		t.mu.Unlock()
	}
	return response, err
}

func mcpStartupDiagnostic(err error) string {
	diagnostic := mcpDiagnostic(err)
	if diagnostic == "connection closed; the call may have run" {
		return "server exited during startup; check server configuration"
	}
	return diagnostic
}

func mcpServerTransport(s dashimcp.Server) string {
	switch strings.ToLower(s.Type) {
	case "http", "streamable-http":
		return "http"
	case "stdio":
		return "stdio"
	case "":
		if s.Command != "" {
			return "stdio"
		}
		return "http"
	default:
		return s.Type
	}
}

var mcpDroppedTool = regexp.MustCompile(`^dropped tool ("(?:[^"\\]|\\.)*"):`)
var mcpCollision = regexp.MustCompile(`^dropped tool ("(?:[^"\\]|\\.)*"): name ([A-Za-z0-9_-]+) is taken by `)

func mcpWarning(set *dashimcp.Toolset, raw string) string {
	if mcpMissingVariable.MatchString(raw) {
		return raw
	}
	if strings.HasPrefix(raw, "ignored server ") {
		return ""
	} // Source selection is already shown.
	if match := mcpCollision.FindStringSubmatch(raw); len(match) > 0 {
		remote, _ := strconv.Unquote(match[1])
		if set != nil {
			if winner, ok := set.Info(match[2]); ok {
				return fmt.Sprintf("Dropped remote tool %s: %s already belongs to %s.%s", workspace.PrintableText(remote), match[2], workspace.PrintableText(winner.Server), workspace.PrintableText(winner.Remote))
			}
		}
	}
	if match := mcpDroppedTool.FindStringSubmatch(raw); len(match) > 0 {
		remote, _ := strconv.Unquote(match[1])
		return "Dropped remote tool " + workspace.PrintableText(remote) + ": invalid or colliding definition"
	}
	return "Catalog warning: a tool result was omitted or degraded."
}
