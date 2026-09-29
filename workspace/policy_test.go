package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// planFor builds a plan for a tool that the Workspace did not build.
func planFor(t *testing.T, name string, args any, readOnly *bool) dashi.ToolPlan {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	def := tool.Definition{Name: name}
	def.Annotations.ReadOnly = readOnly
	return dashi.ToolPlan{
		Call:       tool.Call{ID: "c1", Name: name, Args: raw},
		Definition: def,
	}
}

// policyFixture is a Workspace in a temporary directory with its tools.
type policyFixture struct {
	ws    *Workspace
	dir   string
	tools tool.List
}

func newPolicyFixture(t *testing.T) *policyFixture {
	t.Helper()
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ask := func(context.Context, Question) (Answer, error) { return Answer{}, nil }
	return &policyFixture{ws: ws, dir: ws.Dir(), tools: ws.Tools(WithAsker(ask))}
}

// plan builds a plan for the Workspace's own tool.
func (f *policyFixture) plan(t *testing.T, name string, args any) dashi.ToolPlan {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range f.tools {
		if tl.Definition.Name == name {
			return dashi.ToolPlan{
				Call:       tool.Call{ID: "c1", Name: name, Args: raw},
				Definition: tl.Definition,
			}
		}
	}
	t.Fatalf("no tool %s", name)
	return dashi.ToolPlan{}
}

func boolPtr(b bool) *bool {
	return &b
}

// recordingAsk answers each request with answer and records the requests.
type recordingAsk struct {
	mu       sync.Mutex
	answer   Approval
	requests []ApprovalRequest
}

func (r *recordingAsk) ask(_ context.Context, req ApprovalRequest) (Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return r.answer, nil
}

func TestPolicyRunsOwnReadOnlyToolsWithoutPrompt(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask)
	for _, name := range []string{ToolReadFile, ToolListDirectory, ToolGlob, ToolGrep, ToolAskUser} {
		d, err := p.decide(context.Background(), f.plan(t, name, map[string]any{}))
		if err != nil || d.Kind != dashi.DecisionAllow {
			t.Errorf("%s: decision = %+v, err = %v", name, d, err)
		}
	}
	if len(r.requests) != 0 {
		t.Fatalf("policy asked %d times", len(r.requests))
	}
}

func TestPolicyDoesNotTrustToolsOfOtherPackages(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask)
	ctx := context.Background()
	// An MCP tool with a new name and a ReadOnly hint.
	d, err := p.decide(ctx, planFor(t, "mcp_query", map[string]any{}, boolPtr(true)))
	if err != nil || d.Kind != dashi.DecisionDeny || len(r.requests) != 1 {
		t.Fatalf("decision = %+v, err = %v, asks = %d", d, err, len(r.requests))
	}
	// An outside tool that reuses the name and the hint of a package tool,
	// for example after WithoutTools removed the package tool.
	for _, name := range []string{ToolGrep, ToolReadFile, ToolAskUser} {
		before := len(r.requests)
		p.decide(ctx, planFor(t, name, map[string]any{}, boolPtr(true)))
		if len(r.requests) != before+1 {
			t.Fatalf("an outside %s skipped the prompt", name)
		}
	}
	// A copy of the name, description and hint still differs in its schema.
	own := f.plan(t, ToolGrep, map[string]any{})
	own.Definition.Schema = json.RawMessage(`{"type":"object"}`)
	before := len(r.requests)
	p.decide(ctx, own)
	if len(r.requests) != before+1 {
		t.Fatal("a tool with a changed schema skipped the prompt")
	}
}

func TestPolicyOutsideToolDoesNotShareRemembered(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{answer: Approval{Allow: true, Remember: true}}
	p := f.ws.Policy(r.ask)
	ctx := context.Background()
	args := map[string]any{"path": "a.go", "content": "x"}
	p.decide(ctx, f.plan(t, ToolWriteFile, args))
	p.decide(ctx, f.plan(t, ToolWriteFile, args))
	if len(r.requests) != 1 {
		t.Fatalf("own tool asked %d times", len(r.requests))
	}
	p.decide(ctx, planFor(t, ToolWriteFile, args, nil))
	if len(r.requests) != 2 {
		t.Fatal("an outside write_file used the rule of the package tool")
	}
	if r.requests[1].Preview != "" {
		t.Fatal("an outside tool got a package preview")
	}
}

func TestPolicyTrustToolsSkipsPrompt(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask, TrustTools("mcp_query"))
	d, _ := p.decide(context.Background(), planFor(t, "mcp_query", map[string]any{}, nil))
	if d.Kind != dashi.DecisionAllow || len(r.requests) != 0 {
		t.Fatalf("decision = %+v, asks = %d", d, len(r.requests))
	}
}

func TestPolicyAllowsAndDeniesFromAnswer(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{answer: Approval{Allow: true}}
	p := f.ws.Policy(r.ask)
	plan := f.plan(t, ToolBash, map[string]any{"command": "go test ./..."})
	d, _ := p.decide(context.Background(), plan)
	if d.Kind != dashi.DecisionAllow {
		t.Fatalf("decision = %+v", d)
	}
	r.answer = Approval{Reason: "Use make test."}
	d, _ = p.decide(context.Background(), plan)
	if d.Kind != dashi.DecisionDeny || d.Reason != "Use make test." {
		t.Fatalf("decision = %+v", d)
	}
	r.answer = Approval{}
	d, _ = p.decide(context.Background(), plan)
	wantContains(t, d.Reason, "Do not retry the same call")
}

func TestPolicyRequestForBashUsesWorkspaceDir(t *testing.T) {
	f := newPolicyFixture(t)
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask)
	plan := f.plan(t, ToolBash, map[string]any{"command": "go test ./...\necho x"})
	if _, err := p.decide(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	got := r.requests[0]
	if got.Command != "go test ./...\necho x" || got.Dir != f.dir ||
		got.Summary != "run: go test ./... ..." || got.Tool != ToolBash {
		t.Fatalf("request = %+v", got)
	}
}

func TestPolicyRequestCarriesDiff(t *testing.T) {
	f := newPolicyFixture(t)
	writeTestFile(t, f.dir, "m.go", "a\nb\nc\n")
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask)
	edit := f.plan(t, ToolEditFile, map[string]any{"path": "m.go", "old_string": "b", "new_string": "B"})
	write := f.plan(t, ToolWriteFile, map[string]any{"path": "new.go", "content": "x\n"})
	over := f.plan(t, ToolWriteFile, map[string]any{"path": "m.go", "content": "z\n"})
	escape := f.plan(t, ToolWriteFile, map[string]any{"path": "../x", "content": "x\n"})
	for _, plan := range []dashi.ToolPlan{edit, write, over, escape} {
		if _, err := p.decide(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.requests[0]; got.Summary != "edit m.go" || !strings.Contains(got.Preview, "-b\n+B") {
		t.Fatalf("edit request = %+v", got)
	}
	if got := r.requests[1]; got.Summary != "write new.go" || !strings.Contains(got.Preview, "+x") {
		t.Fatalf("write request = %+v", got)
	}
	// The preview of an overwrite shows the old lines, never a new-file diff.
	if got := r.requests[2]; !strings.Contains(got.Preview, "-a") || strings.Contains(got.Preview, "-0,0") {
		t.Fatalf("overwrite preview = %q", got.Preview)
	}
	if got := r.requests[3]; got.Preview != "" {
		t.Fatalf("a path outside the workspace got a preview: %q", got.Preview)
	}
}

func TestPolicyEditThatCannotSucceedSkipsPrompt(t *testing.T) {
	f := newPolicyFixture(t)
	writeTestFile(t, f.dir, "m.go", "a\na\nb\n")
	r := &recordingAsk{}
	p := f.ws.Policy(r.ask)
	ctx := context.Background()
	for name, args := range map[string]map[string]any{
		"missing text": {"path": "m.go", "old_string": "zzz", "new_string": "y"},
		"not unique":   {"path": "m.go", "old_string": "a", "new_string": "y"},
		"no file":      {"path": "none.go", "old_string": "a", "new_string": "y"},
		"outside":      {"path": "../x", "old_string": "a", "new_string": "y"},
	} {
		d, err := p.decide(ctx, f.plan(t, ToolEditFile, args))
		if err != nil || d.Kind != dashi.DecisionAllow {
			t.Errorf("%s: decision = %+v, err = %v", name, d, err)
		}
	}
	if len(r.requests) != 0 {
		t.Fatalf("policy asked %d times", len(r.requests))
	}
	// An edit that will work still asks.
	p.decide(ctx, f.plan(t, ToolEditFile, map[string]any{"path": "m.go", "old_string": "b", "new_string": "c"}))
	if len(r.requests) != 1 {
		t.Fatal("a working edit skipped the prompt")
	}
	// The tools themselves report the failures to the model.
	out, text := call(t, f.tools, ToolEditFile, map[string]any{"path": "m.go", "old_string": "a", "new_string": "y"})
	wantState(t, out, text, tool.StateFailed)
}

func TestPolicyRememberCoversFileToolsAndExactCommands(t *testing.T) {
	f := newPolicyFixture(t)
	writeTestFile(t, f.dir, "a.go", "a")
	writeTestFile(t, f.dir, "b.go", "a")
	r := &recordingAsk{answer: Approval{Allow: true, Remember: true}}
	p := f.ws.Policy(r.ask)
	ctx := context.Background()
	edit := func(path string) dashi.ToolPlan {
		return f.plan(t, ToolEditFile, map[string]any{"path": path, "old_string": "a", "new_string": "b"})
	}
	bash := func(cmd string) dashi.ToolPlan {
		return f.plan(t, ToolBash, map[string]any{"command": cmd})
	}
	steps := []struct {
		plan dashi.ToolPlan
		asks int
	}{
		{edit("a.go"), 1},
		{edit("b.go"), 1},
		{bash("go test ./..."), 2},
		{bash("go test ./..."), 2},
		{bash("go test ./... && rm -rf ~"), 3},
	}
	for i, s := range steps {
		d, err := p.decide(ctx, s.plan)
		if err != nil || d.Kind != dashi.DecisionAllow {
			t.Fatalf("step %d: %+v, %v", i, d, err)
		}
		if len(r.requests) != s.asks {
			t.Fatalf("step %d: %d asks, want %d", i, len(r.requests), s.asks)
		}
	}
	// A new Policy starts with no rules.
	if len(f.ws.Policy(r.ask).rules) != 0 {
		t.Fatal("a new Policy has rules")
	}
}

func TestPolicyDoesNotRememberDenial(t *testing.T) {
	f := newPolicyFixture(t)
	writeTestFile(t, f.dir, "a", "a")
	r := &recordingAsk{answer: Approval{Remember: true}}
	p := f.ws.Policy(r.ask)
	plan := f.plan(t, ToolEditFile, map[string]any{"path": "a", "old_string": "a", "new_string": "b"})
	p.decide(context.Background(), plan)
	p.decide(context.Background(), plan)
	if len(r.requests) != 2 {
		t.Fatalf("asked %d times", len(r.requests))
	}
}

func TestPolicyDefersWithoutAsk(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.ws.Policy(nil)
	d, err := p.decide(context.Background(), f.plan(t, ToolBash, map[string]any{"command": "ls"}))
	if err != nil || d.Kind != dashi.DecisionDefer {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	d, _ = p.decide(context.Background(), f.plan(t, ToolReadFile, map[string]any{}))
	if d.Kind != dashi.DecisionAllow {
		t.Fatalf("read-only decision = %+v", d)
	}
}

func TestPolicyAskErrorIsReturned(t *testing.T) {
	f := newPolicyFixture(t)
	want := errors.New("no terminal")
	p := f.ws.Policy(func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{}, want
	})
	_, err := p.decide(context.Background(), f.plan(t, ToolBash, map[string]any{"command": "ls"}))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyAsksOneQuestionAtATime(t *testing.T) {
	f := newPolicyFixture(t)
	var mu sync.Mutex
	active, peak := 0, 0
	p := f.ws.Policy(func(context.Context, ApprovalRequest) (Approval, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return Approval{Allow: true}, nil
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			p.decide(context.Background(), f.plan(t, ToolBash, map[string]any{"command": "ls"}))
		})
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("%d prompts overlapped", peak)
	}
}

func TestPolicyHooksHoldOneBeforeToolHook(t *testing.T) {
	f := newPolicyFixture(t)
	h := f.ws.Policy(nil).Hooks()
	if len(h.BeforeTool) != 1 || h.BeforeTool[0].Name == "" {
		t.Fatalf("hooks = %+v", h)
	}
}
