// These examples exercise existing Dashi APIs with a scripted model and no keys.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/feed"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func savedStart() dashi.Start {
	return dashi.Start{
		CommandID: "request-42",
		Agent:     dashi.Agent{Provider: dashitest.Provider, API: dashitest.API, Model: dashitest.Model},
		Input:     []llm.Message{llm.UserMessage(llm.Text("Check order 42."))},
	}
}

func paused(runner dashi.Runner) dashi.RunResult {
	pause := make(chan struct{})
	close(pause)
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "order-42"}, savedStart(), dashi.WithPause(pause))
	if err != nil {
		panic(err)
	}
	return out
}

func Example_saveBeforeCancel() {
	runner := dashi.Runner{} // Deliberately no model client.
	out := paused(runner)
	fmt.Printf("saved: %s, next: %s, model calls: %d\n", out.Stop.Kind, out.Next(), out.ModelCalls)
	out, err := runner.Run(context.Background(), out.Turn, dashi.Cancel{CommandID: "cancel", Reason: "user canceled queued work"})
	if err != nil {
		panic(err)
	}
	fmt.Printf("canceled: %s, ended: %t\n", out.Stop.Kind, out.Stop.Ended)
	// Output:
	// saved: interrupted, next: continue, model calls: 0
	// canceled: canceled, ended: true
}

func Example_restartAndRetry() {
	ctx := context.Background()
	store := dashi.NewMemoryRecorder()
	out := paused(dashi.Runner{Recorder: store})
	before := len(out.Turn.Steps)
	// A fresh Runner has no old in-process state. The same saved command is a retry.
	runner := dashi.Runner{Recorder: store, Client: dashitest.NewClient(dashitest.Reply("Order 42 is ready."))}
	retry, err := runner.Run(ctx, dashi.Turn{ID: out.Turn.ID}, savedStart())
	if err != nil {
		panic(err)
	}
	fmt.Println("retry added records:", len(retry.Turn.Steps)-before)
	// JSON stands for persisted rows. The store remains an in-memory fixture.
	raw, err := json.Marshal(retry.Turn)
	if err != nil {
		panic(err)
	}
	var loaded dashi.Turn
	if err = json.Unmarshal(raw, &loaded); err != nil {
		panic(err)
	}
	out, err = runner.Run(ctx, loaded, dashi.Continue{CommandID: "continue-42", OnHistoryMismatch: dashi.HistoryMismatchReject})
	if err != nil {
		panic(err)
	}
	fmt.Println("reply:", out.Reply)
	fmt.Println("model calls:", out.ModelCalls)
	// Output:
	// retry added records: 0
	// reply: Order 42 is ready.
	// model calls: 1
}

func Example_brokenHistory() {
	good := paused(dashi.Runner{}).Turn
	broken := dashi.Turn{ID: "broken", Steps: []dashi.Step{{Version: 1, TurnID: "broken", Seq: 2, Kind: dashi.StepTurnStarted}}}
	history, warnings := dashi.BuildHistory([]dashi.Turn{good, broken})
	fmt.Println("history still contains messages:", len(history) > 0)
	fmt.Println("warning:", warnings[0].Code)
	// Swarm's current strict wrapper refuses this result.
	fmt.Println("Swarm accepts history:", len(warnings) == 0)
	// Output:
	// history still contains messages: true
	// warning: turn_unreadable
	// Swarm accepts history: false
}

func Example_workerStatus() {
	runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("done"))}
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "live-42"}, savedStart())
	if err != nil {
		panic(err)
	}
	prefix := out.Turn
	prefix.Steps = prefix.Steps[:2] // The instant after the model-call start, before its finish.
	view, err := prefix.View()
	if err != nil {
		panic(err)
	}
	fmt.Printf("saved view: %s, inferred: %t\n", view.Stop.Kind, view.Stop.Inferred)
	// Same mapping as Swarm's event(turn, live): process status comes from the app.
	status := string(view.Stop.Kind)
	workerIsAlive := true
	if workerIsAlive && view.Stop.Inferred && view.Stop.Kind == dashi.StopInterrupted {
		status = "running"
	}
	fmt.Println("UI with worker status:", status)
	// Output:
	// saved view: interrupted, inferred: true
	// UI with worker status: running
}

func Example_finishedCostCorrection() {
	result := dashitest.Reply("Order 42 is ready.")
	// No final usage arrived, so this is a finished call with a nonfinal charge.
	result.Cost = &llm.Cost{Currency: "USD", Source: llm.CostSourceProviderEstimate, Total: money.MustParseAmount("0.04")}
	runner := dashi.Runner{Client: dashitest.NewClient(result)}
	start := savedStart()
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "billing-42"}, start)
	if err != nil {
		panic(err)
	}
	result.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 20, RequestsComplete: true}
	result.Cost.Source = llm.CostSourceProviderReported
	result.Cost.Total = money.MustParseAmount("0.03")
	_, err = runner.Run(context.Background(), out.Turn, dashi.Resolve{
		CommandID: "billing-correction", CallID: out.ModelCallCosts[0].ModelCallID, ModelResult: &result,
	})
	fmt.Println("call finished:", out.ModelCallCosts[0].Finished)
	fmt.Println("charge final:", out.ModelCallCosts[0].Exact)
	fmt.Println("Resolve accepts correction:", err == nil)
	fmt.Println("recorded USD:", out.Spend.Cost["USD"])
	// Output:
	// call finished: true
	// charge final: false
	// Resolve accepts correction: false
	// recorded USD: 0.04
}

func Example_pendingResult() {
	pending := dashi.Pending{ID: "call-42", Kind: dashi.PendingTool}
	outcome := tool.Succeeded("42 units")
	before := dashi.Resolve{CommandID: "result:" + pending.ID, CallID: pending.ID, Outcome: &outcome}
	after := pending.Result(outcome) // Existing API; no proposed feature required.
	fmt.Println("same command:", reflect.DeepEqual(before, after))
	fmt.Println("command ID:", after.CommandID)
	// Output:
	// same command: true
	// command ID: result:call-42
}

func Example_correctionRounding() {
	// Swarm's ledger rounds each complete call amount up to micro-USD.
	micros := func(nanos int64) int64 { return (nanos + 999) / 1000 }
	oldNanos, newNanos := int64(1001), int64(2001)
	fmt.Println("difference of rounded totals:", micros(newNanos)-micros(oldNanos))
	fmt.Println("rounded difference:", micros(newNanos-oldNanos))
	// Use a smaller update to expose the unequal rounding paths.
	newNanos = 1999
	fmt.Println("smaller update, rounded totals:", micros(newNanos)-micros(oldNanos))
	fmt.Println("smaller update, rounded difference:", micros(newNanos-oldNanos))
	// Output:
	// difference of rounded totals: 1
	// rounded difference: 1
	// smaller update, rounded totals: 0
	// smaller update, rounded difference: 1
}

func Example_readerDisconnect() {
	workerCtx, stopWorker := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWorker()
	requestCtx, stopReader := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopReader()
	release := make(chan struct{})
	client := dashitest.NewClient(dashitest.Reply("Finished after the reader left."))
	client.Hold(release)
	store := dashi.NewMemoryRecorder()
	var hub feed.Hub
	runner := dashi.Runner{Client: client, Recorder: store, Observer: hub.Observe}
	finished := make(chan dashi.RunResult, 1)
	failed := make(chan error, 1)
	go func() {
		out, err := runner.Run(workerCtx, dashi.Turn{ID: "reader-42"}, savedStart())
		if err != nil {
			failed <- err
			return
		}
		finished <- out
	}()
	for event, err := range feed.Follow(requestCtx, &hub, store, "reader-42", 0) {
		if err != nil {
			panic(err)
		}
		if event.Step != nil && event.Step.Kind == dashi.StepModelCallStarted {
			break
		}
	}
	stopReader()
	fmt.Println("reader cancellation ended worker context:", workerCtx.Err() != nil)
	close(release)
	select {
	case err := <-failed:
		panic(err)
	case out := <-finished:
		fmt.Println("worker result:", out.Reply)
	case <-workerCtx.Done():
		panic(workerCtx.Err())
	}
	// Output:
	// reader cancellation ended worker context: false
	// worker result: Finished after the reader left.
}

func Example_lateResultAfterCancel() {
	ctx := context.Background()
	store := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.CallTool("call-late", "remote", "{}"))
	runner := dashi.Runner{Client: client, Recorder: store, Tools: tool.List{{
		Definition: tool.Definition{Name: "remote", Description: "Run elsewhere.", Kind: tool.KindExternal, Schema: json.RawMessage(`{"type":"object"}`)},
	}}}
	out, err := runner.Run(ctx, dashi.Turn{ID: "late-42"}, savedStart())
	if err != nil {
		panic(err)
	}
	canceled, err := runner.Run(ctx, out.Turn, dashi.Cancel{CommandID: "cancel-42"})
	if err != nil {
		panic(err)
	}
	outcome := tool.Succeeded("Finished late.")
	late, err := runner.Run(ctx, canceled.Turn, dashi.Resolve{CommandID: "late-result", CallID: "call-late", Outcome: &outcome})
	if err != nil {
		panic(err)
	}
	var cursor int64
	for step, err := range dashi.Follow(ctx, store, "late-42", 0) {
		if err != nil {
			panic(err)
		}
		cursor = step.Seq
	}
	remaining, err := store.StepsAfter(ctx, "late-42", cursor, 10)
	if err != nil {
		panic(err)
	}
	fmt.Println("Turn remains:", late.Stop.Kind)
	fmt.Println("Follow stopped at cancel:", cursor == int64(len(canceled.Turn.Steps)))
	fmt.Println("later records through StepsAfter:", len(remaining))
	// Output:
	// Turn remains: canceled
	// Follow stopped at cancel: true
	// later records through StepsAfter: 1
}

// dashitest normally prepares a tiny {} body. Real encoded message sizes are
// needed here to exercise the library's context-size estimate.
type sizedScript struct{ *dashitest.Client }

func (c sizedScript) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.Client.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snapshot := req.Snapshot()
	snapshot.Body, err = json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	return llm.NewRequest(snapshot)
}

func Example_activeCompaction() {
	client := dashitest.NewClient(
		dashitest.CallTool("call-log", "read_log", "{}"),
		dashitest.Reply("<summary>The log confirms order 42 is ready.</summary>"),
		dashitest.Reply("Order 42 is ready."),
	)
	logTool := tool.MustFunc("read_log", "Read the order log.", func(_ context.Context, _ tool.Call, _ struct{}) (tool.Outcome, error) {
		return tool.Succeeded(strings.Repeat("Order 42 is ready. ", 800)), nil
	})
	start := savedStart()
	start.Agent.ActiveCompaction = &dashi.ActiveCompactionPolicy{Window: 4096, Fraction: 0.5, MaxTokens: 256}
	start.Agent.Settings.MaxTokens = 128
	runner := dashi.Runner{Client: sizedScript{client}, Tools: tool.List{logTool}}
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "summary-42"}, start)
	if err != nil {
		panic(err)
	}
	summaries := 0
	for _, call := range out.ModelCallCosts {
		if call.Purpose == dashi.ModelPurposeCompaction {
			summaries++
		}
	}
	fmt.Println("summary model calls:", summaries)
	fmt.Println("reply:", out.Reply)
	// Output:
	// summary model calls: 1
	// reply: Order 42 is ready.
}

// joiningStream sends one delta before the reader joins, then waits for the gap.
type joiningStream struct {
	*dashitest.Client
	release <-chan struct{}
}

func (c joiningStream) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	call.Sink(llm.Delta{Kind: llm.DeltaText, Text: "Partial"})
	select {
	case <-c.release:
	case <-ctx.Done():
		return llm.Result{}, ctx.Err()
	}
	call.Sink(llm.Delta{Kind: llm.DeltaText, Text: " reply"})
	return c.Client.Send(ctx, call)
}

func Example_previewGapThenReply() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, preview := make(chan struct{}), make(chan struct{})
	store := dashi.NewMemoryRecorder()
	var hub feed.Hub
	runner := dashi.Runner{
		Client:   joiningStream{dashitest.NewClient(dashitest.Reply("Complete saved reply.")), release},
		Recorder: store,
		Observer: func(event dashi.Event) {
			hub.Observe(event)
			if event.Delta != nil && event.Delta.Text == "Partial" {
				close(preview)
			}
		},
	}
	type completion struct {
		out dashi.RunResult
		err error
	}
	done := make(chan completion, 1)
	go func() {
		out, err := runner.Run(ctx, dashi.Turn{ID: "gap-42"}, savedStart())
		done <- completion{out, err}
	}()
	select {
	case <-preview: // Join after the first preview, before the model returns.
	case <-ctx.Done():
		panic(ctx.Err())
	}
	sawFinish := false
	savedReply := ""
	for event, err := range feed.Follow(ctx, &hub, store, "gap-42", 0) {
		if err != nil {
			panic(err)
		}
		if event.Gap {
			fmt.Println("preview gap: clear partial text and keep following")
			close(release)
			continue
		}
		if event.Step != nil && event.Step.Kind == dashi.StepModelCallFinished {
			sawFinish = true
			if event.Step.Model != nil && event.Step.Model.Result != nil {
				savedReply = event.Step.Model.Result.Message.Text()
			}
		}
	}
	result := <-done
	if result.err != nil {
		panic(result.err)
	}
	fmt.Println("received saved finish:", sawFinish)
	fmt.Println("reply:", savedReply)
	// Output:
	// preview gap: clear partial text and keep following
	// received saved finish: true
	// reply: Complete saved reply.
}

// oneSteer is a sequential fixture; a durable host supplies its own store.
type oneSteer struct {
	pending []dashi.Steer
	acked   int
}

func (s *oneSteer) Steers(context.Context, string) ([]dashi.Steer, error) {
	return s.pending, nil
}

func (s *oneSteer) Delivered(_ context.Context, _ string, ids []string) error {
	if len(s.pending) != 0 && len(ids) == 1 && ids[0] == s.pending[0].Item.ID {
		s.acked++
		s.pending = nil
	}
	return nil
}

func Example_savedInputBeforeDispatch() {
	ctx := context.Background()
	client := dashitest.NewClient(dashitest.Reply("Used the new branch."))
	source := &oneSteer{pending: []dashi.Steer{{Item: dashi.ContextItem{
		ID: "input-42", Message: llm.UserMessage(llm.Text("Use the new branch")),
	}}}}
	deny := dashi.SendHook{Name: "budget", Func: func(context.Context, dashi.SendPlan) (dashi.SendDecision, error) {
		return dashi.SendDecision{Reason: "waiting for funds", Limit: &dashi.LimitDetail{
			Kind: dashi.LimitHost, Code: "shared_budget",
		}}, nil
	}}
	runner := dashi.Runner{Client: client, Hooks: deny.Hooks()}
	out, err := runner.Run(ctx, dashi.Turn{ID: "steer-42"}, savedStart(), dashi.WithSteering(source))
	if err != nil {
		panic(err)
	}
	fmt.Println("saved input acknowledged:", source.acked == 1)
	fmt.Println("model dispatches before funding:", len(client.Inputs()))
	fmt.Println("stop:", out.Stop.Kind)
	// Remove the preflight denial and use a fresh command after funding.
	runner.Hooks = dashi.Hooks{}
	_, err = runner.Run(ctx, out.Turn, dashi.Continue{CommandID: "funded-42"}, dashi.WithSteering(source))
	if err != nil {
		panic(err)
	}
	input, _ := json.Marshal(client.Inputs()[0].Messages)
	fmt.Println("funded call includes saved input:", strings.Contains(string(input), "Use the new branch"))
	// Output:
	// saved input acknowledged: true
	// model dispatches before funding: 0
	// stop: limited
	// funded call includes saved input: true
}

func Example_calculatedAndPostedCost() {
	first := dashitest.CallTool("tiny-tool", "read", "{}")
	last := dashitest.Reply("done")
	for _, result := range []*llm.Result{&first, &last} {
		result.Cost = &llm.Cost{Currency: "USD", Source: llm.CostSourceProviderReported, Total: money.Amount(1)}
	}
	read := tool.MustFunc("read", "Read one item.", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
		return tool.Succeeded("item"), nil
	})
	runner := dashi.Runner{Client: dashitest.NewClient(first, last), Tools: tool.List{read}}
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "tiny-cost"}, savedStart())
	if err != nil {
		panic(err)
	}
	var postedMicros int64
	for _, call := range out.ModelCallCosts {
		nanos := call.Cost.Total.Nanos()
		postedMicros += nanos / 1000
		if nanos%1000 != 0 {
			postedMicros++
		}
	}
	fmt.Println("model cost in nanos:", out.Spend.Cost["USD"].Nanos())
	fmt.Println("account postings in micros:", postedMicros)
	// Output:
	// model cost in nanos: 2
	// account postings in micros: 2
}
