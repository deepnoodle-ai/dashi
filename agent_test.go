package dashi

import (
	"context"
	"encoding/json"
	"errors"
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
	if agent.ID != "" || !strings.HasPrefix(agent.Revision, "auto:") ||
		len(agent.Revision) != len("auto:")+16 {
		t.Fatalf("recorded agent ID=%q revision=%q", agent.ID, agent.Revision)
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
		if event.Outcome != nil {
			finals++
			if event.Outcome.Turn.ID != id {
				t.Fatalf("outcome turn ID %q", event.Outcome.Turn.ID)
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
	// the error names the field and the fix. The revision is pinned so the
	// derived one does not differ too.
	edited := agent
	edited.Instructions = "Be thorough."
	edited.Revision = got.Turn.Steps[0].Started.Agent.Revision
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
	delivered, err := runner.Run(t.Context(), got.Turn, Deliver{Items: []ContextItem{{
		Origin:  "user",
		Message: llm.UserMessage(llm.Text("more")),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	out := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("r"))}
	settled, err := runner.Run(
		t.Context(),
		delivered.Turn,
		Settle{CallID: "call-1", Outcome: &out},
	)
	if err != nil {
		t.Fatal(err)
	}
	done, err := runner.Run(t.Context(), settled.Turn, Continue{})
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

func TestAgentRevisionTracksBehaviorFields(t *testing.T) {
	base := Agent{
		ID:       "a",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
	}.resolved()
	want := agentRevision(base)
	if agentRevision(base) != want {
		t.Fatal("revision is not stable")
	}
	changes := map[string]func(*Agent){
		"instructions": func(a *Agent) { a.Instructions = "x" },
		"provider":     func(a *Agent) { a.Provider = "other" },
		"api":          func(a *Agent) { a.API = "other.chat" },
		"model":        func(a *Agent) { a.Model = "other" },
		"settings":     func(a *Agent) { a.Settings.MaxTokens = 10 },
		"schema":       func(a *Agent) { a.OutputSchema = json.RawMessage(`{"type":"object"}`) },
		"limits":       func(a *Agent) { a.Limits.ModelAttempts = 2 },
		"final answer": func(a *Agent) { a.Limits.NoReservedFinalAnswer = true },
	}
	for name, change := range changes {
		a := base
		change(&a)
		if agentRevision(a) == want {
			t.Errorf("%s change kept revision %s", name, want)
		}
	}
	same := map[string]func(*Agent){
		"id":       func(a *Agent) { a.ID = "b" },
		"revision": func(a *Agent) { a.Revision = "r9" },
		"deadline": func(a *Agent) { a.Limits.Deadline = time.Now().Add(time.Hour) },
	}
	for name, change := range same {
		a := base
		change(&a)
		if agentRevision(a) != want {
			t.Errorf("%s change altered revision", name)
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
	view, err := Replay(out.Turn)
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
	const small = "claude-haiku-4-5"
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
	settled, err := runner.Run(t.Context(), got.Turn, Settle{CallID: "call-1", Outcome: &out})
	if err != nil {
		t.Fatal(err)
	}

	// A retried Start with a different model is a change.
	_, err = runner.Run(t.Context(), settled.Turn, Start{
		CommandID: got.Turn.Steps[0].CommandID,
		Agent:     Agent{Model: "claude-sonnet-5", Instructions: "Be brief."},
		Input:     userInput("hi"),
	})
	if !errors.Is(err, ErrAgentChanged) || !strings.Contains(err.Error(), "model") {
		t.Fatalf("changed model error = %v", err)
	}

	// Continue runs with the recorded model, not today's catalog default.
	done, err := runner.Run(t.Context(), settled.Turn, Continue{})
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
	_, err = runner.Run(t.Context(), tampered, Settle{CallID: "call-1", Outcome: &out})
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
		Settle{CallID: "call-1", Outcome: &out},
	); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedCommandIDIsPartOfDigest(t *testing.T) {
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

func TestAgentRevisionIsPinned(t *testing.T) {
	// A change to this value changes the revision of every recorded agent
	// that relied on the derived one; make it deliberately.
	agent := Agent{
		Provider:     "anthropic",
		API:          "anthropic.messages",
		Model:        "claude-sonnet-5",
		Instructions: "Be brief.",
	}.resolved()
	if got := agentRevision(agent); got != "auto:9abf96dec04e55a6" {
		t.Fatalf("agentRevision = %q", got)
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
