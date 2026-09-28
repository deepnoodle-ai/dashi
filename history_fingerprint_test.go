package kernel

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestHistoryFingerprintTreatsNoHistoryConsistently(t *testing.T) {
	withoutHistory, err := historyFingerprint(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyHistory, err := historyFingerprint([]llm.Message{})
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
	const want = "v2:sha256:cebd6398674617d53df5b037c36f04f78241ec59ea669e892cf19ec1c5dbe736"
	if withoutHistory != want {
		t.Fatalf("empty history fingerprint = %q, want %q", withoutHistory, want)
	}
}

func TestHistoryFingerprintTracksMessageFields(t *testing.T) {
	first := llm.UserMessage(llm.Text("earlier"))
	history := []llm.Message{first}
	base, err := historyFingerprint(history)
	if err != nil {
		t.Fatal(err)
	}
	const want = "v2:sha256:5e8edb9a6ec6af53b6fa2c0e9263e6eb624d32018b122e60103b95db35ea6638"
	if base != want {
		t.Fatalf("single-message fingerprint = %q, want %q", base, want)
	}
	copyFingerprint, err := historyFingerprint([]llm.Message{first.Clone()})
	if err != nil {
		t.Fatal(err)
	}
	if base != copyFingerprint {
		t.Fatal("equivalent messages have different fingerprints")
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
		got, err := historyFingerprint([]llm.Message{message})
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Fatalf("changed message kept fingerprint %q", got)
		}
	}
}

func TestProjectUsesCallHistoryWithoutSavingIt(t *testing.T) {
	history := llm.UserMessage(llm.Text("earlier"))
	input := llm.UserMessage(llm.Text("current"))
	fingerprint, err := historyFingerprint([]llm.Message{history})
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
				Kind:    StepStarted,
				Started: &StartFact{
					Definition:         definition(),
					HistoryFingerprint: fingerprint,
					Input:              []llm.Message{input},
				},
			},
		},
	}

	projected, err := Project(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || projected[0].Text() != "current" {
		t.Fatalf("turn projection included prior history: %+v", projected)
	}
	withHistory, err := projectWithHistory(turn, []llm.Message{history})
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
		t.Fatal("projection shared caller history content")
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
				Kind:    StepStarted,
				Started: &StartFact{
					Definition: definition(),
					Input:      []llm.Message{llm.UserMessage(llm.Text("current"))},
				},
			},
			{
				Version: 1,
				TurnID:  "turn-1",
				Seq:     2,
				Kind:    StepContinued,
				Warnings: []Warning{
					{
						Code:    WarningHistoryMismatch,
						Message: "prior history differs from turn start",
					},
				},
			},
		},
	}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningHistoryMismatch {
		t.Fatalf("continuation warning was not replayed: %+v", view.Warnings)
	}

	turn.Steps[1].Warnings[0].Code = "unknown"
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("unknown warning replayed: %v", err)
	}
	turn.Steps[1].Warnings[0].Code = WarningHistoryMismatch
	turn.Steps[1].Warnings[0].Message = ""
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("empty warning message replayed: %v", err)
	}
	turn.Steps[1].Warnings[0].Message = "prior history differs from turn start"
	turn.Steps[1].Kind = StepDelivered
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("warning outside continuation replayed: %v", err)
	}
}

// Fingerprints written before v2 were pinned by earlier tests; turns carrying
// them must keep matching the same history.
func TestHistoryMatchesLegacyV1Fingerprints(t *testing.T) {
	cases := []struct {
		history []llm.Message
		v1      string
	}{
		{
			nil,
			"v1:sha256:45534b2b2de441698879afa92a091595118dbeddc6096be5d6fa63efb8f69287",
		},
		{
			[]llm.Message{llm.UserMessage(llm.Text("earlier"))},
			"v1:sha256:65f8814a69a0e497248526e5f00e400e6406a7695aefb960c3d73187978100cc",
		},
	}
	for _, c := range cases {
		current, err := historyFingerprint(c.history)
		if err != nil {
			t.Fatal(err)
		}
		if !historyMatches(c.v1, current, c.history) {
			t.Fatalf("v1 fingerprint %q no longer matches its history", c.v1)
		}
		other := []llm.Message{llm.UserMessage(llm.Text("other"))}
		otherCurrent, _ := historyFingerprint(other)
		if historyMatches(c.v1, otherCurrent, other) {
			t.Fatalf("v1 fingerprint %q matched different history", c.v1)
		}
	}
}

// A v1-started turn continued with the same history gets no mismatch warning.
func TestContinueAcceptsLegacyV1HistoryFingerprint(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("earlier"))}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	turn := Turn{
		ID: "t1",
		Steps: []Step{
			{
				Version:       1,
				TurnID:        "t1",
				Seq:           1,
				Kind:          StepStarted,
				CommandID:     "start",
				CommandDigest: "legacy",
				Started: &StartFact{
					Definition:         definition(),
					HistoryFingerprint: "v1:sha256:65f8814a69a0e497248526e5f00e400e6406a7695aefb960c3d73187978100cc",
					Input:              []llm.Message{llm.UserMessage(llm.Text("go"))},
				},
			},
		},
	}
	out, err := (Engine{}).Apply(
		t.Context(),
		turn,
		definition(),
		Continue{ID: "continue", History: history},
		Bindings{Client: client},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("continue=%+v err=%v", out.Stop, err)
	}
	for _, step := range out.Turn.Steps {
		if len(step.Warnings) != 0 {
			t.Fatalf("legacy fingerprint produced warnings: %+v", step.Warnings)
		}
	}
}
