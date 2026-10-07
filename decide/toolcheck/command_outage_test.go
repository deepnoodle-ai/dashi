package toolcheck_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

type commandTransport struct {
	mu       sync.Mutex
	commands []string
	answer   func(context.Context, string) (*decide.Response, error)
}

func (c *commandTransport) SystemOne(
	ctx context.Context,
	req *decide.Request,
) (*decide.Response, error) {
	var state struct {
		Command string `json:"command"`
	}
	b, _ := json.Marshal(req.State)
	json.Unmarshal(b, &state)
	c.mu.Lock()
	c.commands = append(c.commands, state.Command)
	c.mu.Unlock()
	return c.answer(ctx, state.Command)
}

func (c *commandTransport) ListModels(context.Context) (*decide.ModelList, error) {
	return nil, errors.ErrUnsupported
}

func (c *commandTransport) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commands...)
}

func commandClient(t *testing.T, tr *commandTransport) *decide.Client {
	t.Helper()
	c, err := decide.NewClient(
		decide.WithoutEnvironment(),
		decide.WithTransport(tr),
		decide.WithMaxRetries(5),
		decide.WithRetryBackoff(0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func commandAnswer() *decide.Response {
	return &decide.Response{
		Model: "jev-test",
		Answers: map[string]decide.Answer{
			"severe": decidetest.NoulAnswer(0.99),
		},
		Usage: decide.Usage{InputTokens: 5, OutputTokens: 1},
	}
}

func TestCommandRiskKeepsRepeatedPayloadFailureSuspect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		parts   int
	}{
		{name: "one part", command: "payload that breaks Decide", parts: 1},
		{
			name:    "eight parts",
			command: strings.Repeat("x", 8*toolcheck.PartBytes),
			parts:   8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFixture(t)
			original := &decide.APIError{StatusCode: 500, RequestID: "original"}
			tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
				if text == "true" {
					return commandAnswer(), nil
				}
				return nil, original
			}}
			var seen checks
			risk := toolcheck.CommandRisk{Client: commandClient(t, tr), OnCheck: seen.add}
			for range 2 {
				v, err := risk.Check(t.Context(), bashPlan(tc.command))
				if v.Checked || v.Flagged {
					t.Fatal("the benign answer became the command's verdict")
				}
				if toolcheck.IsOutage(err) || !errors.Is(err, decide.ErrServer) {
					t.Fatalf("payload failure = %v", err)
				}
				if got, ok := errors.AsType[*decide.APIError](err); !ok || got != original {
					t.Fatal("the original API error was not preserved")
				}
				if toolcheck.CauseOf(err) != toolcheck.CauseServerError {
					t.Fatalf("cause = %s", toolcheck.CauseOf(err))
				}
				if v.Requests != tc.parts+1 {
					t.Fatalf("requests = %d", v.Requests)
				}
				if v.Usage.InputTokens != 5 || v.Usage.OutputTokens != 1 {
					t.Fatalf("observed probe usage = %+v", v.Usage)
				}
			}
			if toolcheck.PauseCount() != 0 {
				t.Fatal("an isolated payload failure paused the check")
			}
			if len(seen.all()) != 2 {
				t.Fatal("OnCheck did not report one result per command")
			}
			if got := len(tr.calls()); got != 2*(tc.parts+1) {
				t.Fatalf("provider calls = %d; hidden SDK retries ran", got)
			}
		})
	}
}

func TestCommandRiskProbeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		pause  bool
		answer *decide.Response
	}{
		{name: "valid answer", answer: commandAnswer()},
		{name: "server", err: &decide.APIError{StatusCode: 503}, pause: true},
		{name: "network", err: &net.OpError{Op: "read", Err: errors.New("dropped")}, pause: true},
		{name: "auth", err: &decide.APIError{StatusCode: 401}, pause: true},
		{name: "rate limit", err: &decide.APIError{StatusCode: 429}},
		{name: "rejected", err: &decide.APIError{StatusCode: 422}},
		{name: "invalid answer", answer: &decide.Response{Answers: map[string]decide.Answer{}}},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			original := &decide.APIError{StatusCode: 500, RequestID: "first"}
			tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
				if text == "true" {
					return tc.answer, tc.err
				}
				return nil, original
			}}
			risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
			v, err := risk.Check(t.Context(), bashPlan("failed command"))
			if toolcheck.IsOutage(err) != tc.pause {
				t.Fatalf("error = %v, outage = %v", err, toolcheck.IsOutage(err))
			}
			if !errors.Is(err, decide.ErrServer) {
				t.Fatal("original cause was lost")
			}
			if v.Checked || v.Requests != 2 {
				t.Fatalf("verdict = %+v", v)
			}
			p, paused := errors.AsType[*toolcheck.PausedError](err)
			if paused != tc.pause {
				t.Fatalf("pause = %v", paused)
			}
			if tc.pause {
				if !p.Until.Equal(f.clock.Add(time.Minute)) {
					t.Fatalf("pause end = %s", p.Until)
				}
				skip, again := risk.Check(t.Context(), bashPlan("another command"))
				if again != p || skip.Requests != 0 {
					t.Fatal("a paused check did not return the original pause")
				}
			}
			if len(tr.calls()) != 2 {
				t.Fatal("probe or command was retried")
			}
		})
	}
}

func TestCommandRiskKeepsAnIsolatedConnectionFailureSuspect(t *testing.T) {
	newFixture(t)
	original := &net.OpError{Op: "read", Err: errors.New("dropped")}
	tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
		if text == "true" {
			return commandAnswer(), nil
		}
		return nil, original
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	for range 2 {
		v, err := risk.Check(t.Context(), bashPlan("connection-breaking payload"))
		if toolcheck.IsOutage(err) || !errors.Is(err, original) {
			t.Fatalf("isolated connection failure = %v", err)
		}
		if toolcheck.CauseOf(err) != toolcheck.CauseNetworkError {
			t.Fatalf("cause = %s", toolcheck.CauseOf(err))
		}
		if v.Requests != 2 || v.Checked {
			t.Fatalf("verdict = %+v", v)
		}
	}
	if toolcheck.PauseCount() != 0 || len(tr.calls()) != 4 {
		t.Fatal("an isolated connection failure opened a pause or retried")
	}
}

func TestCommandRiskHoldsAnUnconfirmedOutage(t *testing.T) {
	f := newFixture(t)
	f.srv.FailNext(http.StatusInternalServerError)
	var ran []string
	out, _ := run(t, tool.List{bashTool(&ran)}, toolcheck.CommandRisk{Client: f.client}.Hooks(),
		dashitest.CallTool("call-1", "bash", `{"command":"rm -rf ~"}`),
	)
	if len(ran) != 0 || out.Stop.Kind != dashi.StopWaiting {
		t.Fatalf("ran = %v, stop = %+v", ran, out.Stop)
	}
	r := hookResults(out, toolcheck.CommandRiskName)
	if len(r) != 1 || r[0].Verdict != dashi.VerdictNeedsApproval {
		t.Fatalf("hook result = %+v", r)
	}
	if !strings.Contains(r[0].Reason, "server error") {
		t.Fatalf("reason = %q", r[0].Reason)
	}
}

func TestCommandRiskKeepsObservedUsageWhenOnePartFails(t *testing.T) {
	newFixture(t)
	tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
		if strings.HasPrefix(text, "x") {
			return nil, decide.ErrServer
		}
		return commandAnswer(), nil
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	command := strings.Repeat("x", toolcheck.PartBytes) + "echo other part"
	v, err := risk.Check(t.Context(), bashPlan(command))
	if toolcheck.IsOutage(err) || v.Checked {
		t.Fatalf("failed whole command = %+v, %v", v, err)
	}
	if v.Requests != 3 || v.Usage.InputTokens != 10 || v.Usage.OutputTokens != 2 {
		t.Fatalf("observations = %+v", v)
	}
}

func TestCommandRiskCancellationBeforeAPauseSkipsAllRequests(t *testing.T) {
	newFixture(t)
	tr := &commandTransport{answer: func(context.Context, string) (*decide.Response, error) {
		return nil, decide.ErrAuth
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	_, err := risk.Check(t.Context(), bashPlan("first"))
	if !toolcheck.IsOutage(err) {
		t.Fatalf("auth failure = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	v, err := risk.Check(ctx, bashPlan("canceled"))
	if !errors.Is(err, context.Canceled) || toolcheck.IsOutage(err) {
		t.Fatalf("canceled paused check = %v", err)
	}
	if v.Requests != 0 || len(tr.calls()) != 1 {
		t.Fatal("a canceled check initiated a request")
	}
}

func TestCommandRiskConfirmationKeepsTheOriginalDeadline(t *testing.T) {
	newFixture(t)
	var deadline time.Time
	tr := &commandTransport{answer: func(ctx context.Context, text string) (*decide.Response, error) {
		d, _ := ctx.Deadline()
		if text != "true" {
			deadline = d
			return nil, decide.ErrServer
		}
		if d != deadline {
			t.Error("confirmation reset the check deadline")
		}
		<-ctx.Done()
		return nil, decide.ErrServer
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr), Timeout: 30 * time.Millisecond}
	v, err := risk.Check(t.Context(), bashPlan("slow confirmation"))
	if !errors.Is(err, context.DeadlineExceeded) || toolcheck.IsOutage(err) {
		t.Fatalf("deadline result = %v", err)
	}
	if v.Requests != 2 || toolcheck.PauseCount() != 0 {
		t.Fatalf("requests = %d, pauses = %d", v.Requests, toolcheck.PauseCount())
	}
}

func TestCommandRiskCancellationDuringConfirmationNeverPauses(t *testing.T) {
	newFixture(t)
	entered := make(chan struct{})
	tr := &commandTransport{answer: func(ctx context.Context, text string) (*decide.Response, error) {
		if text != "true" {
			return nil, decide.ErrServer
		}
		close(entered)
		<-ctx.Done()
		return nil, decide.ErrServer
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := risk.Check(ctx, bashPlan("cancel me")); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || toolcheck.IsOutage(err) {
		t.Fatalf("canceled result = %v", err)
	}
	if toolcheck.PauseCount() != 0 {
		t.Fatal("cancellation confirmed an outage")
	}
}

func TestCommandRiskCancellationAtConfirmationResultNeverPauses(t *testing.T) {
	newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
		if text == "true" {
			cancel()
		}
		return nil, decide.ErrServer
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	_, err := risk.Check(ctx, bashPlan("cancel before pause"))
	if !errors.Is(err, context.Canceled) || toolcheck.IsOutage(err) {
		t.Fatalf("canceled result = %v", err)
	}
	if toolcheck.PauseCount() != 0 {
		t.Fatal("a result arriving after cancellation started a pause")
	}
}

func TestCommandRiskConfirmationDoesNotBlockOtherChecks(t *testing.T) {
	newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	tr := &commandTransport{answer: func(ctx context.Context, text string) (*decide.Response, error) {
		switch text {
		case "broken":
			return nil, decide.ErrServer
		case "true":
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return commandAnswer(), nil
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	done := make(chan error, 1)
	go func() { _, err := risk.Check(t.Context(), bashPlan("broken")); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	v, err := risk.Check(ctx, bashPlan("healthy"))
	free()
	if err != nil || !v.Checked {
		t.Fatalf("another check waited behind confirmation: %v", err)
	}
	if err := <-done; err == nil || toolcheck.IsOutage(err) {
		t.Fatalf("isolated failure = %v", err)
	}
	if toolcheck.PauseCount() != 0 {
		t.Fatal("an isolated confirmation paused other checks")
	}
}

func TestCommandRiskConfirmationsShareOnePauseConcurrently(t *testing.T) {
	newFixture(t)
	var mu sync.Mutex
	probes := 0
	both := make(chan struct{})
	tr := &commandTransport{answer: func(_ context.Context, text string) (*decide.Response, error) {
		if text == "true" {
			mu.Lock()
			probes++
			if probes == 2 {
				close(both)
			}
			mu.Unlock()
			<-both
		}
		return nil, decide.ErrServer
	}}
	risk := toolcheck.CommandRisk{Client: commandClient(t, tr)}
	done := make(chan error, 2)
	for _, command := range []string{"first", "second"} {
		go func() { _, err := risk.Check(t.Context(), bashPlan(command)); done <- err }()
	}
	first, second := <-done, <-done
	p, ok := errors.AsType[*toolcheck.PausedError](first)
	if !ok {
		t.Fatalf("error = %v", first)
	}
	other, ok := errors.AsType[*toolcheck.PausedError](second)
	if !ok || p != other {
		t.Fatal("concurrent confirmations created different pauses")
	}
	if len(tr.calls()) != 4 {
		t.Fatalf("provider calls = %d", len(tr.calls()))
	}
}
