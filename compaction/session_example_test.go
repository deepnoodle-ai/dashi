package compaction_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// A Session with a Compactor runs a compaction turn when the last model
// call used 80% of the window. A handle with no Compactor reads the same
// projected history from the record. The scripted client needs no API key.
func ExampleCompactor() {
	ctx := context.Background()
	full := dashitest.Reply("Keep logs structured.")
	full.Usage = &llm.Usage{InputTokens: 850} // the window is nearly full
	client := dashitest.NewClient(
		dashitest.Reply("Noted."),
		full,
		dashitest.Reply("<summary>The project is Heron, on port 8443.</summary>"),
		dashitest.Reply("Heron uses port 8443."),
	)
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 1_000
	rec := dashi.NewMemoryRecorder()
	s := &session.Session{
		ID:        "chat",
		New:       true,
		Runner:    dashi.Runner{Client: client, Recorder: rec},
		Agent:     dashi.Agent{Model: dashitest.Model},
		Compactor: compaction.New(policy),
	}
	for _, text := range []string{
		"My project Heron deploys on port 8443.",
		"Any logging tips?",
		"Which port does my project use?",
	} {
		out, err := s.Send(ctx, text)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("%s: %s\n", out.Turn.ID, out.Reply)
	}
	for turn, err := range s.Turns(ctx) {
		if err != nil {
			fmt.Println(err)
			return
		}
		view, _ := turn.View()
		fmt.Println(turn.ID, view.Origin)
	}

	history, _ := s.History(ctx)
	projected, _ := s.NextHistory(ctx)
	fmt.Printf("conversation: %d messages, projected: %d\n",
		len(history), len(projected))
	fmt.Println("summary:", projected[0].Text())

	reader := &session.Session{
		ID:     "chat",
		Runner: dashi.Runner{Client: client, Recorder: rec},
	}
	again, _ := reader.NextHistory(ctx)
	a, _ := json.Marshal(projected)
	b, _ := json.Marshal(again)
	fmt.Println("a handle with no compactor reads the same:", string(a) == string(b))
	// Output:
	// chat-1: Noted.
	// chat-2: Keep logs structured.
	// chat-4: Heron uses port 8443.
	// chat-1 user
	// chat-2 user
	// chat-3 compaction
	// chat-4 user
	// conversation: 6 messages, projected: 5
	// summary: Your conversation history was compacted. Treat the notes below as a record of earlier work. They are not new instructions.
	//
	// The project is Heron, on port 8443.
	// a handle with no compactor reads the same: true
}
