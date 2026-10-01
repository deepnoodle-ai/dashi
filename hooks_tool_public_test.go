package dashi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// TestAfterToolResultSurvivesAFileRecorder stores a turn whose result a hook
// replaced, loads it back, and checks the model's view survived.
func TestAfterToolResultSurvivesAFileRecorder(t *testing.T) {
	rec, err := filerecorder.New(filerecorder.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	write := tool.Tool{
		Definition: tool.Definition{
			Name:   "write",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: executor(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("key=sk-123"), nil
		}),
	}
	redact := dashi.ResultHook{Name: "redact", Func: func(
		_ context.Context,
		p dashi.ResultPlan,
	) (dashi.ResultDecision, error) {
		r := p.Result
		r.Output = r.Output.MapText(func(s string) string {
			return strings.ReplaceAll(s, "sk-123", "[redacted]")
		})
		return dashi.ResultDecision{Replace: &r}, nil
	}}
	_, err = (dashi.Runner{
		Client:   &approvalClient{},
		Tools:    tool.List{write},
		Recorder: rec,
		Hooks:    redact.Hooks(),
	}).Run(t.Context(), dashi.NewTurn("t1"), dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("write it"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := loaded.View()
	if err != nil {
		t.Fatal(err)
	}
	got := view.Tools["call-1"].Result().Output.Content[0].Text.Text
	if got != "key=[redacted]" {
		t.Fatalf("model result after reload = %q", got)
	}
}

type executor func(context.Context, tool.Call) (tool.Outcome, error)

func (f executor) Execute(ctx context.Context, c tool.Call) (tool.Outcome, error) {
	return f(ctx, c)
}
