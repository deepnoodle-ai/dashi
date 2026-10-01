package compaction

import (
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestLastUsageReadsTheLastModelCall(t *testing.T) {
	step := func(in int, r *llm.Result) dashi.Step {
		if r != nil {
			r.Usage = &llm.Usage{InputTokens: in}
		}
		return dashi.Step{
			Kind:  dashi.StepModelCallFinished,
			Model: &dashi.ModelFact{Result: r},
		}
	}
	turn := dashi.Turn{Steps: []dashi.Step{
		{Kind: dashi.StepTurnStarted},
		step(100, &llm.Result{}),
		step(250, &llm.Result{}),
		step(0, nil),
	}}
	if got := LastUsage(turn).InputTokens; got != 250 {
		t.Fatalf("LastUsage = %d, want the last reported call (250)", got)
	}
	if got := LastUsage(dashi.Turn{}); got.InputTokens != 0 {
		t.Fatalf("empty turn usage = %+v", got)
	}
}
