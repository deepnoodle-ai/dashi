package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

type preparationFailureClient struct {
	scriptedClient
	failure error
}

func (c *preparationFailureClient) Prepare(
	ctx context.Context,
	in llm.Input,
) (llm.Request, error) {
	if c.failure != nil {
		return llm.Request{}, c.failure
	}
	return c.scriptedClient.Prepare(ctx, in)
}

func TestFailurePreparationPreservesSafeFactsWithoutModelCall(t *testing.T) {
	const secret = "PRIVATE-PROVIDER-CONTENT"
	client := &preparationFailureClient{
		scriptedClient: scriptedClient{results: []llm.Result{final("done")}},
		failure: &llm.Error{
			Provider: secret, Category: llm.ErrorUnsupported,
			Reason: llm.ReasonMediaRejected, Code: secret,
			Body: []byte(secret), Cause: errors.New(secret),
			SendStatus: llm.SendStatusUnknown,
		},
	}
	start := Say("work")
	start.CommandID, start.Agent = "start", testAgent()
	out, err := (Runner{Client: client}).Run(t.Context(), Turn{ID: "prepare"}, start)
	if err != nil || out.Next() != NextFix || out.Failure == nil {
		t.Fatalf("preparation did not expose failure: %+v err=%v", out.TurnView, err)
	}
	f := out.Failure
	if f.Category != llm.ErrorUnsupported || f.Reason != llm.ReasonMediaRejected ||
		f.SendStatus != llm.SendStatusNotSent {
		t.Fatalf("preparation failure facts wrong: %+v", f)
	}
	if out.ModelCalls != 0 || len(out.ModelCallCosts) != 0 || client.sends != 0 {
		t.Fatal("preparation fabricated dispatch or spend")
	}
	if !f.RecordedAt.Equal(out.Turn.Steps[1].At) {
		t.Fatal("failure time differs from original recorded step")
	}
	raw, err := json.Marshal(out.Turn)
	if err != nil || strings.Contains(string(raw), secret) {
		t.Fatalf("provider secrets were recorded: %v", err)
	}
	client.failure = nil
	out, err = (Runner{Client: client}).Run(
		t.Context(), out.Turn, Continue{CommandID: "continue"},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Failure != nil {
		t.Fatalf("successful continuation retained failure: %+v err=%v", out.TurnView, err)
	}
}

func TestFailureRejectionTimingSurvivesJSONReload(t *testing.T) {
	const secret = "PRIVATE-PROVIDER-CONTENT"
	client := &scriptedClient{
		results: []llm.Result{{}, final("done")},
		errors: []error{&llm.Error{
			Provider: secret, Category: llm.ErrorRateLimited,
			SendStatus: llm.SendStatusProviderRejected, Status: 429,
			Retryable: true, RetryAfter: 30 * time.Second,
			Code: secret, Body: []byte(secret), Cause: errors.New(secret),
		}},
	}
	out, err := start(t, client, nil, nil, Hooks{})
	if err != nil || out.Failure == nil {
		t.Fatalf("send failure details absent: %v", err)
	}
	original := *out.Failure
	raw, err := json.Marshal(out.Turn)
	if err != nil || strings.Contains(string(raw), secret) {
		t.Fatalf("provider data escaped into record: %v", err)
	}
	var loaded Turn
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	view, err := loaded.View()
	if err != nil || view.Failure == nil || *view.Failure != original {
		t.Fatalf("reload lost failure facts: %+v err=%v", view.Failure, err)
	}
	if !view.Failure.RecordedAt.Add(view.Failure.RetryAfter).Equal(
		original.RecordedAt.Add(30 * time.Second),
	) {
		t.Fatal("reload restarted provider delay")
	}
	if view.Failure.Status != 429 || !view.Failure.Retryable ||
		view.Failure.SendStatus != llm.SendStatusProviderRejected {
		t.Fatalf("rejection facts wrong: %+v", view.Failure)
	}
	view.Failure.RetryAfter = time.Hour
	fresh, err := loaded.View()
	if err != nil || fresh.Failure.RetryAfter != 30*time.Second {
		t.Fatal("view mutation changed saved failure")
	}
	old := loaded.Clone()
	for i := range old.Steps {
		if old.Steps[i].Model != nil {
			old.Steps[i].Model.Failure = nil
		}
	}
	legacy, err := old.View()
	if err != nil || legacy.Failure != nil || legacy.Next() != fresh.Next() ||
		legacy.ReadyToContinue != fresh.ReadyToContinue {
		t.Fatalf("legacy failure changed execution meaning: %v", err)
	}
}

func TestFailureRetryHintNeverAuthorizesUnknownEffect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status llm.SendStatus
		result llm.Result
	}{
		{name: "unknown dispatch", status: llm.SendStatusUnknown},
		{name: "missing dispatch", status: ""},
		{
			name: "partial overrides rejection", status: llm.SendStatusProviderRejected,
			result: llm.Result{
				Message: llm.AssistantMessage(llm.Text("partial")),
				Usage:   &llm.Usage{InputTokens: 4},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{
				results: []llm.Result{tc.result},
				errors: []error{&llm.Error{
					Category: llm.ErrorTransport, SendStatus: tc.status,
					Retryable: true, RetryAfter: time.Second,
				}},
			}
			out, err := start(t, client, nil, nil, Hooks{})
			if err != nil || out.Stop.Kind != StopResultUnknown || out.Failure == nil {
				t.Fatalf("unknown failure missing: %+v err=%v", out.TurnView, err)
			}
			if !out.Failure.Retryable || out.Failure.SendStatus != llm.SendStatusUnknown {
				t.Fatalf("unknown evidence changed: %+v", out.Failure)
			}
			if _, err := (Runner{Client: client}).Run(
				t.Context(), out.Turn, Continue{CommandID: "unsafe"},
			); !errors.Is(err, ErrNotReady) || client.sends != 1 {
				t.Fatalf("retry hint authorized resend: %v", err)
			}
		})
	}
}

func TestFailureRejectsContradictorySavedMetadata(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors: []error{&llm.Error{
			Category: llm.ErrorAuthentication, SendStatus: llm.SendStatusNotSent,
		}},
	}
	out, err := start(t, client, nil, nil, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Failure)
	}{
		{name: "send contradiction", change: func(f *Failure) { f.SendStatus = llm.SendStatusUnknown }},
		{name: "arbitrary category", change: func(f *Failure) { f.Category = "secret" }},
		{name: "negative delay", change: func(f *Failure) { f.RetryAfter = -1 }},
		{
			name: "changed time",
			change: func(f *Failure) {
				f.RecordedAt = f.RecordedAt.Add(time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := out.Turn.Clone()
			for i := range changed.Steps {
				if m := changed.Steps[i].Model; m != nil && m.Failure != nil {
					tc.change(m.Failure)
				}
			}
			if _, err := changed.View(); !errors.Is(err, ErrInvalidTurn) {
				t.Fatalf("contradictory metadata accepted: %v", err)
			}
		})
	}
}

func TestFailurePreparationViewsDoNotShareSavedMetadata(t *testing.T) {
	client := &preparationFailureClient{failure: &llm.Error{
		Category: llm.ErrorRateLimited, RetryAfter: time.Second,
	}}
	start := Say("work")
	start.CommandID, start.Agent = "start", testAgent()
	out, err := (Runner{Client: client}).Run(t.Context(), Turn{ID: "isolated"}, start)
	if err != nil {
		t.Fatal(err)
	}
	for _, projection := range []string{"failure", "stop failure"} {
		t.Run(projection, func(t *testing.T) {
			view, err := out.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			failure := view.Failure
			if projection == "stop failure" {
				failure = view.Stop.Failure
			}
			failure.RetryAfter = time.Hour
			failure.Category = "invalid-edit"
			fresh, err := out.Turn.View()
			if err != nil || fresh.Failure.RetryAfter != time.Second ||
				fresh.Stop.Failure.RetryAfter != time.Second ||
				fresh.Failure.Category != llm.ErrorRateLimited {
				t.Fatalf("view edit changed saved failure: fresh=%+v err=%v", fresh, err)
			}
		})
	}
}
