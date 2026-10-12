package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// The live agent checks make paid calls through Runner against one provider
// client. Callers gate them behind the live build tag, and AgentCredentials
// skips them unless DASHI_LIVE_AGENT_TEST=1.

// AgentCredentials returns <PROVIDER>_API_KEY and <PROVIDER>_MODEL. It skips
// the test unless DASHI_LIVE_AGENT_TEST=1 and fails it when either is unset.
func AgentCredentials(t *testing.T, provider string) (string, string) {
	t.Helper()
	if os.Getenv("DASHI_LIVE_AGENT_TEST") != "1" {
		t.Skip("set DASHI_LIVE_AGENT_TEST=1 to make paid agent calls")
	}
	key, modelName := os.Getenv(provider+"_API_KEY"), os.Getenv(provider+"_MODEL")
	if key == "" || modelName == "" {
		t.Fatalf("%s_API_KEY and %s_MODEL are required", provider, provider)
	}
	return key, modelName
}

// RecordedReminder makes one paid call with a recorded reminder and checks
// that the reply follows it and the turn's messages keep it.
func RecordedReminder(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "reminder-live",
		Version:  "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 256},
		Limits: dashi.Limits{
			ModelCalls:     1,
			ProviderPauses: 1,
		},
	}
	out, err := (dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(
		ctx,
		dashi.Turn{ID: "reminder-live"},
		dashi.Start{
			CommandID: "start",
			Agent:     def,
			Origin:    dashi.StartOperator,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Reply with exactly the one word specified by the application reminder.")),
				llm.ReminderMessage(llm.Reminder{
					Name:    "answer-word",
					Tier:    llm.ReminderOperator,
					Content: "The answer word is indigo.",
				}),
			},
		},
	)
	if err != nil || out.Stop.Kind != dashi.StopCompleted || !strings.Contains(
		strings.ToLower(out.Reply),
		"indigo",
	) {
		t.Fatalf(
			"outcome=%+v answer=%q err=%v",
			out.Stop,
			out.Reply,
			err,
		)
	}
	messages, err := dashi.TurnMessagesFull(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Reminder != nil && part.Reminder.Name == "answer-word" && part.Reminder.Content == "The answer word is indigo." {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("recorded reminder missing from history")
	}
}

// IteratorTurn makes one paid streamed model call through Runner.Stream.
func IteratorTurn(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "iterator-live",
		Version:  "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 256},
	}
	var outcome *dashi.RunResult
	previews, committed := 0, 0
	for event, err := range (dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Stream(
		ctx,
		dashi.Turn{ID: "iterator-live"},
		dashi.Start{
			CommandID: "start",
			Agent:     def,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Reply with exactly one word: indigo.")),
			},
		},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil {
			previews++
		}
		if event.Step != nil {
			committed++
		}
		if event.Result != nil {
			outcome = event.Result
		}
	}
	if outcome == nil || outcome.Stop.Kind != dashi.StopCompleted || !strings.Contains(
		strings.ToLower(outcome.Reply),
		"indigo",
	) || previews == 0 || committed < 4 {
		t.Fatalf(
			"outcome=%+v previews=%d committed=%d",
			outcome,
			previews,
			committed,
		)
	}
	t.Logf(
		"provider=%s model=%s previews=%d steps=%d",
		client.Provider(),
		modelName,
		previews,
		committed,
	)
}

// ReservedFinalReply makes exactly two paid calls. The second call must see
// the recorded tool result while no longer advertising application tools.
func ReservedFinalReply(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runs := 0
	typed, err := tool.Func[struct {
		A int `json:"a"`
		B int `json:"b"`
	}]("multiply_numbers", "Multiply two integers and return the product.", func(_ context.Context, _ tool.Call, args struct {
		A int `json:"a"`
		B int `json:"b"`
	}) (tool.Outcome, error) {
		if args.A != 7 || args.B != 8 {
			return tool.Outcome{}, tool.Fatal(errors.New("unexpected multiplication arguments"))
		}
		runs++
		return tool.Succeeded("56"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingClient{Client: client}
	limits := dashi.Limits{ModelCalls: 2}
	def := dashi.Agent{
		ID:           "limit-live",
		Version:      "1",
		Provider:     client.Provider(),
		API:          client.API(),
		Model:        modelName,
		Instructions: "Use multiply_numbers to calculate products. Report the result after the tool returns.",
		Settings: llm.Settings{
			MaxTokens:  1024,
			ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceAny},
		},
		Limits: limits,
	}
	out, err := (dashi.Runner{
		Client:   counted,
		Tools:    tool.List{typed},
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(
		ctx,
		dashi.Turn{ID: "limit-live"},
		dashi.Start{
			CommandID: "start",
			Agent:     def,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8, then answer with the result.")),
			},
		},
	)
	if err != nil || out.Stop.Kind != dashi.StopLimited || out.Stop.Reason != "model_calls" ||
		!strings.Contains(
			out.Reply,
			"56",
		) || counted.sends != 2 || runs != 1 {
		t.Fatalf(
			"out=%+v err=%v sends=%d runs=%d",
			out.Stop,
			err,
			counted.sends,
			runs,
		)
	}
	view, err := out.Turn.View()
	if err != nil || view.ModelCalls != 2 {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallStarted && step.Model.FinalReplyOnly {
			if len(step.Model.Tools) != 0 {
				t.Fatalf("final request advertised tools: %+v", step.Model.Tools)
			}
			t.Logf(
				"model=%s calls=%d answer=%q",
				modelName,
				view.ModelCalls,
				out.Reply,
			)
			return
		}
	}
	t.Fatal("missing final-reply call start")
}

// StructuredOutput makes one paid call and checks that a per-turn schema is
// satisfied through a recorded dashi_submit_output call.
func StructuredOutput(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "live-structured",
		Version:  "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 512},
		Limits:   dashi.Limits{ModelCalls: 1},
	}
	start := dashi.Start{
		CommandID:    "start",
		Agent:        def,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`),
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Call dashi_submit_output with answer equal to 42. Do not write a final text answer.")),
		},
	}
	out, err := (dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(ctx, dashi.Turn{ID: "live-structured"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted || out.Output == nil || out.Output.Source != dashi.OutputSourceToolCall || string(out.Output.Value) != `{"answer":42}` {
		t.Fatalf(
			"stop=%+v output=%+v",
			out.Stop,
			out.Output,
		)
	}
	view, err := out.Turn.View()
	if err != nil || view.Output == nil || view.ModelCalls != 1 {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	t.Logf(
		"provider=%s model=%s source=%s steps=%d",
		client.Provider(),
		modelName,
		out.Output.Source,
		len(out.Turn.Steps),
	)
}

// AgentTurn makes three paid calls: a streamed final reply, then a local
// tool call followed by a second model call. It checks the recorded steps,
// replay, and that a duplicate start repeats no work.
func AgentTurn(
	t *testing.T,
	provider string,
	client llm.Client,
	modelName string,
) {
	t.Helper()
	agent := dashi.Agent{
		ID:       provider + "-live",
		Version:  "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 1024},
	}

	t.Run("streamed_final", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		recorder := dashi.NewMemoryRecorder()
		start := dashi.Start{
			CommandID: "start",
			Agent:     agent,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Reply with exactly one word: indigo.")),
			},
		}
		out, err := streamOutcome(
			ctx,
			dashi.Runner{Client: client, Recorder: recorder},
			dashi.Turn{ID: provider + "-live-stream"},
			start,
		)
		if err != nil {
			t.Fatal(err)
		}
		if out.Stop.Kind != dashi.StopCompleted ||
			!strings.Contains(strings.ToLower(out.Reply), "indigo") {
			t.Fatalf(
				"streamed turn: stop=%+v answer=%q",
				out.Stop,
				out.Reply,
			)
		}
		view, err := out.Turn.View()
		if err != nil || view.ModelCalls != 1 || len(view.Pending) != 0 {
			t.Fatalf("streamed record: view=%+v err=%v", view, err)
		}
		if out.Turn.Steps[2].Model.Result.Message.Source == nil ||
			out.Turn.Steps[2].Model.Result.Message.Source.Requested.Model != modelName {
			t.Fatalf("streamed answer lost %s message provenance", provider)
		}
		t.Logf(
			"streamed turn: calls=%d steps=%d answer=%q",
			view.ModelCalls,
			len(out.Turn.Steps),
			out.Reply,
		)
	})

	t.Run(
		"local_tool_and_duplicate_start",
		func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			turnID := provider + "-live-tool"
			recorder := dashi.NewMemoryRecorder()
			runs := 0
			typed, err := tool.Func[struct {
				A int `json:"a"`
				B int `json:"b"`
			}]("multiply_numbers", "Multiply two integers and return the product.", func(_ context.Context, _ tool.Call, args struct {
				A int `json:"a"`
				B int `json:"b"`
			}) (tool.Outcome, error) {
				saved, err := recorder.Load(context.Background(), turnID)
				if err != nil {
					return tool.Outcome{}, tool.Fatal(err)
				}
				if len(saved.Steps) == 0 || saved.Steps[len(saved.Steps)-1].Kind != dashi.StepToolStarted {
					return tool.Outcome{}, tool.Fatal(errors.New("tool started before its step was acknowledged"))
				}
				if args.A != 7 || args.B != 8 {
					return tool.Outcome{}, tool.Fatal(errors.New(provider + " supplied unexpected tool arguments"))
				}
				runs++
				return tool.Succeeded(strconv.Itoa(args.A * args.B)), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			agent.Instructions = "For multiplication, use multiply_numbers. After the tool result, answer with only the product."
			start := dashi.Start{
				CommandID: "start",
				Agent:     agent,
				Input: []llm.Message{
					llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8. Answer with the result.")),
				},
			}
			counted := &countingClient{Client: client}
			runner := dashi.Runner{
				Client:   counted,
				Tools:    tool.List{typed},
				Recorder: recorder,
			}
			out, err := runner.Run(
				ctx,
				dashi.Turn{ID: turnID},
				start,
			)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted ||
				!strings.Contains(
					out.Reply,
					"56",
				) || runs != 1 || counted.sends != 2 {
				t.Fatalf(
					"tool turn: stop=%+v answer=%q runs=%d sends=%d",
					out.Stop,
					out.Reply,
					runs,
					counted.sends,
				)
			}
			view, err := out.Turn.View()
			if err != nil || view.ModelCalls != 2 || len(view.Tools) != 1 || len(view.Pending) != 0 {
				t.Fatalf("tool record: view=%+v err=%v", view, err)
			}
			for _, observed := range view.Tools {
				if observed.Status != dashi.ToolFinished || observed.Outcome == nil ||
					observed.Outcome.State != tool.StateSucceeded {
					t.Fatalf("tool did not finish: %+v", observed)
				}
			}
			expected := []dashi.StepKind{
				dashi.StepTurnStarted,
				dashi.StepModelCallStarted,
				dashi.StepModelCallFinished,
				dashi.StepToolDecision,
				dashi.StepToolStarted,
				dashi.StepToolFinished,
				dashi.StepModelCallStarted,
				dashi.StepModelCallFinished,
				dashi.StepTurnStopped,
			}
			if len(out.Turn.Steps) != len(expected) {
				t.Fatalf(
					"tool turn recorded %d steps, want %d",
					len(out.Turn.Steps),
					len(expected),
				)
			}
			for i, want := range expected {
				if out.Turn.Steps[i].Kind != want {
					t.Fatalf("step %d = %s, want %s", i+1, out.Turn.Steps[i].Kind, want)
				}
			}
			for _, index := range []int{2, 7} {
				if out.Turn.Steps[index].Model.Result.Message.Source == nil ||
					out.Turn.Steps[index].Model.Result.Message.Source.Requested.Model != modelName {
					t.Fatalf("model step %d lost %s message provenance", index+1, provider)
				}
			}
			duplicate, err := runner.Run(
				ctx,
				dashi.Turn{ID: turnID},
				start,
			)
			if err != nil || duplicate.Stop.Kind != dashi.StopCompleted ||
				len(duplicate.Turn.Steps) != len(out.Turn.Steps) || runs != 1 || counted.sends != 2 {
				t.Fatalf(
					"duplicate start repeated work: stop=%+v err=%v runs=%d sends=%d",
					duplicate.Stop,
					err,
					runs,
					counted.sends,
				)
			}
			t.Logf(
				"tool turn: calls=%d steps=%d tool_runs=%d answer=%q",
				view.ModelCalls,
				len(out.Turn.Steps),
				runs,
				out.Reply,
			)
		},
	)
}

type countingClient struct {
	llm.Client
	sends int
}

func (c *countingClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	c.sends++
	return c.Client.Send(ctx, call)
}

// streamOutcome runs command through Runner.Stream and returns its final
// RunResult, so a check can exercise a streamed model request.
func streamOutcome(
	ctx context.Context,
	runner dashi.Runner,
	turn dashi.Turn,
	command dashi.Command,
) (dashi.RunResult, error) {
	for event, err := range runner.Stream(ctx, turn, command) {
		if event.Result != nil {
			return *event.Result, err
		}
		if err != nil {
			return dashi.RunResult{}, err
		}
	}
	return dashi.RunResult{}, errors.New(
		"providertest: stream ended without an outcome",
	)
}

// SteerAfterToolBatch makes two paid calls. A steer sent while the tool runs
// must reach the second call after the tool result, which every provider
// must accept, and change the reply.
func SteerAfterToolBatch(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inbox := dashi.NewMemoryInbox()
	sent := false
	type args struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	multiply, err := tool.Func(
		"multiply_numbers",
		"Multiply two integers and return the product.",
		func(_ context.Context, _ tool.Call, in args) (tool.Outcome, error) {
			if !sent {
				sent = true
				if _, err := inbox.SendText(
					"Change of plan: add 10 to the product and answer with only that total.",
				); err != nil {
					return tool.Outcome{}, tool.Fatal(err)
				}
			}
			return tool.Succeeded(strconv.Itoa(in.A * in.B)), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (dashi.Runner{
		Client: client,
		Tools:  tool.List{multiply},
	}).Run(
		ctx,
		dashi.Turn{},
		dashi.Start{
			Agent: dashi.Agent{
				Provider:     client.Provider(),
				API:          client.API(),
				Model:        modelName,
				Instructions: "Use multiply_numbers to calculate products.",
				Settings:     llm.Settings{MaxTokens: 1024},
				Limits:       dashi.Limits{ModelCalls: 4},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text(
				"Use multiply_numbers to calculate 7 times 8.",
			))},
		},
		dashi.WithInbox(inbox),
	)
	if err != nil || out.Stop.Kind != dashi.StopCompleted ||
		!strings.Contains(out.Reply, "66") {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
	}
	delivered := 0
	for _, step := range out.Turn.Steps {
		delivered += len(step.Items())
	}
	if delivered != 1 {
		t.Fatalf("delivered %d steers; want 1", delivered)
	}
	t.Logf("model=%s answer=%q", modelName, out.Reply)
}

// UrgentSteerCutsStream makes two paid calls. An urgent steer sent once a
// long streamed reply has begun must cut that call short, and the provider
// must accept the next request: the partial reply, the interruption
// reminder, and the steer.
func UrgentSteerCutsStream(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inbox := dashi.NewMemoryInbox()
	runner := dashi.Runner{
		Client: client,
	}
	var streamed strings.Builder
	sent := false
	var out dashi.RunResult
	for event, err := range runner.Stream(
		ctx,
		dashi.Turn{},
		dashi.Start{
			Agent: dashi.Agent{
				Provider: client.Provider(),
				API:      client.API(),
				Model:    modelName,
				Settings: llm.Settings{MaxTokens: 2048},
				Limits:   dashi.Limits{ModelCalls: 3},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text(
				"Write a 600-word essay on the history of bridges.",
			))},
		},
		dashi.WithInbox(inbox),
	) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if event.Result != nil {
			out = *event.Result
		}
		if event.Delta == nil || event.Delta.Kind != llm.DeltaText || sent {
			continue
		}
		streamed.WriteString(event.Delta.Text)
		if streamed.Len() >= 80 {
			sent = true
			if _, err := inbox.SendUrgent(
				"Stop the essay. Reply with only the word PINEAPPLE.",
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	var cut *dashi.ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallFinished &&
			step.Model.Interrupted == dashi.InterruptSteering {
			cut = step.Model
		}
	}
	if cut == nil || cut.Result == nil || cut.Result.Message.Text() == "" {
		t.Fatalf(
			"no call was cut short with a partial: stop=%+v calls=%s",
			out.Stop,
			describeCalls(out.Turn),
		)
	}
	if out.Stop.Kind != dashi.StopCompleted ||
		!strings.Contains(strings.ToUpper(out.Reply), "PINEAPPLE") {
		t.Fatalf("stop=%+v answer=%q", out.Stop, out.Reply)
	}
	t.Logf(
		"model=%s partial=%d chars answer=%q",
		modelName,
		len(cut.Result.Message.Text()),
		out.Reply,
	)
}

// UrgentSteerPartialEndsInWhitespace makes two paid calls. It cuts a
// streamed reply where the text so far ends in whitespace, and checks that
// the provider accepts the next request, whose partial Dashi trims.
func UrgentSteerPartialEndsInWhitespace(
	t *testing.T,
	client llm.Client,
	modelName string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inbox := dashi.NewMemoryInbox()
	cutter := &whitespaceCutClient{Client: client, inbox: inbox}
	runner := dashi.Runner{
		Client: cutter,
	}
	var out dashi.RunResult
	// Stream, since only a streamed call hands the client a delta sink.
	for event, err := range runner.Stream(
		ctx,
		dashi.Turn{},
		dashi.Start{
			Agent: dashi.Agent{
				Provider: client.Provider(),
				API:      client.API(),
				Model:    modelName,
				Settings: llm.Settings{MaxTokens: 8192},
				Limits:   dashi.Limits{ModelCalls: 3},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text(
				"Write the numbers from one to two hundred in English words, " +
					"one per line.",
			))},
		},
		dashi.WithInbox(inbox),
	) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if event.Result != nil {
			out = *event.Result
		}
	}
	cut := steeringCut(out.Turn)
	if cut == nil || cut.Result == nil {
		t.Fatalf(
			"no call was cut short with a partial: stop=%+v calls=%s",
			out.Stop,
			describeCalls(out.Turn),
		)
	}
	partial := cut.Result.Message.Text()
	if partial == strings.TrimRightFunc(partial, unicode.IsSpace) {
		t.Fatalf(
			"the recorded partial does not end in whitespace: %q",
			partial,
		)
	}
	if out.Stop.Kind != dashi.StopCompleted ||
		!strings.Contains(strings.ToUpper(out.Reply), "PINEAPPLE") {
		t.Fatalf("stop=%+v answer=%q", out.Stop, out.Reply)
	}
	t.Logf(
		"model=%s partial=%d chars ending %q answer=%q",
		modelName,
		len(partial),
		partial[max(0, len(partial)-6):],
		out.Reply,
	)
}

// whitespaceCutClient forwards a call's deltas until its text is at least
// 80 bytes long, then forwards the next text delta only through its first
// whitespace. Providers tend to start chunks with whitespace rather than
// end them with it, so the delta is split. It then sends an urgent steer,
// forwards nothing more, and drops the client's result for the cut call,
// so the partial the run keeps is the forwarded text.
type whitespaceCutClient struct {
	llm.Client
	inbox *dashi.MemoryInbox
	sent  bool
}

func (c *whitespaceCutClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	sink := call.Sink
	forwarded := 0
	held := false
	call.Sink = func(delta llm.Delta) {
		if held {
			return
		}
		space := strings.IndexFunc(delta.Text, unicode.IsSpace)
		if c.sent || delta.Kind != llm.DeltaText || forwarded < 80 ||
			space < 0 {
			forwarded += len(delta.Text)
			if sink != nil {
				sink(delta)
			}
			return
		}
		_, size := utf8.DecodeRuneInString(delta.Text[space:])
		delta.Text = delta.Text[:space+size]
		if sink != nil {
			sink(delta)
		}
		c.sent = true
		held = true
		if _, err := c.inbox.SendUrgent(
			"Stop the list. Reply with only the word PINEAPPLE.",
		); err != nil {
			panic(err)
		}
	}
	result, err := c.Client.Send(ctx, call)
	if held && err != nil {
		return llm.Result{}, err
	}
	return result, err
}

// UrgentSteerAfterPause makes three paid calls with many web searches, so
// it costs about $1.30 on claude-haiku-4-5. The first call must end in a
// provider pause (Anthropic's pause_turn after ten server iterations); it
// skips when the model finishes without pausing. An urgent steer then
// arrives during the continuation. When the pause left a provider tool call
// unfinished, as a pause during web search does, the continuation must not
// be cut: the steer lands after it. Otherwise the continuation is cut.
// Either way the provider must accept the request that carries the steer.
func UrgentSteerAfterPause(
	t *testing.T,
	client llm.Client,
	modelName string,
	search tool.Tool,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	inbox := dashi.NewMemoryInbox()
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{search},
	}
	paused := ""
	sent := false
	var out dashi.RunResult
	for event, err := range runner.Stream(
		ctx,
		dashi.Turn{},
		dashi.Start{
			Agent: dashi.Agent{
				Provider: client.Provider(),
				API:      client.API(),
				Model:    modelName,
				Settings: llm.Settings{MaxTokens: 4096},
				Limits: dashi.Limits{
					ModelCalls:     4,
					ProviderPauses: 2,
				},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text(
				"Run 14 separate web searches, one per query and one at a time, " +
					"never combining them: the current mayor of each of Oslo, " +
					"Lima, Quito, Accra, Hanoi, Perth, Porto, Turin, Leeds, " +
					"Graz, Bergen, Tampere, Cork, and Basel. After all 14 " +
					"searches, list the mayors.",
			))},
		},
		dashi.WithInbox(inbox),
	) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if event.Result != nil {
			out = *event.Result
		}
		if step := event.Step; paused == "" && step != nil &&
			step.Kind == dashi.StepModelCallFinished && step.Model.Result != nil &&
			step.Model.Result.Stop.Kind == llm.StopPause {
			paused = step.Model.ModelCallID
		}
		if paused == "" || sent || event.Delta == nil ||
			event.ModelCallID == paused {
			continue
		}
		sent = true
		if _, err := inbox.SendUrgent(
			"Stop searching. Reply with only the word PINEAPPLE.",
		); err != nil {
			t.Fatal(err)
		}
	}
	if paused == "" {
		t.Skipf(
			"the model finished without a provider pause: stop=%+v",
			out.Stop,
		)
	}
	cut := steeringCut(out.Turn)
	pending := unfinishedProviderTool(out.Turn, paused)
	switch {
	case pending && cut != nil:
		t.Fatal("the continuation of an unfinished provider tool call was cut")
	case pending && !hasWarning(out.Turn, "provider paused"):
		t.Fatal("the declined cut was not reported")
	case !pending && cut == nil:
		t.Fatalf("the continuation was not cut short: stop=%+v", out.Stop)
	}
	answer := strings.TrimSpace(out.Reply)
	if out.Stop.Kind != dashi.StopCompleted ||
		!strings.Contains(strings.ToUpper(answer), "PINEAPPLE") ||
		len(answer) > 40 {
		t.Fatalf(
			"stop=%+v answer=%q calls=%s",
			out.Stop,
			out.Reply,
			describeCalls(out.Turn),
		)
	}
	t.Logf(
		"model=%s unfinished provider tool call=%t cut=%t answer=%q spend=%v",
		modelName,
		pending,
		cut != nil,
		out.Reply,
		out.Spend.Cost,
	)
}

// unfinishedProviderTool reports whether the result of the model call left a
// provider tool call without its result.
func unfinishedProviderTool(turn dashi.Turn, attempt string) bool {
	for _, step := range turn.Steps {
		if step.Kind != dashi.StepModelCallFinished ||
			step.Model.ModelCallID != attempt || step.Model.Result == nil {
			continue
		}
		open := map[string]bool{}
		for _, part := range step.Model.Result.Message.Content {
			switch {
			case part.ProviderToolUse != nil:
				open[part.ProviderToolUse.ID] = true
			case part.ProviderToolResult != nil:
				delete(open, part.ProviderToolResult.CallID)
			}
		}
		return len(open) > 0
	}
	return false
}

func hasWarning(turn dashi.Turn, text string) bool {
	for _, step := range turn.Steps {
		for _, w := range step.Warnings {
			if w.Code == dashi.WarningSteering &&
				strings.Contains(w.Message, text) {
				return true
			}
		}
	}
	return false
}

// describeCalls summarizes a turn's finished model calls for a failure.
func describeCalls(turn dashi.Turn) string {
	var b strings.Builder
	for _, step := range turn.Steps {
		if step.Kind != dashi.StepModelCallFinished {
			continue
		}
		m := step.Model
		b.WriteString("[" + string(m.Interrupted) + " err=" + m.Error)
		if m.Result != nil {
			b.WriteString(" stop=" + string(m.Result.Stop.Kind) +
				" text=" + strconv.Quote(m.Result.Message.Text()))
		}
		b.WriteString("] ")
	}
	return b.String()
}

// steeringCut returns the last model call an urgent steer cut short.
func steeringCut(turn dashi.Turn) *dashi.ModelFact {
	var cut *dashi.ModelFact
	for _, step := range turn.Steps {
		if step.Kind == dashi.StepModelCallFinished &&
			step.Model.Interrupted == dashi.InterruptSteering {
			cut = step.Model
		}
	}
	return cut
}
