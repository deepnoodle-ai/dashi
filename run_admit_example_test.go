package dashi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func ExampleRunner_Admit() {
	ctx := context.Background()
	runner := dashi.Runner{Recorder: dashi.NewMemoryRecorder()}
	start := dashi.Say("Check order 42")
	start.CommandID = "start-42"
	start.Agent = dashi.Agent{
		Provider: dashitest.Provider, API: dashitest.API, Model: dashitest.Model,
	}
	out, err := runner.Admit(ctx, dashi.NewTurn("order-42"), start, dashi.WithOwner("worker"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("saved: %s, next: %s, model calls: %d\n", out.Stop.Kind, out.Next(), out.ModelCalls)
	out, err = runner.Run(ctx, out.Turn, dashi.Cancel{
		CommandID: "cancel-42", Reason: "user canceled queued work",
	}, dashi.WithOwner("worker"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("canceled: %s, ended: %t\n", out.Stop.Kind, out.Stop.Ended)
	// Output:
	// saved: ready, next: continue, model calls: 0
	// canceled: canceled, ended: true
}

func ExampleStart_context() {
	ctx := context.Background()
	store := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("Order 42 is ready."))
	start := dashi.Say("Check order 42")
	start.CommandID, start.Agent.Model = "start-42", dashitest.Model
	start.Context = []dashi.ContextItem{{
		ID: "checkout", Name: "checkout", Origin: dashi.ContextRuntime, Snapshot: true,
		Message: llm.ReminderMessage(llm.Reminder{
			Name: "checkout", Content: "Use /workspace/orders", Tier: llm.ReminderContextual,
		}),
	}}
	runner := dashi.Runner{Client: client, Recorder: store}
	out, err := runner.Admit(ctx, dashi.NewTurn("order-42"), start)
	if err != nil {
		panic(err)
	}
	// JSON stands for application persistence; a fresh Runner resumes it.
	raw, _ := json.Marshal(out.Turn)
	var loaded dashi.Turn
	if err := json.Unmarshal(raw, &loaded); err != nil {
		panic(err)
	}
	out, err = runner.Run(ctx, loaded, dashi.Continue{CommandID: "continue-42"})
	if err != nil {
		panic(err)
	}
	// Replaying the original Start acknowledges it without another request.
	if _, err := (dashi.Runner{Recorder: store}).Admit(
		ctx, dashi.NewTurn("order-42"), start,
	); err != nil {
		panic(err)
	}
	var reminders int
	for _, message := range client.Inputs()[0].Messages {
		if message.Context != nil && message.Context.Name == "checkout" {
			reminders++
		}
	}
	fmt.Printf(
		"reminders: %d, requests: %d, resumes: %d\n",
		reminders, len(client.Inputs()), out.Resumes,
	)
	// Output:
	// reminders: 1, requests: 1, resumes: 0
}

type failureExampleClient struct {
	*dashitest.Client
	status llm.SendStatus
}

func (c failureExampleClient) Send(context.Context, llm.Call) (llm.Result, error) {
	return llm.Result{}, &llm.Error{
		Category: llm.ErrorRateLimited, SendStatus: c.status, Status: 429,
		Retryable: true, RetryAfter: 30 * time.Second,
	}
}

func ExampleFailure() {
	ctx := context.Background()
	client := failureExampleClient{
		Client: dashitest.NewClient(), status: llm.SendStatusProviderRejected,
	}
	start := dashi.Say("Check order 42")
	start.Agent.Model = dashitest.Model
	out, err := (dashi.Runner{Client: client}).Run(ctx, dashi.Turn{}, start)
	if err != nil {
		panic(err)
	}
	raw, _ := json.Marshal(out.Turn)
	var loaded dashi.Turn
	if err := json.Unmarshal(raw, &loaded); err != nil {
		panic(err)
	}
	view, err := loaded.View()
	if err != nil {
		panic(err)
	}
	failure := view.Failure
	safe := failure.SendStatus == llm.SendStatusNotSent ||
		failure.SendStatus == llm.SendStatusProviderRejected
	// The application saves this absolute due time with its retry policy.
	due := failure.RecordedAt.Add(failure.RetryAfter)
	fmt.Println(view.Next(), failure.Category, safe && failure.Retryable)
	fmt.Println("original delay:", due.Sub(failure.RecordedAt))
	// Output:
	// fix rate_limited true
	// original delay: 30s
}

func ExampleFailure_unknownSend() {
	ctx := context.Background()
	client := failureExampleClient{
		Client: dashitest.NewClient(), status: llm.SendStatusUnknown,
	}
	start := dashi.Say("Check order 42")
	start.Agent.Model = dashitest.Model
	runner := dashi.Runner{Client: client}
	out, err := runner.Run(ctx, dashi.Turn{}, start)
	if err != nil {
		panic(err)
	}
	_, err = runner.Run(ctx, out.Turn, dashi.Continue{CommandID: "unsafe"})
	fmt.Printf("hint: %t, send: %s, continue blocked: %t\n",
		out.Failure.Retryable, out.Failure.SendStatus, errors.Is(err, dashi.ErrNotReady))
	// Output:
	// hint: true, send: unknown, continue blocked: true
}
