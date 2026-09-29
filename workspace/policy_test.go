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

func TestPolicyRunsReadOnlyToolsWithoutPrompt(t *testing.T) {
	r := &recordingAsk{}
	p := NewPolicy(r.ask)
	for _, name := range readOnlyTools {
		d, err := p.decide(context.Background(), planFor(t, name, map[string]any{}, boolPtr(true)))
		if err != nil || d.Kind != dashi.DecisionAllow {
			t.Errorf("%s: decision = %+v, err = %v", name, d, err)
		}
	}
	if len(r.requests) != 0 {
		t.Fatalf("policy asked %d times", len(r.requests))
	}
}

func TestPolicyDoesNotTrustAnnotationFromOtherTools(t *testing.T) {
	r := &recordingAsk{}
	p := NewPolicy(r.ask)
	d, err := p.decide(context.Background(), planFor(t, "mcp_query", map[string]any{}, boolPtr(true)))
	if err != nil || d.Kind != dashi.DecisionDeny || len(r.requests) != 1 {
		t.Fatalf("decision = %+v, err = %v, asks = %d", d, err, len(r.requests))
	}
	// A package tool name without the annotation is not trusted either.
	d, _ = p.decide(context.Background(), planFor(t, ToolGrep, map[string]any{}, nil))
	if len(r.requests) != 2 {
		t.Fatalf("unannotated %s skipped the prompt", ToolGrep)
	}
}

func TestPolicyTrustToolsSkipsPrompt(t *testing.T) {
	r := &recordingAsk{}
	p := NewPolicy(r.ask, TrustTools("mcp_query"))
	d, _ := p.decide(context.Background(), planFor(t, "mcp_query", map[string]any{}, nil))
	if d.Kind != dashi.DecisionAllow || len(r.requests) != 0 {
		t.Fatalf("decision = %+v, asks = %d", d, len(r.requests))
	}
}

func TestPolicyAllowsAndDeniesFromAnswer(t *testing.T) {
	r := &recordingAsk{answer: Approval{Allow: true}}
	p := NewPolicy(r.ask)
	plan := planFor(t, ToolBash, map[string]any{"command": "go test ./..."}, boolPtr(false))
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

func TestPolicyRequestForBash(t *testing.T) {
	r := &recordingAsk{}
	p := NewPolicy(r.ask, InDir("/repo"))
	plan := planFor(t, ToolBash, map[string]any{"command": "go test ./...\necho x"}, boolPtr(false))
	if _, err := p.decide(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	got := r.requests[0]
	if got.Command != "go test ./...\necho x" || got.Dir != "/repo" ||
		got.Summary != "run: go test ./... ..." || got.Tool != ToolBash {
		t.Fatalf("request = %+v", got)
	}
}

func TestPolicyRequestCarriesDiff(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "m.go", "a\nb\nc\n")
	r := &recordingAsk{}
	p := NewPolicy(r.ask, InDir(dir))
	edit := planFor(t, ToolEditFile, map[string]any{"path": "m.go", "old_string": "b", "new_string": "B"}, boolPtr(false))
	write := planFor(t, ToolWriteFile, map[string]any{"path": "new.go", "content": "x\n"}, boolPtr(false))
	escape := planFor(t, ToolWriteFile, map[string]any{"path": "../x", "content": "x\n"}, boolPtr(false))
	for _, plan := range []dashi.ToolPlan{edit, write, escape} {
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
	if got := r.requests[2]; got.Preview != "" {
		t.Fatalf("a path outside the workspace got a preview: %q", got.Preview)
	}
}

func TestPolicyRememberCoversFileToolsAndExactCommands(t *testing.T) {
	r := &recordingAsk{answer: Approval{Allow: true, Remember: true}}
	p := NewPolicy(r.ask)
	ctx := context.Background()
	edit := func(path string) dashi.ToolPlan {
		return planFor(t, ToolEditFile, map[string]any{"path": path, "old_string": "a", "new_string": "b"}, boolPtr(false))
	}
	bash := func(cmd string) dashi.ToolPlan {
		return planFor(t, ToolBash, map[string]any{"command": cmd}, boolPtr(false))
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
	if len(NewPolicy(r.ask).rules) != 0 {
		t.Fatal("a new Policy has rules")
	}
}

func TestPolicyDoesNotRememberDenial(t *testing.T) {
	r := &recordingAsk{answer: Approval{Remember: true}}
	p := NewPolicy(r.ask)
	plan := planFor(t, ToolEditFile, map[string]any{"path": "a"}, boolPtr(false))
	p.decide(context.Background(), plan)
	p.decide(context.Background(), plan)
	if len(r.requests) != 2 {
		t.Fatalf("asked %d times", len(r.requests))
	}
}

func TestPolicyDefersWithoutAsk(t *testing.T) {
	p := NewPolicy(nil)
	d, err := p.decide(context.Background(), planFor(t, ToolBash, map[string]any{"command": "ls"}, boolPtr(false)))
	if err != nil || d.Kind != dashi.DecisionDefer {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	d, _ = p.decide(context.Background(), planFor(t, ToolReadFile, map[string]any{}, boolPtr(true)))
	if d.Kind != dashi.DecisionAllow {
		t.Fatalf("read-only decision = %+v", d)
	}
}

func TestPolicyAskErrorIsReturned(t *testing.T) {
	want := errors.New("no terminal")
	p := NewPolicy(func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{}, want
	})
	_, err := p.decide(context.Background(), planFor(t, ToolBash, map[string]any{"command": "ls"}, boolPtr(false)))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyAsksOneQuestionAtATime(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	p := NewPolicy(func(context.Context, ApprovalRequest) (Approval, error) {
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
			p.decide(context.Background(), planFor(t, ToolBash, map[string]any{"command": "ls"}, boolPtr(false)))
		})
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("%d prompts overlapped", peak)
	}
}

func TestPolicyHooksHoldOneBeforeToolHook(t *testing.T) {
	h := NewPolicy(nil).Hooks()
	if len(h.BeforeTool) != 1 || h.BeforeTool[0].Name == "" {
		t.Fatalf("hooks = %+v", h)
	}
}
