// Command compaction shows a session that compacts its history. After three
// questions the context window is nearly full, so before the fourth the
// session runs a compaction turn: the model summarizes the older messages,
// and later turns send the summary in their place. A second handle with no
// compactor then builds the same history from the record. It
// needs no API key: a scripted dashitest client stands in for the model,
// so the replies are fixed. With a real client, the session code stays the
// same.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

// used is a scripted reply whose model call used tokens of context.
func used(text string, tokens int) llm.Result {
	r := dashitest.Reply(text)
	r.Usage = &llm.Usage{InputTokens: tokens}
	return r
}

func run(ctx context.Context) error {
	// The script is what a model would answer. The third reply reports a
	// context of 26,000 tokens, over 80% of the 32,000-token window below.
	client := dashitest.NewClient(
		used("Noted: Heron deploys on port 8443.", 9_000),
		used("Log in JSON, one event per line, with a request ID.", 17_000),
		used("Review small changes, and read the tests first.", 26_000),
		used("<summary>The user's project is Heron; it deploys on port 8443. "+
			"They asked for tips on logs and code review.</summary>", 12_000),
		used("Heron, on port 8443.", 3_000),
	)
	// A real host takes the model's window from client.ModelInfo; the
	// scripted client knows none.
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 32_000
	rec := dashi.NewMemoryRecorder()
	s := &session.Session{
		ID:     "heron",
		New:    true,
		Runner: dashi.Runner{Client: client, Recorder: rec},
		Agent:  dashi.Agent{Model: dashitest.Model},
		// By default the newest turns that fit in a quarter of the window
		// stay verbatim. This example keeps none, so the summary alone
		// stands for the earlier turns.
		Compactor: &compaction.Compactor{Policy: policy, TailFraction: -1},
	}

	for _, text := range []string{
		"My project is called Heron and it deploys on port 8443.",
		"Any tips for logs?",
		"And for code review?",
		"What is my project called, and which port does it use?",
	} {
		fmt.Println("> " + text)
		out, err := s.Send(ctx, text)
		if err != nil {
			return err
		}
		for _, w := range out.SessionWarnings {
			fmt.Printf("  (%s: %s)\n", w.Code, w.Message)
		}
		fmt.Printf("  %s: %s\n", out.Turn.ID, out.Reply)
	}

	// The compaction turn is in the record, beside the conversation.
	fmt.Println()
	for turn, err := range s.Turns(ctx) {
		if err != nil {
			return err
		}
		view, err := turn.View()
		if err != nil {
			return err
		}
		fmt.Printf("%s  %-10s  %d tokens used\n",
			turn.ID, view.Origin, view.Spend.Usage.TotalInputTokens())
	}
	history, err := s.History(ctx)
	if err != nil {
		return err
	}
	messages, err := s.NextHistory(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nconversation: %d messages; the next turn sends %d:\n",
		len(history), len(messages))
	for _, m := range messages {
		fmt.Printf("  [%s] %.60s\n", m.Role, m.Text())
	}

	// A viewer or a recovery worker needs no compactor to send the same.
	reader := &session.Session{
		ID:     "heron",
		Runner: dashi.Runner{Client: client, Recorder: rec},
	}
	again, err := reader.NextHistory(ctx)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(messages)
	b, _ := json.Marshal(again)
	fmt.Println("a handle with no compactor reads the same:", string(a) == string(b))
	return nil
}
