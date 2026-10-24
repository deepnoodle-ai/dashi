// Command compaction shows a session that compacts its history. A model's
// context window is the most text it can read at once. When the history
// reaches a share of the window, the session asks the model to summarize the
// older messages, and later turns send the summary in their place. It needs
// no API key: a scripted test client plays the model, so the replies are
// fixed.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

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
	// The script is what a model would answer. Each reply reports the
	// context its call used: the history, plus about 500 tokens of
	// instructions. The third reaches 1,600 tokens, the due count: a fifth
	// (compaction.DefaultFraction) of the 8,000-token window below.
	client := dashitest.NewClient(
		used("Noted: Heron deploys on port 8443.", 1_100),
		used("Log in JSON, one event per line, with a request ID.", 1_250),
		used("Review small changes, and read the tests first.", 1_650),
		used("<summary>The user's project is Heron; it deploys on port 8443. "+
			"They asked for tips on logs and code review.</summary>", 1_300),
		used("Heron, on port 8443.", 650),
	)
	// A real program gets the window size from client.ModelInfo; the
	// scripted client has none, so set it here.
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 8_000
	s := &session.Session{
		ID:     "heron",
		New:    true,
		Runner: dashi.Runner{Client: client, Recorder: dashi.NewMemoryRecorder()},
		Agent:  dashi.Agent{Model: dashitest.Model},
		// By default the newest turns are kept word for word. A negative
		// TailFraction keeps none, so the summary replaces all older turns.
		// With an empty Model, the session's model writes the summary. A
		// Session needs no Compactor.Client; that field is for hosts that
		// call StartIfNeeded themselves.
		Compactor: &compaction.Compactor{Policy: policy, TailFraction: -1},
	}

	notes := "My project is called Heron. My notes: " +
		strings.Repeat("Heron deploys on port 8443. ", 140)
	for _, text := range []string{
		notes,
		"Any tips for logs?",
		"And for code review?",
		"What is my project called, and which port does it use?",
	} {
		fmt.Printf("> %.70s\n", text)
		out, err := s.Send(ctx, text)
		if err != nil {
			return err
		}
		fmt.Printf("  %s: %s\n", out.Turn.ID, out.Reply)
	}

	// The summary was written by a turn of its own, listed with the rest.
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
	// History is the whole conversation; NextHistory is what the next turn
	// would send the model, with the summary in place of older messages.
	fmt.Printf("\nconversation: %d messages; the next turn sends %d:\n",
		len(history), len(messages))
	for _, m := range messages {
		fmt.Printf("  [%s] %.60s\n", m.Role, m.Text())
	}
	return nil
}
