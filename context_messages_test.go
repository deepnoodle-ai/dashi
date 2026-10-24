package dashi

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// A provider pause chain that asked for a provider tool approval on an
// earlier call stays protected, though its closing call asked for none.
func TestContextGroupsPauseChainKeepsApproval(t *testing.T) {
	request := llm.ProviderToolApprovalRequest{ID: "req-1", Name: "remote"}
	paused := &llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:                        llm.ContentProviderToolApprovalRequest,
			ProviderToolApprovalRequest: &request,
		}),
		Stop: llm.Stop{Kind: llm.StopPause},
	}
	closing := &llm.Result{Message: llm.AssistantMessage(llm.Text("done")), Stop: llm.Stop{Kind: llm.StopFinished}}
	turn := Turn{ID: "t", Steps: []Step{
		{Seq: 1, Kind: StepModelCallFinished, Model: &ModelFact{ModelCallID: "t:model:1", Result: paused}},
		{Seq: 2, Kind: StepModelCallFinished, Model: &ModelFact{ModelCallID: "t:model:2", Result: closing}},
	}}
	messages := []contextMessage{
		{Ref: TurnMessageRef{Seq: 1}, Message: paused.Message},
		{Ref: TurnMessageRef{Seq: 2}, Message: closing.Message},
	}
	groups := contextGroups(turn, messages)
	if len(groups) != 1 || len(groups[0].Messages) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Eligible {
		t.Fatal("pause chain with an approval request is eligible for compaction")
	}
}
