package session_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

func priorGenerationSettings() llm.Settings {
	return llm.Settings{MaxTokens: 1234, Temperature: new(0.7), Reasoning: &llm.ReasoningSettings{Effort: "high"}, ReasoningSummary: new(true), ParallelTools: new(true), Citations: true, PromptCache: llm.PromptCache{TTL: time.Hour, Key: "old-key"}}
}

func TestCompleteSettingsPriorityAndDispatchedCallStart(t *testing.T) {
	old := priorGenerationSettings()
	for _, tc := range []struct {
		name           string
		session, start dashi.Agent
		want           llm.Settings
		marked         bool
	}{
		{name: "ordinary partial", session: dashi.Agent{Settings: llm.Settings{MaxTokens: 2000}}, start: dashi.Agent{Settings: llm.Settings{Temperature: new(0.0)}}, want: func() llm.Settings { v := old; v.MaxTokens = 2000; v.Temperature = new(0.0); return v }()},
		{name: "complete start", session: dashi.Agent{Settings: old}, start: dashi.Agent{NoInheritedSettings: true}, marked: true},
		{name: "complete session", session: dashi.Agent{NoInheritedSettings: true, Settings: llm.Settings{MaxTokens: 2000}}, start: dashi.Agent{Settings: llm.Settings{Temperature: new(0.0)}}, want: llm.Settings{MaxTokens: 2000, Temperature: new(0.0)}, marked: true},
		{name: "both complete", session: dashi.Agent{NoInheritedSettings: true, Settings: old}, start: dashi.Agent{NoInheritedSettings: true, Settings: llm.Settings{ReasoningSummary: new(false), ParallelTools: new(false)}}, want: llm.Settings{ReasoningSummary: new(false), ParallelTools: new(false)}, marked: true},
		{name: "empty complete session", session: dashi.Agent{NoInheritedSettings: true}, marked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := dashi.NewMemoryRecorder()
			client := dashitest.NewClient(dashitest.Reply("old"), dashitest.Reply("new"))
			s := handle(rec, client)
			s.New = true
			s.Agent.Settings = old
			s.Agent.Instructions = "keep instructions"
			s.Agent.Limits.ModelCalls = 3
			first, err := s.Send(t.Context(), "old controls")
			if err != nil {
				t.Fatal(err)
			}
			if first.Stop.Kind != dashi.StopCompleted || len(client.Inputs()) != 1 {
				t.Fatalf("first stop=%+v inputs=%d", first.Stop, len(client.Inputs()))
			}
			before, _ := json.Marshal(first.Turn)
			s.Agent = tc.session
			start := dashi.Say("resolved new controls")
			start.Agent = tc.start
			out, err := s.Run(t.Context(), start)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted || len(client.Inputs()) != 2 {
				t.Fatalf("next stop=%+v inputs=%d", out.Stop, len(client.Inputs()))
			}
			if !reflect.DeepEqual(out.Agent.Settings, tc.want) || !reflect.DeepEqual(client.Inputs()[1].Settings, tc.want) || out.Agent.NoInheritedSettings != tc.marked {
				t.Fatalf("agent=%+v dispatch=%+v want=%+v", out.Agent, client.Inputs()[1].Settings, tc.want)
			}
			if out.Agent.Instructions != "keep instructions" || out.Agent.Limits.ModelCalls != 3 {
				t.Fatalf("non-settings inheritance changed: %+v", out.Agent)
			}
			original := loadTurn(t, rec, first.Turn.ID)
			after, _ := json.Marshal(original)
			if !bytes.Equal(before, after) {
				t.Fatal("earlier facts changed")
			}
			if tc.marked && out.Agent.Version == first.Agent.Version {
				t.Fatal("complete settings did not change derived identity")
			}
			if tc.want.MaxTokens == 0 {
				adapter, err := responses.New(responses.Config{Provider: "fixture", APIKey: "fixture", Endpoint: "https://fixture.test/responses"})
				if err != nil {
					t.Fatal(err)
				}
				request, err := adapter.Prepare(t.Context(), llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}, Settings: out.Agent.Settings})
				if err != nil {
					t.Fatal(err)
				}
				if request.Snapshot().Effective.MaxTokens != llm.DefaultMaxTokens {
					t.Fatalf("default output allowance=%d", request.Snapshot().Effective.MaxTokens)
				}
			}
		})
	}
}

func TestCompleteSettingsSurvivePhysicalStorageAndFreshHandle(t *testing.T) {
	dir := t.TempDir()
	rec, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	client := dashitest.NewClient(dashitest.Reply("old"), dashitest.Reply("cleared"), dashitest.Reply("reopened"))
	s := handle(rec, client)
	s.New = true
	s.Agent.Settings = priorGenerationSettings()
	first, err := s.Send(t.Context(), "old")
	if err != nil {
		t.Fatal(err)
	}
	s.Agent = dashi.Agent{NoInheritedSettings: true}
	cleared, err := s.Send(t.Context(), "clear")
	if err != nil {
		t.Fatal(err)
	}
	reopenedRec, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	reopened := handle(reopenedRec, client)
	out, err := reopened.Send(t.Context(), "next")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Agent.NoInheritedSettings || !reflect.DeepEqual(out.Agent.Settings, llm.Settings{}) {
		t.Fatalf("carried agent=%+v", out.Agent)
	}
	recorded := load(t, reopenedRec, cleared.Turn.ID)
	if !recorded.Agent.NoInheritedSettings || !reflect.DeepEqual(recorded.Agent.Settings, llm.Settings{}) {
		t.Fatalf("stored boundary=%+v", recorded.Agent)
	}
	legacy := load(t, reopenedRec, first.Turn.ID)
	encoded, _ := json.Marshal(legacy.Agent)
	if legacy.Agent.NoInheritedSettings || bytes.Contains(encoded, []byte("no_inherited_settings")) {
		t.Fatal("legacy Agent JSON changed")
	}
}

func TestCompleteSettingsQueueAndCompaction(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			rec := dashi.NewMemoryRecorder()
			client := dashitest.NewClient(used("old", 100), dashitest.Reply("summary"), dashitest.Reply("queued"), dashitest.Reply("later"))
			s := handle(rec, client)
			s.New = true
			s.Agent.Settings = priorGenerationSettings()
			if _, err := s.Send(t.Context(), "old controls"); err != nil {
				t.Fatal(err)
			}
			s.Agent = dashi.Agent{NoInheritedSettings: true}
			s.Compactor = keepLast{limit: 100}
			if manual {
				if _, err := s.Compact(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Enqueue(t.Context(), "queued user"); err != nil {
				t.Fatal(err)
			}
			out, ran, err := s.RunQueue(t.Context())
			if err != nil || !ran {
				t.Fatalf("ran=%v error=%v", ran, err)
			}
			summary := load(t, rec, "s-2")
			if summary.Origin != dashi.StartCompaction || summary.Agent.NoInheritedSettings || !reflect.DeepEqual(summary.Agent.Settings, llm.Settings{}) {
				t.Fatalf("summary inherited user controls: %+v", summary.Agent)
			}
			if !out.Agent.NoInheritedSettings || !reflect.DeepEqual(client.Inputs()[2].Settings, llm.Settings{}) || out.Origin != dashi.StartUser {
				t.Fatalf("queued agent=%+v origin=%s input=%+v", out.Agent, out.Origin, client.Inputs()[2])
			}
			fresh := handle(rec, client)
			next, err := fresh.Send(t.Context(), "later")
			if err != nil {
				t.Fatal(err)
			}
			if !next.Agent.NoInheritedSettings || !reflect.DeepEqual(next.Agent.Settings, llm.Settings{}) {
				t.Fatalf("queue boundary did not carry: %+v", next.Agent)
			}
		})
	}
}

func TestCompleteSettingsDoNotChangeRecordedContinuation(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("continued"))
	s := handle(rec, client)
	s.New = true
	s.Agent.Settings = priorGenerationSettings()
	pause := make(chan struct{})
	close(pause)
	first, err := s.Send(t.Context(), "pause", dashi.WithPause(pause))
	if err != nil || first.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("pause=%+v error=%v", first.Stop, err)
	}
	s.Agent = dashi.Agent{NoInheritedSettings: true}
	out, err := s.Run(t.Context(), dashi.Continue{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Agent.Settings, first.Agent.Settings) || out.Agent.NoInheritedSettings || out.Agent.Version != first.Agent.Version || !reflect.DeepEqual(client.Inputs()[0].Settings, first.Agent.Settings) {
		t.Fatalf("continuation changed recorded agent: %+v", out.Agent)
	}
}

// A carried NoInheritedSettings marks the recorded set as complete; it does
// not stop later layers' partial settings from merging over that set.
func TestCompleteSettingsAllowLaterPartialSettings(t *testing.T) {
	client := dashitest.NewClient(dashitest.Reply("1"), dashitest.Reply("2"), dashitest.Reply("3"), dashitest.Reply("4"))
	s := handle(dashi.NewMemoryRecorder(), client)
	s.New = true
	s.Agent.Settings = priorGenerationSettings()
	steps := []struct {
		agent dashi.Agent
		want  llm.Settings
	}{
		{agent: dashi.Agent{NoInheritedSettings: true}, want: llm.Settings{}},
		{agent: dashi.Agent{Settings: llm.Settings{MaxTokens: 2000}}, want: llm.Settings{MaxTokens: 2000}},
		{agent: dashi.Agent{Settings: llm.Settings{Temperature: new(0.0)}}, want: llm.Settings{MaxTokens: 2000, Temperature: new(0.0)}},
	}
	if _, err := s.Send(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	for i, step := range steps {
		s.Agent = step.agent
		out, err := s.Send(t.Context(), "next")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out.Agent.Settings, step.want) || !reflect.DeepEqual(client.Inputs()[i+1].Settings, step.want) {
			t.Fatalf("turn %d settings=%+v, want %+v", i+2, out.Agent.Settings, step.want)
		}
	}
}
