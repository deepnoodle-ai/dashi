package dashi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestRecordedSummaryPreferenceSurvivesReopenAndModelSwitch(t *testing.T) {
	agentOf := func(turn dashi.Turn) dashi.Agent {
		view, err := dashi.Replay(turn)
		if err != nil {
			t.Fatal(err)
		}
		return view.Agent
	}
	dir := t.TempDir()
	recorder, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	public := llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: "public"}, Replay: &llm.Opaque{Version: 1, Type: "reasoning", Unit: llm.OpaqueUnitContentBlock, Origin: content.Origin{Provider: dashitest.Provider, API: dashitest.API, Model: dashitest.Model, Scope: "dashitest"}, Raw: json.RawMessage(`{"private":"CIPHER"}`)}}
	reply := dashitest.Reply("answer")
	reply.Message.Content = append([]llm.Content{public}, reply.Message.Content...)
	client := dashitest.NewClient(reply, dashitest.Reply("next"), dashitest.Reply("third"))
	session := &dashi.Session{ID: "summary", New: true, Runner: dashi.Runner{Client: client, Recorder: recorder}, Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{ReasoningSummary: new(true)}}}
	first, err := session.Send(t.Context(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if first.Answer != "answer" {
		t.Fatalf("public summary reached answer: %q stop=%+v turn=%+v", first.Answer, first.Stop, first.Turn)
	}
	reopenedRecorder, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	reopened := &dashi.Session{ID: "summary", Runner: dashi.Runner{Client: client, Recorder: reopenedRecorder}}
	second, err := reopened.Send(t.Context(), "again")
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(second.Turn).Settings.ReasoningSummary; pref == nil || !*pref {
		t.Fatal("reopened turn did not inherit true")
	}
	reopened.Agent = dashi.Agent{Model: "another-model", Settings: llm.Settings{ReasoningSummary: new(false)}}
	third, err := reopened.Send(t.Context(), "stop requesting")
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(third.Turn).Settings.ReasoningSummary; pref == nil || *pref || agentOf(third.Turn).Model != "another-model" {
		t.Fatal("false/model switch lost")
	}
	loaded, err := reopenedRecorder.Load(t.Context(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(loaded).Settings.ReasoningSummary; pref == nil || !*pref {
		t.Fatal("new off preference changed prior turn")
	}
	if !strings.Contains(string(public.Replay.Raw), "CIPHER") {
		t.Fatal("private artifact changed")
	}
	inputs := client.Inputs()
	if len(inputs) != 3 || inputs[2].Settings.ReasoningSummary == nil || *inputs[2].Settings.ReasoningSummary {
		t.Fatal("false not sent to next call")
	}
	found := false
	for _, message := range inputs[2].Messages {
		for _, part := range message.Content {
			if part.Reasoning != nil && part.Reasoning.Text == "public" && part.Replay != nil && strings.Contains(string(part.Replay.Raw), "CIPHER") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("summary-off stripped earlier display/replay")
	}
	for _, turn := range []dashi.Turn{first.Turn, second.Turn, third.Turn} {
		foundFact := false
		for _, step := range turn.Steps {
			if step.Kind == dashi.StepModelIntent && step.Model != nil {
				pref := step.Model.Effective.ReasoningSummary
				if pref == nil || *pref != *agentOf(turn).Settings.ReasoningSummary {
					t.Fatal("recorded model fact preference differs")
				}
				foundFact = true
			}
		}
		if !foundFact {
			t.Fatal("missing recorded model facts")
		}
	}
}
