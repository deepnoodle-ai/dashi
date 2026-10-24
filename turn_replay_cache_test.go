package dashi

import (
	"flag"
	"os"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// TestMain checks every cached rebuild against a full replay, so each test
// that runs a turn also shows the cache gives the same state. Benchmarks
// run without the check, which would double their replay work.
func TestMain(m *testing.M) {
	flag.Parse()
	checkReplayCache = flag.Lookup("test.bench").Value.String() == ""
	os.Exit(m.Run())
}

// TestReplayCacheMatchesFullReplay grows a turn one step at a time, as a
// run does, and compares each cached rebuild with a full replay.
func TestReplayCacheMatchesFullReplay(t *testing.T) {
	turn := runBenchToolTurn(t, 6)
	var c replayCache
	grown := Turn{ID: turn.ID, Steps: make([]Step, 0, len(turn.Steps))}
	for i, step := range turn.Steps {
		if i > 0 {
			extra, err := c.replayWith(grown, &step)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := replay(Turn{ID: turn.ID, Steps: turn.Steps[:i+1]})
			sameReplayState(extra, want, "replayWith differs")
		}
		grown.Steps = append(grown.Steps, step)
		got, err := c.replay(grown)
		if err != nil {
			t.Fatal(err)
		}
		want, err := replay(grown)
		if err != nil {
			t.Fatal(err)
		}
		sameReplayState(got, want, "cached replay differs")
	}
	if len(c.steps) != len(turn.Steps) {
		t.Fatalf("cache holds %d steps, want %d", len(c.steps), len(turn.Steps))
	}
}

// TestReplayCacheReplaysChangedTurn shows the cache replays in full when
// the turn is not the one it applied plus new steps.
func TestReplayCacheReplaysChangedTurn(t *testing.T) {
	turn := runBenchToolTurn(t, 3)
	var c replayCache
	if _, err := c.replay(turn); err != nil {
		t.Fatal(err)
	}
	short := Turn{ID: turn.ID, Steps: turn.Steps[:3:3]}
	got, err := c.replay(short)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := replay(short)
	sameReplayState(got, want, "shorter turn")
	// A copy with a broken step is caught, though it is as long.
	broken := turn.Clone()
	broken.Steps[1].Seq = 9
	if _, err := c.replay(broken); err == nil {
		t.Fatal("broken copy passed")
	}
	// A broken appended step leaves the cache usable.
	if _, err := c.replay(turn); err != nil {
		t.Fatal(err)
	}
	bad := Turn{ID: turn.ID, Steps: append(slices.Clip(turn.Steps), Step{Version: 1, TurnID: turn.ID, Seq: 1})}
	if _, err := c.replay(bad); err == nil {
		t.Fatal("broken step passed")
	}
	if got, err = c.replay(turn); err != nil {
		t.Fatal(err)
	}
	want, _ = replay(turn)
	sameReplayState(got, want, "after a broken step")
}

// TestReplayCacheCatchesRewrittenStep rewrites the last applied step in
// place, in the same backing array, and shows the cache replays in full.
func TestReplayCacheCatchesRewrittenStep(t *testing.T) {
	turn := runBenchToolTurn(t, 3)
	var c replayCache
	if _, err := c.replay(turn); err != nil {
		t.Fatal(err)
	}
	turn.Steps[len(turn.Steps)-1].Seq = 99
	if _, err := c.replay(turn); err == nil {
		t.Fatal("rewritten step passed")
	}
}

// TestReplayStateCloneKeepsProviderToolOrder reuses a resolved provider
// tool approval ID on a clone and shows the original's order is unchanged.
func TestReplayStateCloneKeepsProviderToolOrder(t *testing.T) {
	base := &replayState{
		TurnView:               TurnView{Tools: map[string]ToolView{}},
		providerTool:           map[string]llm.ProviderToolApprovalRequest{},
		providerToolModelCalls: map[string]string{},
		modelCallOfTool:        map[string]string{},
		providerToolOrder:      []string{"req-1", "req-2"},
	}
	cp := base.clone()
	request := llm.ProviderToolApprovalRequest{
		ID:     "req-1",
		Name:   "remote",
		Origin: content.Origin{Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture"},
	}
	result := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:                        llm.ContentProviderToolApprovalRequest,
			ProviderToolApprovalRequest: &request,
		}),
		Stop: llm.Stop{Kind: llm.StopFinished},
	}
	if err := cp.recordResult("call-2", &result, true); err != nil {
		t.Fatal(err)
	}
	if got := base.providerToolOrder; !slices.Equal(got, []string{"req-1", "req-2"}) {
		t.Fatalf("base order = %v", got)
	}
	if got := cp.providerToolOrder; !slices.Equal(got, []string{"req-2", "req-1"}) {
		t.Fatalf("clone order = %v", got)
	}
}
