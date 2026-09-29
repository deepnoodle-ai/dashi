package dashi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestContextIdentityDoesNotChangeHistoryFingerprint(t *testing.T) {
	message := llm.UserMessage(llm.Text("seed"))
	before, err := historyFingerprint([]llm.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	message.Context = &llm.MessageContext{Origin: "runtime", Name: "seed"}
	after, err := historyFingerprint([]llm.Message{message})
	if err != nil || before != after {
		t.Fatalf("fingerprints differ: %s %s %v", before, after, err)
	}
	message.Content = []llm.Content{llm.Text("changed")}
	changed, err := historyFingerprint([]llm.Message{message})
	if err != nil || changed == before {
		t.Fatalf("value change lost: %s %v", changed, err)
	}
}

func TestIdentifiedMessageValidation(t *testing.T) {
	for _, tc := range []struct {
		name, origin, slot string
		message            llm.Message
		errorText          string
	}{
		{"user", "user", "seed", llm.UserMessage(llm.Text("seed")), ""},
		{"operator", "operator", "seed", llm.Message{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("seed")}}, ""},
		{"empty origin", "", "seed", llm.UserMessage(llm.Text("seed")), "requires origin and name"},
		{"empty name", "user", "", llm.UserMessage(llm.Text("seed")), "requires origin and name"},
		{"unknown origin", "alien", "seed", llm.UserMessage(llm.Text("seed")), "invalid context origin"},
		{"role", "runtime", "seed", llm.AssistantMessage(llm.Text("seed")), "untrusted context must use user role"},
		{"agent sender", "agent", "seed", llm.UserMessage(llm.Text("seed")), "requires one contextual reminder with a sender"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := tc.message.Clone()
			message.Context = &llm.MessageContext{Origin: tc.origin, Name: tc.slot}
			err := validateMessage(message)
			if tc.errorText == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("err=%v want=%s", err, tc.errorText)
			}
		})
	}
}

func TestSeedContextIDCollisionAndHookViewOwnership(t *testing.T) {
	view := TurnView{ID: "turn", Revision: 5, usedContextIDs: map[string]bool{"seed": true, "seed:turn:5": true, "seed:turn:5:1": true}}
	copy := snapshotView(view)
	copy.usedContextIDs["seed:turn:5:2"] = true
	if view.usedContextIDs["seed:turn:5:2"] {
		t.Fatal("hook view shares ID membership")
	}
	seed := SeedContext(ContextItem{ID: "seed", Name: "seed", Message: llm.UserMessage(llm.Text("value"))})
	d, err := seed.Func(t.Context(), ModelPlan{Turn: view})
	if err != nil || len(d.Deliver) != 1 || d.Deliver[0].ID != "seed:turn:5:2" {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	again, err := seed.Func(t.Context(), ModelPlan{Turn: view})
	if err != nil || again.Deliver[0].ID != d.Deliver[0].ID {
		t.Fatalf("unstable decision=%+v err=%v", again, err)
	}
}

func TestProjectContextOwnsMetadataAndPreservesRecord(t *testing.T) {
	// Minimal legacy record contains no projected metadata. Project restores the
	// slot from recorded ContextItem fields and overrides embedded metadata.
	turn := NewTurn("legacy")
	turn.Steps = []Step{
		{Version: 1, TurnID: turn.ID, Seq: 1, Kind: StepStarted, Started: &StartFact{Agent: Agent{Revision: "test", Provider: "test", API: "test.chat", Model: "test"}, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}},
		{Version: 1, TurnID: turn.ID, Seq: 2, Kind: StepDelivered, Context: &ContextFact{Items: []ContextItem{{Name: "seed", Message: llm.UserMessage(llm.Text("seed"))}}}},
	}
	data, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	var restored Turn
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	messages, err := Project(restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Context == nil || messages[1].Context.Origin != "user" || messages[1].Context.Name != "seed" {
		t.Fatalf("messages=%+v", messages)
	}
	messages[1].Context.Name = "changed"
	if restored.Steps[1].Context.Items[0].Message.Context != nil {
		t.Fatal("projection changed record")
	}
	again, err := Project(restored)
	if err != nil || again[1].Context.Name != "seed" {
		t.Fatalf("projection aliases metadata %v", err)
	}
}

func TestContextPointerOwnershipAcrossInputAndCompactionClones(t *testing.T) {
	message := llm.UserMessage(llm.Text("seed"))
	message.Context = &llm.MessageContext{Origin: "runtime", Name: "seed"}
	input := cloneInput(llm.Input{Messages: []llm.Message{message}})
	input.Messages[0].Context.Name = "input mutation"
	fact := (&CompactionFact{Keep: []llm.Message{message}}).clone()
	fact.Keep[0].Context.Name = "compaction mutation"
	if message.Context.Name != "seed" {
		t.Fatal("context metadata aliases cloned input or compaction")
	}
}

func TestStrictContinueAcceptsDescriptiveIdentityOnExistingHistory(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("prior seed"))}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	rec := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, rec, history)
	history[0].Context = &llm.MessageContext{Origin: "runtime", Name: "seed"}
	out, err := (Runner{Client: client, Recorder: rec}).Run(t.Context(), first.Turn, Continue{History: history, OnHistoryMismatch: HistoryMismatchReject})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	for _, warning := range out.Warnings {
		if warning.Code == WarningHistoryMismatch {
			t.Fatal("metadata changed continuation history identity")
		}
	}
}
