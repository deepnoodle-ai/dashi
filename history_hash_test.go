package dashi

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestHistoryHashTreatsNoHistoryConsistently(t *testing.T) {
	withoutHistory, err := historyHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyHistory, err := historyHash([]llm.Message{})
	if err != nil {
		t.Fatal(err)
	}
	if withoutHistory != emptyHistory {
		t.Fatalf(
			"nil and empty history differ: %q, %q",
			withoutHistory,
			emptyHistory,
		)
	}
	const want = "v2:sha256:4ab94c2dfa357a765e52d52e4f941b1026a936d05ff1ed73d6131a19d755327b"
	if withoutHistory != want {
		t.Fatalf("empty history hash = %q, want %q", withoutHistory, want)
	}
}

func TestHistoryHashTracksMessageFields(t *testing.T) {
	first := llm.UserMessage(llm.Text("earlier"))
	history := []llm.Message{first}
	base, err := historyHash(history)
	if err != nil {
		t.Fatal(err)
	}
	const want = "v2:sha256:160ff8c56674801daacaaa1ac2da75233ffd696107738894116b0cdaa17363a9"
	if base != want {
		t.Fatalf("single-message hash = %q, want %q", base, want)
	}
	copyHash, err := historyHash([]llm.Message{first.Clone()})
	if err != nil {
		t.Fatal(err)
	}
	if base != copyHash {
		t.Fatal("equivalent messages have different hash")
	}

	changedContent := first.Clone()
	changedContent.Content = []llm.Content{llm.Text("different")}
	changedRole := first.Clone()
	changedRole.Role = llm.RoleAssistant
	changedSource := first.Clone()
	changedSource.Source = &llm.MessageSource{API: "responses"}
	for _, message := range []llm.Message{
		changedContent,
		changedRole,
		changedSource,
	} {
		got, err := historyHash([]llm.Message{message})
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Fatalf("changed message kept hash %q", got)
		}
	}
}

func TestTurnMessagesUsesCallHistoryWithoutSavingIt(t *testing.T) {
	history := llm.UserMessage(llm.Text("earlier"))
	input := llm.UserMessage(llm.Text("current"))
	hash, err := historyHash([]llm.Message{history})
	if err != nil {
		t.Fatal(err)
	}
	turn := Turn{
		ID: "turn-1",
		Steps: []Step{
			{
				Version: 1,
				TurnID:  "turn-1",
				Seq:     1,
				Kind:    StepTurnStarted,
				Started: &StartFact{
					Agent:       testAgent(),
					HistoryHash: hash,
					Input:       []llm.Message{input},
				},
			},
		},
	}

	projected, err := TurnMessagesFull(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || projected[0].Text() != "current" {
		t.Fatalf("turn messages included prior history: %+v", projected)
	}
	withHistory, err := turnMessagesWithHistory(turn, []llm.Message{history})
	if err != nil {
		t.Fatal(err)
	}
	if len(withHistory) != 2 {
		t.Fatalf("model request has %d messages, want 2", len(withHistory))
	}
	if withHistory[0].Text() != "earlier" || withHistory[1].Text() != "current" {
		t.Fatalf("model request order changed: %+v", withHistory)
	}
	withHistory[0].Content[0] = llm.Text("changed")
	if history.Text() != "earlier" {
		t.Fatal("messages shared caller history content")
	}
}

func TestReplaySurfacesContinuationWarning(t *testing.T) {
	turn := Turn{
		ID: "turn-1",
		Steps: []Step{
			{
				Version: 1,
				TurnID:  "turn-1",
				Seq:     1,
				Kind:    StepTurnStarted,
				Started: &StartFact{
					Agent: testAgent(),
					Input: []llm.Message{llm.UserMessage(llm.Text("current"))},
				},
			},
			{
				Version: 1,
				TurnID:  "turn-1",
				Seq:     2,
				Kind:    StepTurnContinued,
				Warnings: []Warning{
					{
						Code:    WarningHistoryMismatch,
						Message: "prior history differs from turn start",
					},
				},
			},
		},
	}
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningHistoryMismatch {
		t.Fatalf("continuation warning was not replayed: %+v", view.Warnings)
	}

	turn.Steps[1].Warnings[0].Code = "unknown"
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("unknown warning replayed: %v", err)
	}
	turn.Steps[1].Warnings[0].Code = WarningHistoryMismatch
	turn.Steps[1].Warnings[0].Message = ""
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("empty warning message replayed: %v", err)
	}
	turn.Steps[1].Warnings[0].Message = "prior history differs from turn start"
	turn.Steps[1].Kind = StepMessagesAdded
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("warning outside continuation replayed: %v", err)
	}
}
