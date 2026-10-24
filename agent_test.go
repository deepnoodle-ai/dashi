package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

var uuidV7 = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

func TestNewIDIsTimeOrderedUUIDv7(t *testing.T) {
	first := NewID()
	if !uuidV7.MatchString(first) {
		t.Fatalf("NewID() = %q, want a UUIDv7", first)
	}
	if NewID() == first {
		t.Fatal("NewID repeated a value")
	}
	time.Sleep(2 * time.Millisecond)
	if later := NewID(); later <= first {
		t.Fatalf("later ID %q does not sort after %q", later, first)
	}
}

func userInput(text string) []llm.Message {
	return []llm.Message{llm.UserMessage(llm.Text(text))}
}

func TestZeroConfigTurnGeneratesAndRecordsIdentity(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	out, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: Agent{Model: "fixture"},
		Input: userInput("hi"),
	})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	if !uuidV7.MatchString(out.Turn.ID) {
		t.Fatalf("turn ID = %q", out.Turn.ID)
	}
	first := out.Turn.Steps[0]
	if !uuidV7.MatchString(first.CommandID) || first.TurnID != out.Turn.ID {
		t.Fatalf("start step command=%q turn=%q", first.CommandID, first.TurnID)
	}
	agent := first.Started.Agent
	if agent.ID != "" || !strings.HasPrefix(agent.Version, "auto:") ||
		len(agent.Version) != len("auto:")+16 {
		t.Fatalf("recorded agent ID=%q version=%q", agent.ID, agent.Version)
	}
	// The generated turn ID reaches the recorder, so the not-found path of
	// Loader and the saved record agree with the outcome.
	saved, err := rec.Load(t.Context(), out.Turn.ID)
	if err != nil || len(saved.Steps) != len(out.Turn.Steps) {
		t.Fatalf("saved=%d err=%v", len(saved.Steps), err)
	}
	if _, err := runner.Run(t.Context(), Turn{}, Continue{}); err == nil {
		t.Fatal("Continue without a turn ID was accepted")
	}
}

func TestStreamCarriesGeneratedTurnID(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client}
	var id string
	var steps, finals int
	for event, err := range runner.Stream(
		t.Context(),
		Turn{},
		Start{Agent: Agent{Model: "fixture"}, Input: userInput("hi")},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			id = event.TurnID
		}
		if event.TurnID == "" || event.TurnID != id {
			t.Fatalf("event turn ID %q, first was %q", event.TurnID, id)
		}
		if event.Step != nil {
			steps++
		}
		if event.Result != nil {
			finals++
			if event.Result.Turn.ID != id {
				t.Fatalf("outcome turn ID %q", event.Result.Turn.ID)
			}
		}
	}
	if !uuidV7.MatchString(id) || steps == 0 || finals != 1 {
		t.Fatalf("id=%q steps=%d finals=%d", id, steps, finals)
	}
}

func TestEmptyCommandIDsAndRecordedAgent(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "lookup"), final("found")},
	}
	rec := NewMemoryRecorder()
	external := tool.List{{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}}
	agent := Agent{Model: "fixture", Instructions: "Be brief."}
	runner := Runner{Client: client, Tools: external, Recorder: rec}
	got, err := runner.Run(t.Context(), Turn{}, Start{Agent: agent, Input: userInput("hi")})
	if err != nil || got.Stop.Kind != StopWaiting {
		t.Fatalf("start stop=%+v err=%v", got.Stop, err)
	}

	// A retried Start whose instructions no longer match the record fails;
	// the error names the field and the fix. The version is pinned so the
	// derived one does not differ too.
	edited := agent
	edited.Instructions = "Be thorough."
	edited.Version = got.Turn.Steps[0].Started.Agent.Version
	_, err = runner.Run(t.Context(), got.Turn, Start{
		CommandID: got.Turn.Steps[0].CommandID,
		Agent:     edited,
		Input:     userInput("hi"),
	})
	if !errors.Is(err, ErrAgentChanged) ||
		!strings.Contains(err.Error(), "instructions") ||
		strings.Contains(err.Error(), "revision") ||
		!strings.Contains(err.Error(), "TurnView.Agent") {
		t.Fatalf("edited agent error = %v", err)
	}

	// Other commands run with the recorded agent.
	delivered, err := runner.Run(t.Context(), got.Turn, AddMessages{Items: []ContextItem{{
		Origin:  "user",
		Message: llm.UserMessage(llm.Text("more")),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	out := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("r"))}
	finished, err := runner.Run(
		t.Context(),
		delivered.Turn,
		Resolve{CallID: "call-1", Outcome: &out},
	)
	if err != nil {
		t.Fatal(err)
	}
	done, err := runner.Run(t.Context(), finished.Turn, Continue{})
	if err != nil || done.Stop.Kind != StopCompleted {
		t.Fatalf("continue stop=%+v err=%v", done.Stop, err)
	}
	ids := map[string]bool{}
	for _, step := range done.Turn.Steps {
		if step.CommandID != "" {
			ids[step.CommandID] = true
		}
	}
	if len(ids) != 4 {
		t.Fatalf("distinct command IDs = %d, want 4", len(ids))
	}
}

func TestCancelUsesRecordedAgentAfterPromptEdit(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	external := tool.List{{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}}
	runner := Runner{
		Client: client,
		Tools:  external,
	}
	got, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: Agent{Model: "fixture", Instructions: "v1"},
		Input: userInput("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := (Runner{Client: client}).Run(
		t.Context(),
		got.Turn,
		Cancel{Reason: "prompt changed"},
	)
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("cancel stop=%+v err=%v", canceled.Stop, err)
	}
}

func TestAgentVersionTracksBehaviorFields(t *testing.T) {
	base := Agent{
		ID:       "a",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
	}.resolved()
	want := agentVersion(base)
	if agentVersion(base) != want {
		t.Fatal("version is not stable")
	}
	changes := map[string]func(*Agent){
		"instructions": func(a *Agent) { a.Instructions = "x" },
		"provider":     func(a *Agent) { a.Provider = "other" },
		"api":          func(a *Agent) { a.API = "other.chat" },
		"model":        func(a *Agent) { a.Model = "other" },
		"settings":     func(a *Agent) { a.Settings.MaxTokens = 10 },
		"schema":       func(a *Agent) { a.OutputSchema = json.RawMessage(`{"type":"object"}`) },
		"limits":       func(a *Agent) { a.Limits.ModelCalls = 2 },
		"final answer": func(a *Agent) { a.Limits.NoReservedFinalReply = true },
	}
	for name, change := range changes {
		a := base
		change(&a)
		if agentVersion(a) == want {
			t.Errorf("%s change kept version %s", name, want)
		}
	}
	same := map[string]func(*Agent){
		"id":       func(a *Agent) { a.ID = "b" },
		"version":  func(a *Agent) { a.Version = "r9" },
		"deadline": func(a *Agent) { a.Limits.Deadline = time.Now().Add(time.Hour) },
	}
	for name, change := range same {
		a := base
		change(&a)
		if agentVersion(a) != want {
			t.Errorf("%s change altered version", name)
		}
	}
}

func TestNoHooksAllowBoundTools(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "echo"), final("done")},
	}
	ran := false
	local := tool.Tool{
		Definition: tool.Definition{Name: "echo", Schema: []byte(`{"type":"object"}`)},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			ran = true
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("ok")),
			}, nil
		}),
	}
	out, err := start(t, client, tool.List{local}, nil, Hooks{})
	if err != nil || out.Stop.Kind != StopCompleted || !ran {
		t.Fatalf("stop=%+v err=%v ran=%v", out.Stop, err, ran)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Tools["call-1"].Verdict; got.Kind != DecisionAllow ||
		got.Reason != "no hook opinion" || got.Hook != "" {
		t.Fatalf("verdict = %+v", got)
	}

	external := tool.Tool{Definition: tool.Definition{
		Name:   "echo",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	client = &scriptedClient{results: []llm.Result{callResult("call-1", "echo")}}
	out, err = start(t, client, tool.List{external}, nil, Hooks{})
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("external stop=%+v err=%v", out.Stop, err)
	}
}

// catalogClient is a fixture destination named like a catalog provider that
// knows the models in known.
type catalogClient struct {
	scriptedClient
	provider, api string
	known         map[string]bool
}

func (c *catalogClient) Provider() string { return c.provider }
func (c *catalogClient) API() string      { return c.api }

func (c *catalogClient) ModelInfo(model string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, c.known[model]
}

func (c *catalogClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: c.provider,
			API:      c.api,
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{"ok":true}`),
	})
}

func finalFrom(provider, model string) llm.Result {
	r := final("done")
	r.Requested = llm.ModelRef{Provider: provider, Model: model}
	return r
}

func TestEmptyModelUsesCatalogDefault(t *testing.T) {
	def, ok := catalog.Default("anthropic")
	if !ok {
		t.Fatal("no anthropic default")
	}
	client := &catalogClient{
		scriptedClient: scriptedClient{results: []llm.Result{
			finalFrom("anthropic", def),
		}},
		provider: "anthropic",
		api:      "anthropic.messages",
		known:    map[string]bool{def: true},
	}
	out, err := (Runner{Client: client}).Run(
		t.Context(),
		Turn{},
		Start{Input: userInput("hi")},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got := out.Turn.Steps[0].Started.Agent.Model; got != def {
		t.Fatalf("model = %q, want %q", got, def)
	}

	// A client that does not know the default model gets a clear error.
	unknown := &catalogClient{provider: "anthropic", api: "anthropic.messages"}
	_, err = (Runner{Client: unknown}).Run(t.Context(), Turn{}, Start{Input: userInput("hi")})
	if err == nil || !strings.Contains(err.Error(), "set Start.Agent.Model") {
		t.Fatalf("unknown default error = %v", err)
	}
	_, err = (Runner{Client: &scriptedClient{}}).Run(
		t.Context(),
		Turn{},
		Start{Input: userInput("hi")},
	)
	if err == nil || !strings.Contains(err.Error(), `provider "test"`) {
		t.Fatalf("no-catalog error = %v", err)
	}
}

func TestContinueKeepsRecordedModel(t *testing.T) {
	const small = "claude-haiku-5-5"
	client := &catalogClient{
		scriptedClient: scriptedClient{results: []llm.Result{
			callResultFrom("anthropic", small),
			finalFrom("anthropic", small),
		}},
		provider: "anthropic",
		api:      "anthropic.messages",
		known:    map[string]bool{small: true},
	}
	external := tool.List{{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}}
	runner := Runner{
		Client: client,
		Tools:  external,
	}
	got, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: Agent{Model: small, Instructions: "Be brief."},
		Input: userInput("hi"),
	})
	if err != nil || got.Stop.Kind != StopWaiting {
		t.Fatalf("start stop=%+v err=%v", got.Stop, err)
	}
	out := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("r"))}
	finished, err := runner.Run(t.Context(), got.Turn, Resolve{CallID: "call-1", Outcome: &out})
	if err != nil {
		t.Fatal(err)
	}

	// A retried Start with a different model is a change.
	_, err = runner.Run(t.Context(), finished.Turn, Start{
		CommandID: got.Turn.Steps[0].CommandID,
		Agent:     Agent{Model: "claude-sonnet-5", Instructions: "Be brief."},
		Input:     userInput("hi"),
	})
	if !errors.Is(err, ErrAgentChanged) || !strings.Contains(err.Error(), "model") {
		t.Fatalf("changed model error = %v", err)
	}

	// Continue runs with the recorded model, not today's catalog default.
	done, err := runner.Run(t.Context(), finished.Turn, Continue{})
	if err != nil || done.Stop.Kind != StopCompleted {
		t.Fatalf("continue stop=%+v err=%v", done.Stop, err)
	}
	if got := client.inputs[len(client.inputs)-1].Model; got != small {
		t.Fatalf("continued with model %q", got)
	}
}

func callResultFrom(provider, model string) llm.Result {
	r := callResult("call-1", "lookup")
	r.Requested = llm.ModelRef{Provider: provider, Model: model}
	return r
}

func TestRejectsSnapshotWithTamperedAgent(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "lookup"), final("ok")},
	}
	rec := NewMemoryRecorder()
	external := tool.List{{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}}
	runner := Runner{
		Client:   client,
		Tools:    external,
		Recorder: rec,
	}
	got, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: Agent{Model: "fixture", Instructions: "safe"},
		Input: userInput("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := cloneTurn(got.Turn)
	tampered.Steps[0].Started.Agent.Instructions = "EVIL"
	out := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("r"))}
	_, err = runner.Run(t.Context(), tampered, Resolve{CallID: "call-1", Outcome: &out})
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("tampered snapshot error = %v, want ErrAgentChanged", err)
	}
	saved, err := rec.Load(t.Context(), got.Turn.ID)
	if err != nil || len(saved.Steps) != len(got.Turn.Steps) {
		t.Fatalf("recorder changed: steps=%d err=%v", len(saved.Steps), err)
	}
	// The untampered snapshot still works.
	if _, err := runner.Run(
		t.Context(),
		got.Turn,
		Resolve{CallID: "call-1", Outcome: &out},
	); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedCommandIDIsPartOfHash(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}
	start := Start{Agent: Agent{Model: "fixture"}, Input: userInput("hi")}
	out, err := runner.Run(t.Context(), Turn{}, start)
	if err != nil {
		t.Fatal(err)
	}
	// Repeating the command under the generated ID it was recorded with is
	// the same command, so it returns the recorded result.
	start.CommandID = out.Turn.Steps[0].CommandID
	again, err := runner.Run(t.Context(), out.Turn, start)
	if err != nil || len(again.Turn.Steps) != len(out.Turn.Steps) ||
		client.sends != 1 {
		t.Fatalf("repeat: steps=%d sends=%d err=%v",
			len(again.Turn.Steps), client.sends, err)
	}
}

func TestAgentVersionIsPinned(t *testing.T) {
	// A change to this value changes the version of every recorded agent
	// that relied on the derived one; make it deliberately.
	agent := Agent{
		Provider:     "anthropic",
		API:          "anthropic.messages",
		Model:        "claude-sonnet-5",
		Instructions: "Be brief.",
	}.resolved()
	if got := agentVersion(agent); got != "auto:20557d1d87035206" {
		t.Fatalf("agentVersion = %q", got)
	}
}

func TestEmptyNonNilFieldsMatchRecordedAgent(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	external := tool.List{{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}}
	runner := Runner{
		Client: client,
		Tools:  external,
	}
	start := Start{
		CommandID: "start",
		Agent:     Agent{Model: "fixture", Instructions: "v1"},
		Input:     userInput("hi"),
	}
	got, err := runner.Run(t.Context(), Turn{}, start)
	if err != nil {
		t.Fatal(err)
	}
	// An empty non-nil schema is the same agent as a nil one.
	start.Agent.OutputSchema = json.RawMessage{}
	if _, err := runner.Run(t.Context(), got.Turn, start); err != nil {
		t.Fatalf("empty non-nil schema: %v", err)
	}
}

func TestLimitsZeroValueResolvesToDefaults(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{}
	want := Limits{
		ModelCalls:      8,
		Resumes:         10,
		ProviderPauses:  3,
		ToolConcurrency: 1,
		ToolResultBytes: 64 << 10,
	}
	if got := def.resolved().Limits; got != want {
		t.Fatalf("zero limits resolved to %+v, want %+v", got, want)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("zero limits were rejected: %v", err)
	}
}

func TestLimitsResolveKeepsExplicitValues(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{
		ModelCalls:           2,
		Resumes:              4,
		ProviderPauses:       5,
		ToolConcurrency:      6,
		ToolResultBytes:      2048,
		NoReservedFinalReply: true,
	}
	if got := def.resolved().Limits; got != def.Limits {
		t.Fatalf("explicit limits resolved to %+v, want %+v", got, def.Limits)
	}
}

func TestLimitsNegativeCountResolvesToNone(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{Resumes: -5, ProviderPauses: -2}
	if err := def.Validate(); err != nil {
		t.Fatalf("negative none counts were rejected: %v", err)
	}
	got := def.resolved().Limits
	if got.Resumes != -1 {
		t.Fatalf("interruptions resolved to %d, want -1", got.Resumes)
	}
	if got.ProviderPauses != -1 {
		t.Fatalf("provider pauses resolved to %d, want -1", got.ProviderPauses)
	}
}

func TestLimitsRejectNegativeModelCallsAndConcurrency(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
	}{
		{name: "model calls", limits: Limits{ModelCalls: -1}},
		{name: "tool concurrency", limits: Limits{ToolConcurrency: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := testAgent()
			def.Limits = tt.limits
			if err := def.Validate(); err == nil {
				t.Fatal("negative limit was accepted")
			}
		})
	}
}

func TestNegativeProviderPausesAllowsNone(t *testing.T) {
	def := testAgent()
	def.Limits.ProviderPauses = -1
	paused := llm.Result{
		Message: llm.AssistantMessage(llm.Text("working")),
		Stop:    llm.Stop{Kind: llm.StopPause},
	}
	client := &scriptedClient{results: []llm.Result{paused, final("done")}}
	out, err := (Runner{Client: client}).Run(
		t.Context(),
		Turn{ID: "pauses"},
		limitsStart(def),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || out.Stop.Reason != "provider_pauses" {
		t.Fatalf("paused turn stopped with %+v, want provider_pauses", out.Stop)
	}
	if out.Stop.Limit == nil || out.Stop.Limit.Limit != 0 {
		t.Fatalf("limit detail=%+v, want a limit of 0", out.Stop.Limit)
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d, want no continuation after the pause", client.sends)
	}
}

func TestNegativeResumesAllowsNoResume(t *testing.T) {
	def := testAgent()
	def.Limits.Resumes = -1
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	rec := NewMemoryRecorder()
	pause := make(chan struct{})
	tools := tool.List{pausingTool(pause)}
	first, err := (Runner{Client: client, Tools: tools, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "resumes"},
		limitsStart(def),
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	client.sends = 0
	if first.ReadyToContinue {
		t.Fatal("interrupted turn is ready to continue with no resumes allowed")
	}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "resume"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || out.Stop.Reason != "resumes" {
		t.Fatalf("resume stopped with %+v, want interruptions", out.Stop)
	}
	if out.Stop.Limit == nil || out.Stop.Limit.Limit != 0 {
		t.Fatalf("limit detail=%+v, want a limit of 0", out.Stop.Limit)
	}
	if client.sends != 0 {
		t.Fatalf("sends=%d, want no model call", client.sends)
	}
}

func TestNoReservedFinalReplyAdvertisesToolsOnLastModelCall(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelCalls: 2, NoReservedFinalReply: true}
	client := &scriptedClient{
		results: []llm.Result{callResult("lookup-1", "lookup"), final("42")},
	}
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(lookup42),
	}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{binding},
	}).Run(t.Context(), Turn{ID: "no-reserve"}, limitsStart(def))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v, want completed", out.Stop, err)
	}
	if len(client.inputs) != 2 || len(client.inputs[1].Tools) != 1 {
		t.Fatalf("last model call did not advertise tools: %+v", client.inputs)
	}
}

func TestRecordedAgentKeepsResolvedLimits(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	pause := make(chan struct{})
	close(pause)
	first, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "recorded"},
		limitsStart(def),
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	var record struct {
		Limits map[string]any `json:"limits"`
	}
	raw, err := json.Marshal(first.Turn.Steps[0].Started.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]float64{
		"model_calls":      8,
		"resumes":          10,
		"provider_pauses":  3,
		"tool_concurrency": 1,
	} {
		if record.Limits[field] != want {
			got := record.Limits[field]
			t.Fatalf("recorded %s=%v, want %v", field, got, want)
		}
	}

	// A record made under different defaults keeps its own numbers. The
	// caller's zero limits now resolve differently, so a retried Start with
	// them no longer matches, while Continue runs with the recorded limits.
	old := first.Turn
	old.Steps = cloneTurn(first.Turn).Steps
	older := *old.Steps[0].Started
	older.Agent.Limits.Resumes = 7
	old.Steps[0].Started = &older
	view, err := old.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Agent.Limits.Resumes != 7 {
		t.Fatalf(
			"replayed interruptions=%d, want recorded 7",
			view.Agent.Limits.Resumes,
		)
	}
	runner := Runner{Client: client}
	if _, err := runner.Run(t.Context(), old, limitsStart(def)); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("zero limits matched a record with other values: %v", err)
	}
	out, err := runner.Run(t.Context(), old, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("continue with recorded limits: stop=%+v err=%v", out.Stop, err)
	}
}

func limitsStart(def Agent) Start {
	return Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	}
}

func lookup42(context.Context, tool.Call) (tool.Outcome, error) {
	return tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("42")),
	}, nil
}

func TestNoRaiseEndsTheTurnAtItsModelCallLimit(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		callResult("c1", "lookup"), callResult("c2", "lookup"),
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 2
	agent.Limits.NoReservedFinalReply = true
	agent.Limits.NoRaise = true
	runs := 0
	runner := Runner{Client: client, Tools: tool.List{countingTool("lookup", &runs)}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startAgent(agent))
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitModelCalls ||
		!out.Stop.Ended || out.Next() != NextNone {
		t.Fatalf("stop=%+v next=%s err=%v", out.Stop, out.Next(), err)
	}
}

func TestNoRaiseRejectsARaise(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	agent := testAgent()
	agent.Limits.NoRaise = true
	runner := Runner{Client: client, Recorder: NewMemoryRecorder()}
	saved, err := runner.Record(t.Context(), Turn{ID: "t1"}, startAgent(agent))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), saved.Turn, Continue{ModelCalls: 20}); err == nil {
		t.Fatal("a raise was accepted with NoRaise")
	}
}

// Agent.Validate checks settings as a request would, so a NaN temperature
// or an invalid task budget fails at Start.
func TestAgentValidateChecksSettings(t *testing.T) {
	for name, s := range map[string]llm.Settings{
		"nan temperature": {Temperature: new(math.NaN())},
		"inf temperature": {Temperature: new(math.Inf(1))},
		"task budget":     {TaskBudget: &llm.TaskBudget{Total: -1}},
		"max tokens":      {MaxTokens: -1},
	} {
		if err := (Agent{Settings: s}).Validate(); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	choice := Agent{Settings: llm.Settings{ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "read"}}}
	if err := choice.Validate(); err != nil {
		t.Fatalf("named tool choice: %v", err)
	}
}
