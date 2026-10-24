package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
)

// summaryFailedRecord shows the stop of turn 1 as a failed in-turn summary,
// which leaves the turn open. A fixture cannot easily make a summary fail.
type summaryFailedRecord struct {
	*filerecorder.Recorder
}

func (r summaryFailedRecord) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := r.Recorder.Load(ctx, id)
	if err != nil || !strings.HasSuffix(id, "-1") {
		return turn, err
	}
	// Only the first stop; the stop that closes the turn stays as recorded.
	for i := range turn.Steps {
		if step := &turn.Steps[i]; step.Kind == dashi.StepTurnStopped {
			step.Stop = &dashi.Stop{Kind: dashi.StopFailed, Reason: dashi.StopReasonCompactionFailed}
			break
		}
	}
	return turn, nil
}

// A new request on a session whose latest turn needs compaction closes that
// turn first. Its reply came from an earlier run and is not output.
func TestAutomationNewRequestOmitsClosedTurnReply(t *testing.T) {
	cfg := testConfig(t, say("Old reply."), say("<summary>Earlier work.</summary>"), say("New answer."))
	if code, _, diag := runAutomationFixture(t, cfg, Options{Prompt: "first", Run: true}, ""); code != 0 {
		t.Fatal(diag)
	}
	saved, err := cfg.Store.Latest(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder { return summaryFailedRecord{r} }
	code, out, diag := runAutomationFixture(t, cfg, Options{Prompt: "new", Run: true, Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, "")
	if code != 0 || !reflect.DeepEqual(decodeResponses(t, "json", out), []responseMessage{{"New answer."}}) {
		t.Fatalf("code=%d out=%s diag=%s", code, out, diag)
	}
}
