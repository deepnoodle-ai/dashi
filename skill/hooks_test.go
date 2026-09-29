package skill_test

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestBundleReminderIsContextualCatalog(t *testing.T) {
	bundle, err := skill.New(
		[]skill.Definition{{Name: "review", Instructions: "Body."}},
		skill.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := bundle.Reminder()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Tier != llm.ReminderContextual || r.Content != bundle.Catalog() {
		t.Fatalf("reminder = %+v", r)
	}
	empty, _ := skill.New(nil, skill.Options{})
	if got := empty.Reminder(); got != (llm.Reminder{}) {
		t.Fatalf("empty reminder = %+v", got)
	}
	if h := empty.Hooks(); len(h.BeforeModel) != 0 {
		t.Fatal("empty bundle returned hooks")
	}
}

func TestBundleHooksDeliverCatalogOnceAsRuntimeContext(t *testing.T) {
	bundle, err := skill.New(
		[]skill.Definition{{
			Name:         "review",
			Description:  "Review code.",
			Instructions: "Body.",
		}},
		skill.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &hookClient{}
	runner := dashi.Runner{
		Agent:    dashi.Agent{Model: "fixture"},
		Client:   client,
		Tools:    tool.Join(tool.List{}, bundle),
		Recorder: dashi.NewMemoryRecorder(),
		Hooks:    bundle.Hooks(),
	}
	outcome, err := runner.Run(
		t.Context(),
		dashi.NewTurn("skills-hooks"),
		dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || outcome.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("run: stop=%+v err=%v", outcome.Stop, err)
	}
	if len(client.inputs) != 2 {
		t.Fatalf("model calls = %d", len(client.inputs))
	}
	for i, input := range client.inputs {
		if got := countCatalog(input, bundle.Catalog()); got != 1 {
			t.Errorf("call %d saw the catalog %d times", i, got)
		}
	}
	delivered := 0
	for _, step := range outcome.Turn.Steps {
		if step.Context == nil {
			continue
		}
		for _, item := range step.Context.Items {
			if item.Name == "skills" {
				delivered++
				if item.Origin != dashi.ContextRuntime ||
					!strings.HasSuffix(item.ID, bundle.Digest()) {
					t.Errorf("item = %+v", item)
				}
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("catalog recorded %d times", delivered)
	}
}

func countCatalog(input llm.Input, catalog string) int {
	n := 0
	for _, m := range input.Messages {
		for _, c := range m.Content {
			if c.Reminder != nil && c.Reminder.Content == catalog &&
				c.Reminder.Tier == llm.ReminderContextual {
				n++
			}
		}
	}
	return n
}

type hookClient struct {
	inputs []llm.Input
	sends  int
}

func (*hookClient) Provider() string { return "test" }
func (*hookClient) API() string      { return "test.chat" }
func (*hookClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c *hookClient) Prepare(
	_ context.Context,
	input llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, input)
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    input.Model,
			Scope:    "fixture",
		},
		Input:     input,
		Effective: input.Settings,
		Body:      []byte("{}"),
	})
}

func (c *hookClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.sends++
	requested := llm.ModelRef{Provider: "test", Model: "fixture"}
	if c.sends == 1 {
		call := tool.Call{
			ID:   "load-call",
			Name: "load_skill",
			Args: []byte(`{"name":"review"}`),
		}
		return llm.Result{
			Message: llm.AssistantMessage(
				llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
			),
			Stop:      llm.Stop{Kind: llm.StopToolUse},
			Requested: requested,
		}, nil
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("done")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: requested,
	}, nil
}
