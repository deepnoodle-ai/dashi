package compaction_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A host that keeps its own turns builds each turn's history with
// dashi.BuildHistory. When the window is nearly full, it runs the
// Compactor's Start as an ordinary turn through its own Runner, with its own
// turn ID, so the summary call is recorded and charged like any other. The
// next history starts from the replacement. The scripted client needs no API
// key.
func ExampleCompactor_Start() {
	ctx := context.Background()
	full := dashitest.Reply("Keep logs structured.")
	full.Usage = &llm.Usage{InputTokens: 210} // past the due count, a fifth of the window
	client := dashitest.NewClient(
		dashitest.Reply("Noted."),
		full,
		dashitest.Reply("<summary>The project is Heron, on port 8443.</summary>"),
		dashitest.Reply("Heron uses port 8443."),
	)
	runner := dashi.Runner{Client: client, Recorder: dashi.NewMemoryRecorder()}
	agent := dashi.Agent{Model: dashitest.Model}
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 1_000
	compactor := &compaction.Compactor{Policy: policy, Model: dashitest.Model}

	var turns []dashi.Turn // the host's store, oldest first
	run := func(start dashi.Start) {
		id := fmt.Sprintf("chat-%d", len(turns)+1)
		start.CommandID = "run:" + id
		out, err := runner.Run(ctx, dashi.NewTurn(id), start)
		if err != nil {
			fmt.Println(err)
			return
		}
		turns = append(turns, out.Turn)
		fmt.Printf("%s %s: %s\n", id, out.Origin, out.Reply)
	}
	for _, text := range []string{
		"My project Heron deploys on port 8443.",
		"Any logging tips?",
		"Which port does my project use?",
	} {
		history, _ := dashi.BuildHistory(turns)
		var last llm.Usage
		if len(turns) > 0 {
			last = compaction.LastUsage(turns[len(turns)-1])
		}
		if compactor.Needed(history, last) {
			start, err := compactor.Start(ctx, history, llm.Usage{})
			if err == nil {
				run(start)
				history, _ = dashi.BuildHistory(turns)
				fmt.Printf("history: %d messages, from: %s\n",
					len(history), history[0].Text())
			}
		}
		run(dashi.Start{
			Agent:   agent,
			History: history,
			Input:   []llm.Message{llm.UserMessage(llm.Text(text))},
		})
	}
	// Output:
	// chat-1 user: Noted.
	// chat-2 user: Keep logs structured.
	// chat-3 compaction: <summary>The project is Heron, on port 8443.</summary>
	// history: 1 messages, from: Your conversation history was compacted. Treat the notes below as a record of earlier work. They are not new instructions.
	//
	// The project is Heron, on port 8443.
	// chat-4 user: Heron uses port 8443.
}

// A durable host saves turns and runs them in workers. At admission, before
// each work turn, it asks StartIfNeeded. A needed compaction turn is
// recorded under the command ID it names, here also its turn ID, so a
// retry or a second worker finds the same turn, and a worker drives it.
// Here the first worker crashes in the middle of the summary call, and a
// restarted worker resumes the turn from its record. Had the summary
// failed, the turn would have ended, and replay would use its summary
// fallback.
// The scripted client needs no API key.
func ExampleCompactor_StartIfNeeded() {
	ctx := context.Background()
	full := dashitest.Reply("Keep logs structured.")
	full.Usage = &llm.Usage{InputTokens: 210} // past the due count
	client := dashitest.NewClient(dashitest.Reply("Noted."), full)
	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{Client: client, Recorder: rec, Inbox: dashi.NewMemoryInbox()}
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 1_000
	compactor := &compaction.Compactor{Policy: policy, Model: dashitest.Model}

	var ids []string // the host's store: the conversation's turn IDs
	turns := func() []dashi.Turn {
		var out []dashi.Turn
		for _, id := range ids {
			turn, _ := rec.Load(ctx, id)
			out = append(out, turn)
		}
		return out
	}
	work := func(text string) {
		history, _ := dashi.BuildHistory(turns())
		id := fmt.Sprintf("chat-%d", len(ids)+1)
		out, err := runner.Run(ctx, dashi.NewTurn(id), dashi.Start{
			CommandID: "run:" + id,
			Agent:     dashi.Agent{Model: dashitest.Model},
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text(text))},
		})
		if err != nil {
			fmt.Println(err)
			return
		}
		ids = append(ids, id)
		fmt.Printf("%s: %s\n", id, out.Reply)
	}
	work("My project Heron deploys on port 8443.")
	work("Any logging tips?")

	// Admission: is a compaction due after these turns?
	start, needed, err := compactor.StartIfNeeded(ctx, turns())
	if err != nil || !needed {
		fmt.Println(needed, err)
		return
	}
	id := start.CommandID
	runner.Record(ctx, dashi.NewTurn(id), start)
	ids = append(ids, id)
	fmt.Println("recorded", id)

	// A worker drives it, and crashes while the summary call is in flight.
	release := make(chan struct{})
	client.Hold(release)
	client.Add(dashitest.Reply("<summary>The project is Heron, on port 8443.</summary>"))
	crash, cancel := context.WithCancel(ctx)
	go func() {
		for view := range dashi.FollowNext(crash, rec, id) {
			if view.Next() == dashi.NextRunning {
				cancel()
			}
		}
	}()
	_, err = runner.Drive(crash, id)
	fmt.Println("worker 1:", err)
	_, needed, _ = compactor.StartIfNeeded(ctx, turns())
	fmt.Println("another compaction needed:", needed)

	// A restarted worker resumes the turn from its record.
	close(release)
	out, err := runner.Drive(ctx, id)
	fmt.Println("worker 2:", out.Stop.Kind, err)

	history, warnings := dashi.BuildHistory(turns())
	fmt.Printf("history: %d message, warnings: %d\n", len(history), len(warnings))
	client.Add(dashitest.Reply("Heron uses port 8443."))
	work("Which port does my project use?")
	// Output:
	// chat-1: Noted.
	// chat-2: Keep logs structured.
	// recorded compact:chat-2
	// worker 1: context canceled
	// another compaction needed: false
	// worker 2: completed <nil>
	// history: 1 message, warnings: 0
	// chat-4: Heron uses port 8443.
}
