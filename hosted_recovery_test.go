package kernel

import (
	"context"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestIncompleteHostedApprovalIsNotActionable(t *testing.T) {
	origin := content.Origin{Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture"}
	request := llm.HostedApprovalRequest{ID: "approval-1", Name: "remote", Origin: origin}
	incomplete := llm.Result{
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Message:   llm.AssistantMessage(llm.Text("partial"), llm.Content{Kind: llm.ContentHostedApprovalRequest, HostedApprovalRequest: &request}),
		Stop:      llm.Stop{Kind: llm.StopIncomplete},
	}
	client := &scriptedClient{results: []llm.Result{incomplete, final("recovered")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopFailed || first.Stop.Reason != "untrusted stop with hosted approval" || len(first.Pending) != 0 {
		t.Fatalf("first: stop=%+v pending=%v err=%v", first.Stop, first.Pending, err)
	}
	response := llm.HostedApprovalResponse{RequestID: request.ID, Approve: true, Origin: origin}
	if _, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Accept{ID: "approve", CallID: request.ID, HostedApproval: &response}, Bindings{Client: client, Recorder: rec}); err == nil {
		t.Fatal("incomplete hosted approval was accepted")
	}
	second, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Continue{ID: "resume"}, Bindings{Client: client, Recorder: rec})
	if err != nil || second.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf("second: stop=%+v inputs=%d err=%v", second.Stop, len(client.inputs), err)
	}
	for _, message := range client.inputs[1].Messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest {
				t.Fatal("incomplete hosted approval reached the next model request")
			}
		}
	}
}

func TestCanceledHostedApprovalDoesNotLeaveDanglingProjection(t *testing.T) {
	origin := content.Origin{Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture"}
	request := llm.HostedApprovalRequest{ID: "approval-1", Name: "remote", Origin: origin}
	result := llm.Result{Requested: llm.ModelRef{Provider: "test", Model: "fixture"}, Message: llm.AssistantMessage(llm.Text("before approval"), llm.Content{Kind: llm.ContentHostedApprovalRequest, HostedApprovalRequest: &request}), Stop: llm.Stop{Kind: llm.StopToolUse}}
	client := &scriptedClient{results: []llm.Result{result}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	canceled, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Cancel{ID: "cancel"}, Bindings{Client: client, Recorder: rec})
	if err != nil || canceled.Stop.Kind != StopCanceled || len(canceled.Pending) != 0 {
		t.Fatalf("canceled=%+v err=%v", canceled, err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	keptText := false
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest {
				t.Fatal("canceled approval reached projection")
			}
			if part.Kind == llm.ContentBlock && part.Block != nil && part.Block.Text != nil && part.Block.Text.Text == "before approval" {
				keptText = true
			}
		}
	}
	if !keptText {
		t.Fatal("non-approval content was lost")
	}
}
