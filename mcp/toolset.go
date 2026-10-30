package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultConnectTimeout bounds one server's start and tool discovery.
const defaultConnectTimeout = 10 * time.Second

// errClosed is the error after Close.
var errClosed = errors.New("the MCP toolset is closed")

// State is the connection state of a server.
type State string

// Server states.
const (
	// StateConnected means the server answered and its tools are listed.
	StateConnected State = "connected"
	// StateFailed means the server did not start, or it stopped later.
	StateFailed State = "failed"
	// StateDeclined means an untrusted server was not approved. It never
	// started.
	StateDeclined State = "declined"
	// StateDisabled means the entry has Disabled set.
	StateDisabled State = "disabled"
)

// Options configures Open.
type Options struct {
	// Dir is the working directory of every stdio child. Empty inherits
	// the host process working directory. It does not affect HTTP servers.
	Dir string

	// Approve runs once for each Untrusted server, in sorted name order, one
	// at a time, before any server starts. It gets the raw entry and its
	// hash. A false result or an error declines the server. Nil declines
	// every untrusted server. The package stores no answer: the host may
	// remember one, keyed on the hash.
	Approve func(
		ctx context.Context,
		name string,
		s Server,
		hash string,
	) (bool, error)

	// ConnectTimeout bounds each server's start and discovery. Zero means
	// 10 seconds.
	ConnectTimeout time.Duration

	// HTTPClient lets the host add authentication to one HTTP server. Nil, or
	// a nil result, uses http.DefaultClient. The package adds the entry's
	// Headers on top.
	HTTPClient func(name string, s Server) *http.Client
}

// ServerStatus reports one configured server.
type ServerStatus struct {
	Name   string
	Source string
	State  State

	// Tools counts the tools offered to the model.
	Tools int

	// Err is set when State is StateFailed. It is also set for a declined
	// server whose approval callback returned an error.
	Err error

	// Changed means the server said its tool list changed. Call Refresh
	// between turns.
	Changed bool

	// Warnings lists config problems, dropped or degraded tools, and notes
	// about results that the model could not use.
	Warnings []string
}

// ToolInfo says where a model-facing tool came from. A BeforeTool hook uses it
// to apply a per-server rule.
type ToolInfo struct {
	// Server is the configured server name.
	Server string

	// Remote is the tool name on that server.
	Remote string
}

// Toolset offers the tools of connected MCP servers to an agent. It is a
// fixed snapshot: the runner requires a tool's definition to match at
// dispatch, so the set changes only when the host calls Refresh, between
// turns. A Toolset is safe for concurrent use.
type Toolset struct {
	opts Options

	mu      sync.Mutex
	conns   map[string]*conn
	tools   []tool.Tool
	info    map[string]ToolInfo
	closed  bool
	closing sync.Once
}

// conn is one configured server and its live session.
type conn struct {
	name    string
	server  Server
	state   State
	err     error
	session *mcp.ClientSession
	// listed holds the remote tools of the current session.
	listed  []*mcp.Tool
	changed bool
	// changes counts list-changed notifications, so a listing can tell
	// whether one arrived while it ran.
	changes int
	stderr  *tail
	// notes are runtime warnings; dropped are the last rebuild's collisions.
	notes   []string
	dropped []string
	exposed int
	initial []string
}

// Open starts the configured servers and returns their tools as a Toolset.
//
// A server that is disabled, declined, or broken is reported in Status and
// never returned as an error, so the agent runs with the servers that work.
// Open returns an error only when ctx ends. Approved servers start in
// parallel. Close the Toolset to stop the servers.
func Open(ctx context.Context, cfg Config, opts Options) (*Toolset, error) {
	t := &Toolset{
		opts:  opts,
		conns: map[string]*conn{},
		info:  map[string]ToolInfo{},
	}
	names := slices.Sorted(maps.Keys(cfg.Servers))
	var start []*conn
	for _, name := range names {
		s := cfg.Servers[name]
		c := &conn{name: name, server: s, initial: s.Warnings}
		t.conns[name] = c
		if s.Disabled {
			c.state = StateDisabled
			continue
		}
		if _, err := s.transport(); err != nil {
			c.state, c.err = StateFailed, err
			continue
		}
		if s.Untrusted {
			ok, err := t.approve(ctx, c)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				// A failed prompt is not consent. Declined servers never
				// start, and Refresh skips them.
				c.state, c.err = StateDeclined, fmt.Errorf("approval: %w", err)
				continue
			}
			if !ok {
				c.state = StateDeclined
				continue
			}
		}
		start = append(start, c)
	}
	t.each(start, func(c *conn) { t.dial(ctx, c) })
	if err := ctx.Err(); err != nil {
		t.Close()
		return nil, err
	}
	t.mu.Lock()
	t.rebuild()
	t.mu.Unlock()
	return t, nil
}

func (t *Toolset) approve(ctx context.Context, c *conn) (bool, error) {
	if t.opts.Approve == nil {
		return false, nil
	}
	return t.opts.Approve(ctx, c.name, c.server, c.server.Hash())
}

// Tools returns the snapshot taken by Open or the last Refresh. It never
// calls a server.
func (t *Toolset) Tools(context.Context) ([]tool.Tool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.tools), nil
}

// Info returns the server and remote name of a model-facing tool name.
func (t *Toolset) Info(toolName string) (ToolInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.info[toolName]
	return info, ok
}

// Status reports every configured server, sorted by name.
func (t *Toolset) Status() []ServerStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []ServerStatus
	for _, name := range slices.Sorted(maps.Keys(t.conns)) {
		c := t.conns[name]
		var warnings []string
		warnings = append(warnings, c.initial...)
		warnings = append(warnings, c.notes...)
		warnings = append(warnings, c.dropped...)
		out = append(out, ServerStatus{
			Name:     c.name,
			Source:   c.server.Source,
			State:    c.state,
			Tools:    c.exposed,
			Err:      c.err,
			Changed:  c.changed,
			Warnings: warnings,
		})
	}
	return out
}

// Refresh updates the snapshot. For each named server, or every server when
// none is named, it lists the tools again if the server is connected, and
// reconnects it if it failed. It never restarts a live server and never asks
// for approval again: a declined server stays declined.
//
// Call it between turns, not during one. It returns an error only when ctx
// ends or the Toolset is closed; a server that fails is reported in Status.
// The snapshot matches Status even when ctx ends partway.
func (t *Toolset) Refresh(ctx context.Context, names ...string) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errClosed
	}
	var pick []*conn
	for _, name := range slices.Sorted(maps.Keys(t.conns)) {
		if len(names) == 0 || slices.Contains(names, name) {
			pick = append(pick, t.conns[name])
		}
	}
	t.mu.Unlock()
	t.each(pick, func(c *conn) {
		t.mu.Lock()
		state, session := c.state, c.session
		t.mu.Unlock()
		switch state {
		case StateConnected:
			t.relist(ctx, c, session)
		case StateFailed:
			t.dial(ctx, c)
		}
	})
	t.mu.Lock()
	t.rebuild()
	t.mu.Unlock()
	return ctx.Err()
}

// Close stops every server and its child process. Afterward Tools is empty
// and Status reports each server that was connected as failed. It is safe to call twice.
func (t *Toolset) Close() error {
	var err error
	t.closing.Do(func() {
		t.mu.Lock()
		t.closed = true
		var sessions []*mcp.ClientSession
		for _, c := range t.conns {
			if c.session != nil {
				sessions = append(sessions, c.session)
			}
			// fail ignores a closed Toolset, so set the state here: Status
			// must not report a stopped server as connected.
			if c.state == StateConnected {
				c.state, c.err = StateFailed, errClosed
			}
		}
		t.rebuild()
		t.mu.Unlock()
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, cs := range sessions {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A server that exits nonzero on shutdown is not a fault.
				var exit *exec.ExitError
				if e := cs.Close(); e != nil && !errors.As(e, &exit) {
					mu.Lock()
					err = errors.Join(err, e)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	})
	return err
}

func (t *Toolset) each(conns []*conn, f func(*conn)) {
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(c)
		}()
	}
	wg.Wait()
}

// dial starts a fresh session for c and lists its tools. It replaces any old
// session, and records the failure on c instead of returning it.
func (t *Toolset) dial(parent context.Context, c *conn) {
	timeout := t.opts.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	t.mu.Lock()
	changes := c.changes
	t.mu.Unlock()
	cs, tools, warnings, errTail, err := t.connect(ctx, c)
	// When the caller gave up, the failure says little about the server, so
	// an earlier error and its stderr win. With no earlier error, the
	// cancel error is recorded so that Status still says why it failed.
	gaveUp := err != nil && parent.Err() != nil
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		err = errTail.explain(err)
	}
	t.mu.Lock()
	if t.closed {
		// Close already stopped the old session. Keep the new one out of
		// the record, so Status never reports a server that Close stopped
		// as connected.
		t.mu.Unlock()
		if cs != nil {
			cs.Close()
		}
		return
	}
	old := c.session
	c.session, c.listed = cs, tools
	c.changed = c.changes != changes
	switch {
	case gaveUp:
		// Keep the earlier failure and its stderr; the server stays failed.
		c.state = StateFailed
		if c.err == nil {
			c.err = err
		}
	case err != nil:
		c.stderr, c.notes = errTail, warnings
		c.state, c.err = StateFailed, err
	default:
		c.stderr, c.notes = errTail, warnings
		c.state, c.err = StateConnected, nil
	}
	t.mu.Unlock()
	if old != nil {
		old.Close()
	}
	if cs == nil {
		return
	}
	go func() {
		cs.Wait()
		t.fail(c, cs, errTail.explain(errors.New("server exited")))
	}()
}

func (t *Toolset) connect(
	ctx context.Context,
	c *conn,
) (*mcp.ClientSession, []*mcp.Tool, []string, *tail, error) {
	// The transport comes from the entry as written, the one Open checked
	// and the user approved. An unset variable must not switch it.
	typ, err := c.server.transport()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	s, warnings := c.server.expanded()
	s.Type = typ
	if _, err := s.transport(); err != nil {
		return nil, nil, warnings, nil, err
	}
	var errTail *tail
	var transport mcp.Transport
	if typ == TypeStdio {
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Dir = t.opts.Dir
		errTail = &tail{}
		cmd.Stderr = errTail
		cmd.Env = os.Environ()
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			cmd.Env = append(cmd.Env, k+"="+s.Env[k])
		}
		// The child gets its own process group, so a Ctrl-C meant for the
		// host does not stop it. Closing the session stops the group.
		ownGroup(cmd)
		transport = &groupTransport{CommandTransport: mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 2 * time.Second,
		}}
	} else {
		transport = &mcp.StreamableClientTransport{
			Endpoint:   s.URL,
			HTTPClient: t.httpClient(c.name, s),
		}
	}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "dashi", Version: "0"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(
				context.Context,
				*mcp.ToolListChangedRequest,
			) {
				t.mu.Lock()
				c.changed = true
				c.changes++
				t.mu.Unlock()
			},
		},
	)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, warnings, errTail, err
	}
	tools, err := listAll(ctx, cs)
	if err != nil {
		cs.Close()
		return nil, nil, warnings, errTail, fmt.Errorf("list tools: %w", err)
	}
	return cs, tools, warnings, errTail, nil
}

func (t *Toolset) httpClient(name string, s Server) *http.Client {
	var base *http.Client
	if t.opts.HTTPClient != nil {
		base = t.opts.HTTPClient(name, s)
	}
	if base == nil {
		base = http.DefaultClient
	}
	if len(s.Headers) == 0 {
		return base
	}
	client := *base
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = headerTransport{next: next, headers: s.Headers}
	return &client
}

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	next    http.RoundTripper
	headers map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.next.RoundTrip(r)
}

func listAll(ctx context.Context, cs *mcp.ClientSession) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	for tl, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, tl)
	}
	return out, nil
}

// relist reads the tool list of a live session again.
func (t *Toolset) relist(ctx context.Context, c *conn, cs *mcp.ClientSession) {
	t.mu.Lock()
	changes := c.changes
	t.mu.Unlock()
	tools, err := listAll(ctx, cs)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		t.fail(c, cs, fmt.Errorf("list tools: %w", err))
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.session == cs {
		c.listed, c.changed = tools, c.changes != changes
	}
}

// fail marks c failed when cs is still its session. The tools stay in the
// snapshot until the next Refresh of any server, so a pending call gets a clear error, not a missing
// tool.
func (t *Toolset) fail(c *conn, cs *mcp.ClientSession, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || c.session != cs || c.state != StateConnected {
		return
	}
	c.state, c.err = StateFailed, err
}

// note records a runtime warning once.
func (t *Toolset) note(c *conn, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.Contains(c.notes, text) {
		c.notes = append(c.notes, text)
	}
}

// rebuild recomputes the snapshot from connected servers. The caller holds
// t.mu. It walks servers in sorted order and keeps the first tool for a name, so start order never
// changes a name.
func (t *Toolset) rebuild() {
	t.tools, t.info = nil, map[string]ToolInfo{}
	for _, name := range slices.Sorted(maps.Keys(t.conns)) {
		c := t.conns[name]
		c.dropped, c.exposed = nil, 0
		if c.state != StateConnected {
			continue
		}
		for _, remote := range c.listed {
			def, err := definition(c.name, remote)
			if err != nil {
				c.dropped = append(c.dropped, fmt.Sprintf(
					"dropped tool %q: %v",
					remote.Name,
					err,
				))
				continue
			}
			if prev, ok := t.info[def.Name]; ok {
				c.dropped = append(c.dropped, fmt.Sprintf(
					"dropped tool %q: name %s is taken by %s.%s",
					remote.Name,
					def.Name,
					prev.Server,
					prev.Remote,
				))
				continue
			}
			t.info[def.Name] = ToolInfo{Server: c.name, Remote: remote.Name}
			t.tools = append(t.tools, tool.Tool{
				Definition: def,
				Executor:   &executor{set: t, c: c, remote: remote.Name},
			})
			c.exposed++
		}
	}
}

// definition builds the model-facing definition. It copies the destructive
// hint, and the open-world hint only when it is true. It never copies the
// read-only or idempotent hint. The runner repeats a call of unknown outcome
// when a tool is marked safe to repeat, and a server must not control that
// choice. A false open-world hint would turn off checks that read outside
// content, such as toolcheck.PromptInjection, and the server's claim does
// not make the text it returns trusted: the text may come from anyone who
// can write to the server's data.
func definition(server string, remote *mcp.Tool) (tool.Definition, error) {
	schema := json.RawMessage(`{"type":"object"}`)
	if remote.InputSchema != nil {
		raw, err := json.Marshal(remote.InputSchema)
		if err != nil {
			return tool.Definition{}, fmt.Errorf("input schema: %w", err)
		}
		schema = raw
	}
	desc := remote.Description
	if desc == "" && remote.Annotations != nil {
		desc = remote.Annotations.Title
	}
	def := tool.Definition{
		Name:        toolName(server, remote.Name),
		Description: desc,
		Schema:      schema,
		Kind:        tool.KindLocal,
	}
	if a := remote.Annotations; a != nil {
		def.Annotations.Destructive = a.DestructiveHint
		if a.OpenWorldHint != nil && *a.OpenWorldHint {
			def.Annotations.OpenWorld = new(true)
		}
	}
	return def, def.Validate()
}

// executor calls one remote tool.
type executor struct {
	set    *Toolset
	c      *conn
	remote string
}

func (e *executor) Execute(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	e.set.mu.Lock()
	cs, state, cause, closed := e.c.session, e.c.state, e.c.err, e.set.closed
	e.set.mu.Unlock()
	if closed {
		return tool.Outcome{}, errClosed
	}
	if cs == nil || state != StateConnected {
		return tool.Outcome{}, fmt.Errorf(
			"MCP server %q is not connected: %v",
			e.c.name,
			cause,
		)
	}
	var args any
	if trimmed := strings.TrimSpace(string(call.Args)); trimmed != "" && trimmed != "null" {
		args = json.RawMessage(call.Args)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      e.remote,
		Arguments: args,
	})
	if err != nil {
		return tool.Outcome{}, e.callError(ctx, cs, err)
	}
	outcome, warnings := ConvertResult(res)
	for _, text := range warnings {
		e.set.note(e.c, text)
	}
	return outcome, nil
}

// callError explains a failed call. A reply from the server means the call
// did not run, so the text is the server's. A lost connection may have lost
// the reply of a call that ran; the text says so, because the client never
// retries a call.
func (e *executor) callError(
	ctx context.Context,
	cs *mcp.ClientSession,
	err error,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var reply *jsonrpc.Error
	if errors.As(err, &reply) {
		return fmt.Errorf("MCP server %q: %w", e.c.name, err)
	}
	e.set.fail(e.c, cs, errors.New("connection lost"))
	return fmt.Errorf(
		"MCP server %q lost its connection; the call may have run: %w",
		e.c.name,
		err,
	)
}
