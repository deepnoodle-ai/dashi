package dashi

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A late result recorded after the turn ended is not part of the turn's
// citations or provider tool uses, as it is not part of its reply.
func TestTurnCitationsStopAtTheEndedStop(t *testing.T) {
	turn := Turn{ID: "t1", Steps: []Step{
		finishedStep(llm.Result{
			Message: llm.AssistantMessage(textPart("A", content.Citation{URL: "https://a"})),
			Stop:    llm.Stop{Kind: llm.StopFinished},
		}),
		{Kind: StepTurnStopped, Stop: &Stop{Kind: StopCanceled, Ended: true}},
		finishedStep(llm.Result{
			Message: llm.AssistantMessage(
				providerToolUsePart("late"),
				textPart("B", content.Citation{URL: "https://b"}),
			),
			Stop: llm.Stop{Kind: llm.StopFinished},
		}),
	}}
	if got := turn.Citations(); len(got) != 1 || got[0].URL != "https://a" {
		t.Fatalf("citations=%+v", got)
	}
	if got := turn.ProviderToolUses(); len(got) != 0 {
		t.Fatalf("provider tool uses=%+v", got)
	}
}
