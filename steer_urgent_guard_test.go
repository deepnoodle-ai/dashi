package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/providercontent"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// The tests in this file guard the urgent-steer safety rules: each one
// fails when the guard it names is removed.

// signalingSource is a tableSource that signals Ready when the test says
// so, reports each Steers call on fetched, and records what a run
// releases. Unlike Inbox, the test decides exactly what each fetch
// returns, so it can make the listener and FR-6 see different things.
type signalingSource struct {
	tableSource
	ready    chan struct{}
	fetched  chan struct{}
	relMu    sync.Mutex
	released []string
}

func newSignalingSource() *signalingSource {
	return &signalingSource{
		ready:   make(chan struct{}, 1),
		fetched: make(chan struct{}, 64),
	}
}

func (s *signalingSource) Ready(string) (<-chan struct{}, func()) {
	return s.ready, func() {}
}

func (s *signalingSource) Steers(
	ctx context.Context,
	turnID string,
) ([]Steer, error) {
	steers, err := s.tableSource.Steers(ctx, turnID)
	s.fetched <- struct{}{}
	return steers, err
}

func (s *signalingSource) Release(
	_ context.Context,
	_ string,
	unrecorded []string,
	inDoubt []string,
) {
	s.relMu.Lock()
	defer s.relMu.Unlock()
	s.released = append(s.released, unrecorded...)
	s.released = append(s.released, inDoubt...)
}

// set replaces what Steers returns: rows, or failure when it is non-nil.
func (s *signalingSource) set(failure error, rows ...Steer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = rows
	s.failSteers = failure
}

// signal wakes the listener and waits until it fetched. Call it only
// while the run goroutine is blocked, so no other Steers call can be the
// one observed.
func (s *signalingSource) signal(t *testing.T) {
	t.Helper()
	for len(s.fetched) > 0 {
		<-s.fetched
	}
	select {
	case s.ready <- struct{}{}:
	default:
	}
	select {
	case <-s.fetched:
	case <-time.After(5 * time.Second):
		t.Error("the listener never fetched")
	}
}

// decided signals twice. The listener handles one signal at a time, so
// once the second fetch is seen it has finished deciding on the first:
// the call's context then shows whether it was cut.
func (s *signalingSource) decided(t *testing.T) {
	t.Helper()
	s.signal(t)
	s.signal(t)
}

func urgentSteer(id, text string) Steer {
	steer := textSteer(id, text)
	steer.Urgent = true
	return steer
}

// passCounter is a BeforeModel hook that counts its passes and runs
// first on the first one. It stops the run after five passes, so a
// broken loop-back guard fails the test instead of hanging it.
func passCounter(asked *int, first func()) Hooks {
	hook := ModelHook{
		Name: "count",
		Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			*asked++
			if *asked == 1 && first != nil {
				first()
			}
			if *asked >= 5 {
				return ModelDecision{Stop: &Stop{
					Kind:   StopFailed,
					Reason: "BeforeModel looped",
				}}, nil
			}
			return ModelDecision{}, nil
		},
	}
	return hook.Hooks()
}

func hookResultsOf(turn Turn, point HookPoint) []HookResult {
	var out []HookResult
	for _, step := range turn.Steps {
		for _, result := range step.HookResults {
			if result.Point == point {
				out = append(out, result)
			}
		}
	}
	return out
}

func steeringInterrupts(turn Turn) int {
	n := 0
	for _, fact := range finishes(turn) {
		if fact.Interrupted == InterruptSteering {
			n++
		}
	}
	return n
}

// unsafeProviderTool is a provider tool without annotations, so a
// call offering it is not safe to repeat.
func unsafeProviderTool() tool.Tool {
	return tool.Tool{Definition: tool.Definition{
		Name: "remote",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "remote",
		},
	}}
}

func TestLoopBackIsBoundedWhenFR6MissesTheSteer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{name: "source fails", failure: errors.New("source down")},
		{name: "source drops the steer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newSignalingSource()
			client := newUrgentClient(urgentCall{result: final("ok")})
			asked := 0
			hooks := passCounter(&asked, func() {
				source.set(nil, urgentSteer("s1", "use staging"))
				source.signal(t)
				source.set(tc.failure)
			})
			out := runUrgent(t, client, source, func(r *Runner) {
				r.Hooks = hooks
			})
			if out.Stop.Kind != StopCompleted || client.sends != 1 {
				t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
			}
			if asked != 2 {
				t.Fatalf(
					"BeforeModel ran %d times; want one loop-back, then a "+
						"sent call",
					asked,
				)
			}
		})
	}
}

func TestUrgentSteerDuringBeforeSendGoesBackForTheSteer(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(urgentCall{result: final("ok")})
	asked := 0
	hook := SendHook{
		Name: "reserve",
		Func: func(context.Context, SendPlan) (SendDecision, error) {
			asked++
			if asked == 1 {
				inbox.sendUrgent(t, "use staging")
			}
			terms := json.RawMessage(fmt.Sprintf(`{"pass":%d}`, asked))
			return SendDecision{Reservation: terms}, nil
		},
	}
	out := runUrgent(t, client, inbox, func(r *Runner) {
		r.Hooks = hook.Hooks()
	})
	if out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	callStarts := stepsOf(out.Turn, StepModelCallStarted)
	if len(callStarts) != 1 {
		t.Fatalf(
			"%d model requests recorded; a call cut during BeforeSend "+
				"records none",
			len(callStarts),
		)
	}
	for _, fact := range finishes(out.Turn) {
		if fact.NotSent {
			t.Fatalf("a call cut during BeforeSend was finished: %+v", fact)
		}
	}
	if asked != 2 {
		t.Fatalf(
			"BeforeSend ran %d times; the pass after the steer asks again",
			asked,
		)
	}
	if n := len(hookResultsOf(out.Turn, HookBeforeSend)); n != 1 {
		t.Fatalf(
			"%d BeforeSend results recorded; the abandoned pass left its own",
			n,
		)
	}
	terms := string(callStarts[0].Model.Reservation["reserve"])
	if terms != `{"pass":2}` {
		t.Fatalf(
			"reservation = %s; the request must carry the second pass's",
			terms,
		)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call")
	}
}

func TestUrgentSteerInterruptsOnlyOnce(t *testing.T) {
	source := newSignalingSource()
	var recut bool
	client := newUrgentClient(
		urgentCall{
			deltas: []llm.Delta{textDelta("Editing main.go")},
			during: func(context.Context) {
				source.set(nil, urgentSteer("s1", "stop, wrong file"))
				source.signal(t)
				// FR-6 misses the steer, so the replacement is sent
				// without it and the source still offers it.
				source.set(errors.New("source down"))
			},
			block: true,
		},
		urgentCall{
			during: func(ctx context.Context) {
				source.set(nil, urgentSteer("s1", "stop, wrong file"))
				source.decided(t)
				recut = ctx.Err() != nil
			},
			result: final("Editing main.go, done"),
		},
		urgentCall{result: final("Editing util.go")},
	)
	out := runUrgent(t, client, source)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if recut {
		t.Fatal("a steer that already cut a call cut its replacement")
	}
	if n := steeringInterrupts(out.Turn); n != 1 {
		t.Fatalf("%d steering interrupts; one steer interrupts once", n)
	}
}

func TestUnsafeProviderToolCallIsNeverCutAfterHandOff(t *testing.T) {
	source := newSignalingSource()
	var cut bool
	client := newUrgentClient(
		urgentCall{
			during: func(ctx context.Context) {
				source.set(nil, urgentSteer("s1", "stop"))
				source.decided(t)
				cut = ctx.Err() != nil
			},
			result: final("done"),
		},
		urgentCall{result: final("stopped")},
	)
	out := runUrgent(t, client, source, func(r *Runner) {
		r.Tools = tool.List{unsafeProviderTool()}
	})
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if cut || steeringInterrupts(out.Turn) != 0 {
		t.Fatal("a call offering an unsafe provider tool was cut")
	}
	warnings := warningsOf(out.Turn, WarningSteering)
	if len(warnings) != 1 ||
		!strings.Contains(warnings[0].Message, "provider tool") {
		t.Fatalf("want one warning naming the provider tool: %+v", warnings)
	}
	if hasText(client.lastMessages(), "stop") < 0 {
		t.Fatal("the steer did not reach the next call")
	}
}

func TestUnsafeProviderToolCallIsCutBeforeHandOff(t *testing.T) {
	source := newSignalingSource()
	client := newUrgentClient(urgentCall{result: final("ok")})
	rec := &callStartHook{MemoryRecorder: NewMemoryRecorder()}
	rec.onCallStart = func() {
		source.set(nil, urgentSteer("s1", "use staging"))
		source.signal(t)
	}
	out := runUrgent(t, client, source, func(r *Runner) {
		r.Recorder = rec
		r.Tools = tool.List{unsafeProviderTool()}
	})
	if out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	cut := finishes(out.Turn)[0]
	if cut.Interrupted != InterruptSteering || !cut.NotSent {
		t.Fatalf(
			"a call not yet handed off must be cut, whatever it offers: %+v",
			cut,
		)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call that was sent")
	}
}

func TestPauseDeclinesUrgentCut(t *testing.T) {
	source := newSignalingSource()
	pause := make(chan struct{})
	var cut bool
	client := newUrgentClient(urgentCall{
		during: func(ctx context.Context) {
			close(pause)
			source.set(nil, urgentSteer("s1", "stop"))
			source.decided(t)
			cut = ctx.Err() != nil
		},
		result: callResult("c1", "lookup"),
	})
	out, err := Runner{
		Client: client,
		Tools:  tool.List{lookupTool()},
	}.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithInbox(source),
		WithPause(pause),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cut || steeringInterrupts(out.Turn) != 0 {
		t.Fatal("a call was cut after a pause was requested")
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v; the pause must still stop the run", out.Stop)
	}
	found := false
	for _, w := range warningsOf(out.Turn, WarningSteering) {
		if strings.Contains(w.Message, "pause") {
			found = true
		}
	}
	if !found {
		t.Fatal("the declined cut was not reported")
	}
}

func TestSteeredCallNotDispatchedFinishesWithoutResult(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			block:  true,
			err: &llm.Error{
				Category:   llm.ErrorTransport,
				SendStatus: llm.SendStatusNotSent,
			},
		},
		urgentCall{result: final("ok")},
	)
	out := runUrgent(t, client, inbox)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	cut := finishes(out.Turn)[0]
	if cut.Interrupted != InterruptSteering {
		t.Fatalf("cause = %q", cut.Interrupted)
	}
	if !cut.NotSent {
		t.Fatal("a call the client never dispatched was counted as sent")
	}
	if cut.Result != nil {
		t.Fatalf("a call never dispatched carries result=%+v", cut.Result)
	}
}

func TestListenerClaimsAreReleased(t *testing.T) {
	source := newSignalingSource()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newUrgentClient(urgentCall{
		during: func(callCtx context.Context) {
			source.set(nil, urgentSteer("s1", "stop"))
			source.signal(t)
			<-callCtx.Done()
			cancel()
		},
		block: true,
	})
	out, err := Runner{Client: client}.Run(
		ctx,
		Turn{ID: "t1"},
		startInput(),
		WithInbox(source),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	source.relMu.Lock()
	released := slices.Clone(source.released)
	source.relMu.Unlock()
	if !slices.Equal(released, []string{"s1"}) {
		t.Fatalf(
			"released %v; a steer only the listener fetched must be "+
				"released",
			released,
		)
	}
}

// TestUrgentSetIsCleared covers the two ways a steer leaves the urgent
// set: FR-6 records it, or the source stops returning it. A steer left in
// the set would send the next call back to FR-6 for nothing.
func TestUrgentSetIsCleared(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
	}{
		{name: "recorded by FR-6"},
		{name: "dropped by the source", drop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newSignalingSource()
			// The listener takes the steer while a tool runs, when no
			// call is registered to cut.
			slow := tool.Tool{
				Definition: lookupTool().Definition,
				Executor: execFunc(func(
					context.Context,
					tool.Call,
				) (tool.Outcome, error) {
					source.set(nil, urgentSteer("s1", "use staging"))
					source.signal(t)
					if tc.drop {
						source.set(nil)
					}
					return tool.Outcome{
						State:  tool.StateSucceeded,
						Output: new(tool.TextOutput("found")),
					}, nil
				}),
			}
			client := newUrgentClient(
				urgentCall{result: callResult("c1", "lookup")},
				urgentCall{result: final("ok")},
			)
			asked := 0
			out := runUrgent(t, client, source, func(r *Runner) {
				r.Tools = tool.List{slow}
				r.Hooks = passCounter(&asked, nil)
			})
			if out.Stop.Kind != StopCompleted || client.sends != 2 {
				t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
			}
			if asked != 2 {
				t.Fatalf(
					"BeforeModel ran %d times for two calls; a stale "+
						"urgent steer sent a call back",
					asked,
				)
			}
		})
	}
}

// adaptingClient is an urgentClient whose Prepare first applies the
// provider content rules, as a real client does, so a request those rules
// reject fails here too.
type adaptingClient struct {
	*urgentClient
}

func (c adaptingClient) Prepare(
	ctx context.Context,
	in llm.Input,
) (llm.Request, error) {
	if _, _, err := providercontent.Adapt(in, fixtureOrigin); err != nil {
		return llm.Request{}, err
	}
	return c.urgentClient.Prepare(ctx, in)
}

// searchTool is a provider search tool that is safe to repeat, so only
// the pause can keep a call offering it from being cut.
func searchTool() tool.Tool {
	return tool.Tool{Definition: tool.Definition{
		Name: "web_search",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "web_search",
		},
		Annotations: tool.Annotations{ReadOnly: new(true)},
	}}
}

// pausedMidSearch is a result the provider paused while a provider search
// had not finished, as an Anthropic pause_turn does. The request after it
// must end with it, so the provider can finish the search.
func pausedMidSearch() llm.Result {
	result := final("Searching")
	result.Stop = llm.Stop{Kind: llm.StopPause}
	result.Message.Content = append(result.Message.Content, llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "search_1",
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: fixtureOrigin,
		},
		Replay: fixtureReplay("server_tool_use"),
	})
	return result
}

// searchFinished is the continuation after pausedMidSearch: the search's
// result, then text.
func searchFinished(text string) llm.Result {
	result := final(text)
	result.Message.Content = append(
		[]llm.Content{{
			Kind: llm.ContentProviderToolResult,
			ProviderToolResult: &llm.ProviderToolResult{
				CallID: "search_1",
				Type:   "web_search_tool_result",
				Origin: fixtureOrigin,
			},
			Replay: fixtureReplay("web_search_tool_result"),
		}},
		result.Message.Content...,
	)
	return result
}

func fixtureReplay(typ string) *llm.Opaque {
	return &llm.Opaque{
		Version: 1,
		Origin:  fixtureOrigin,
		Type:    typ,
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"type":"` + typ + `"}`),
	}
}

func TestSteerWaitsForAPausedProviderToolCall(t *testing.T) {
	inbox := newWatchedInbox()
	client := adaptingClient{newUrgentClient(
		urgentCall{
			during: func(context.Context) {
				if _, err := inbox.SendText("use staging"); err != nil {
					t.Error(err)
				}
			},
			result: pausedMidSearch(),
		},
		urgentCall{result: searchFinished("Found it")},
		urgentCall{result: final("Found it on staging")},
	)}
	agent := testAgent()
	agent.Limits.ModelCalls = 4
	out := runUrgentAgent(t, client, inbox, agent, func(r *Runner) {
		r.Tools = tool.List{searchTool()}
	})
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	continuation := client.inputs()[1].Messages
	if hasText(continuation, "use staging") >= 0 {
		t.Fatal("a steer landed between a paused provider tool call and its end")
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call after the continuation")
	}
}

func TestUrgentSteerDoesNotCutAPausedProviderToolCall(t *testing.T) {
	source := newSignalingSource()
	var cut bool
	client := adaptingClient{newUrgentClient(
		urgentCall{result: pausedMidSearch()},
		urgentCall{
			deltas: []llm.Delta{textDelta("Still searching")},
			during: func(ctx context.Context) {
				source.set(nil, urgentSteer("s1", "use staging"))
				source.decided(t)
				cut = ctx.Err() != nil
			},
			result: searchFinished("Found it"),
		},
		urgentCall{result: final("Found it on staging")},
	)}
	agent := testAgent()
	agent.Limits.ModelCalls = 4
	out := runUrgentAgent(t, client, source, agent, func(r *Runner) {
		r.Tools = tool.List{searchTool()}
	})
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if cut || steeringInterrupts(out.Turn) != 0 {
		t.Fatal("the continuation of a paused provider tool call was cut")
	}
	warnings := warningsOf(out.Turn, WarningSteering)
	if len(warnings) != 1 ||
		!strings.Contains(warnings[0].Message, "paused") {
		t.Fatalf("want one warning naming the pause: %+v", warnings)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call after the continuation")
	}
}

// pausedWithFinishedSearch is a pause whose provider search has its result in
// the same message, so nothing is left for the continuation to finish.
func pausedWithFinishedSearch() llm.Result {
	result := pausedMidSearch()
	result.Message.Content = append(
		result.Message.Content,
		searchFinished("").Message.Content[0],
	)
	return result
}

func TestPausedMidProviderToolCall(t *testing.T) {
	withStatus := func(status string) llm.Result {
		result := pausedMidSearch()
		for _, part := range result.Message.Content {
			if part.ProviderToolUse != nil {
				part.ProviderToolUse.Status = status
			}
		}
		return result
	}
	notPaused := pausedMidSearch()
	notPaused.Stop = llm.Stop{Kind: llm.StopFinished}
	cases := []struct {
		name   string
		result llm.Result
		want   bool
	}{
		{"unfinished search", pausedMidSearch(), true},
		{"result in the same message", pausedWithFinishedSearch(), false},
		{"completed status", withStatus("completed"), false},
		{"failed status", withStatus("failed"), false},
		{"not a pause", notPaused, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pausedMidProviderToolCall(c.result); got != c.want {
				t.Fatalf("pausedMidHostedCall = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSteerIsDeliveredAfterAPauseWithFinishedSearch(t *testing.T) {
	inbox := newWatchedInbox()
	client := adaptingClient{newUrgentClient(
		urgentCall{
			during: func(context.Context) {
				if _, err := inbox.SendText("use staging"); err != nil {
					t.Error(err)
				}
			},
			result: pausedWithFinishedSearch(),
		},
		urgentCall{result: final("Found it on staging")},
	)}
	agent := testAgent()
	agent.Limits.ModelCalls = 4
	out := runUrgentAgent(t, client, inbox, agent, func(r *Runner) {
		r.Tools = tool.List{searchTool()}
	})
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if hasText(client.inputs()[1].Messages, "use staging") < 0 {
		t.Fatal("the steer was held after a pause whose search finished")
	}
	if warnings := warningsOf(out.Turn, WarningSteering); len(warnings) != 0 {
		t.Fatalf("unexpected steering warnings: %+v", warnings)
	}
}

func (s *signalingSource) Waiting(ctx context.Context, turnID string) ([]Command, error) {
	return steerCommands(s.Steers(ctx, turnID))
}
