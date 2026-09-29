package compaction_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/llm"
)

// notes is a Summarizer that needs no model. A real host uses
// compaction.NewSummarizer.
type notes struct{}

func (notes) Summarize(
	context.Context,
	[]llm.Message,
) (compaction.Summary, error) {
	return compaction.Summary{Text: "The user asked about ports."}, nil
}

// A host stores its history, the index where each Turn starts, and the
// newest Record. Before each Turn it calls Project. After each Turn it calls
// Compact when the Policy says the window is nearly full.
func Example() {
	var history []llm.Message
	var starts []int
	for _, prompt := range []string{"first", "second", "third"} {
		starts = append(starts, len(history))
		history = append(history,
			llm.UserMessage(llm.Text(prompt+" "+strings.Repeat("x", 4_000))),
			llm.AssistantMessage(llm.Text("answer to "+prompt)),
		)
	}

	policy := compaction.Policy{} // the window defaults to 128,000 tokens
	opts := compaction.Options{
		Policy:       policy,
		TurnStarts:   starts,
		TailFraction: 0.01, // keep about 1,300 tokens of recent Turns
	}
	rec, err := compaction.Compact(
		context.Background(), notes{}, history, nil, opts,
	)
	if err != nil {
		fmt.Println("compaction failed:", err)
		return
	}
	view, _ := compaction.Project(history, rec)
	fmt.Printf("%d messages -> %d\n", len(history), len(view))

	// A stored Record that no longer matches the history returns the raw
	// history and an error. The host warns and goes on without the Record.
	history[0] = llm.UserMessage(llm.Text("edited"))
	view, err = compaction.Project(history, rec)
	if errors.Is(err, compaction.ErrCoverageMismatch) {
		fmt.Printf("mismatch: using %d raw messages\n", len(view))
	}
	// Output:
	// 6 messages -> 3
	// mismatch: using 6 raw messages
}
