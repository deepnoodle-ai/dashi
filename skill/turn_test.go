package skill_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

// This is a host migration boundary, not a package policy: a second tool in
// one model response is authorized before the first load is settled, even with
// serial execution. Mobius's focus checkpoint and ordering cannot be replaced
// by looking only for settled load calls in BeforeTool.
func TestSameBatchAuthorizationPrecedesLoadSettlement(t *testing.T) {
	bundle, err := skill.New(
		[]skill.Definition{{
			Name:         "Incident Review",
			Instructions: "Investigate before acting.",
		}},
		skill.Options{ToolName: "invoke_skill"},
	)
	if err != nil {
		t.Fatal(err)
	}
	action, err := tool.Func(
		"incident_lookup",
		"Look up an incident.",
		func(
			context.Context,
			tool.Call,
			struct{},
		) (tool.Outcome, error) {
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	sawAction := false
	loadSettledAtAuthorization := false
	hook := dashi.ToolHook{
		Name: "observe-same-batch",
		Func: func(
			_ context.Context,
			plan dashi.ToolPlan,
		) (dashi.ToolDecision, error) {
			if plan.Call.Name != "incident_lookup" {
				return dashi.ToolDecision{}, nil
			}
			sawAction = true
			for _, call := range plan.Turn.Tools {
				if call.Call.Name == "invoke_skill" &&
					call.Status == dashi.ToolFinished {
					loadSettledAtAuthorization = true
				}
			}
			return dashi.ToolDecision{}, nil
		},
	}
	client := &skillClient{}
	revision := "incident-agent-v1:" + bundle.Digest() + ":focus-v1"
	runner := dashi.Runner{
		Client:   client,
		Tools:    tool.Join(tool.List{action}, bundle),
		Recorder: dashi.NewMemoryRecorder(),
		Hooks:    hook.Hooks(),
	}
	start := dashi.Start{
		CommandID: "start",
		Agent: dashi.Agent{
			Model:    "fixture",
			Revision: revision,
		},
		Input: []llm.Message{llm.UserMessage(llm.Text("review incident"))},
	}
	outcome, err := runner.Run(t.Context(), dashi.NewTurn("skills-same-batch"), start)
	if err != nil || outcome.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("run: stop=%+v err=%v", outcome.Stop, err)
	}
	if !sawAction || loadSettledAtAuthorization {
		t.Fatalf(
			"action hook saw load settled=%v, saw action=%v",
			loadSettledAtAuthorization,
			sawAction,
		)
	}
	view, err := dashi.Replay(outcome.Turn)
	if err != nil {
		t.Fatal(err)
	}
	loaded := view.Tools["load-call"]
	if loaded.Status != dashi.ToolFinished ||
		loaded.Outcome == nil ||
		loaded.Outcome.State != tool.StateSucceeded ||
		!strings.Contains(
			loaded.Result().Output.Content[0].Text.Text,
			"Investigate before acting.",
		) {
		t.Fatalf("recorded skill load = %+v", loaded)
	}

	// The bundle digest stayed the same; the host must change its explicit
	// revision when focus selectors or their meaning change, and a retried
	// Start under the new revision no longer matches the recorded agent.
	changed := start
	changed.Agent.Revision = "incident-agent-v1:" + bundle.Digest() + ":focus-v2"
	_, err = runner.Run(t.Context(), outcome.Turn, changed)
	if !errors.Is(err, dashi.ErrAgentChanged) {
		t.Fatalf("changed host policy revision: %v", err)
	}
	changedBundle, err := skill.New(
		[]skill.Definition{{
			Name:         "Incident Review",
			Instructions: "Different instructions.",
		}},
		skill.Options{ToolName: "invoke_skill"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Continue runs with the recorded agent, which does not cover the tool
	// bundle, so a changed bundle is not reported as a changed agent.
	runner.Tools = tool.Join(tool.List{action}, changedBundle)
	_, err = runner.Run(
		t.Context(),
		outcome.Turn,
		dashi.Continue{CommandID: "changed-bundle"},
	)
	if errors.Is(err, dashi.ErrAgentChanged) {
		t.Fatalf("recorded agent unexpectedly verified changed tool bundle: %v", err)
	}
}

type skillClient struct{ sends int }

func (*skillClient) Provider() string { return "test" }
func (*skillClient) API() string      { return "test.chat" }
func (*skillClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (*skillClient) Prepare(
	_ context.Context,
	input llm.Input,
) (llm.Request, error) {
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

func (c *skillClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.sends++
	requested := llm.ModelRef{Provider: "test", Model: "fixture"}
	if c.sends == 1 {
		load := tool.Call{
			ID:   "load-call",
			Name: "invoke_skill",
			Args: []byte(`{"name":"Incident Review"}`),
		}
		action := tool.Call{
			ID:   "action-call",
			Name: "incident_lookup",
			Args: []byte(`{}`),
		}
		return llm.Result{
			Message: llm.AssistantMessage(
				llm.Content{Kind: llm.ContentToolCall, ToolCall: &load},
				llm.Content{Kind: llm.ContentToolCall, ToolCall: &action},
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
