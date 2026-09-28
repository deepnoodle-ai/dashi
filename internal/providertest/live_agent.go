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
// that the answer follows it and the projection keeps it.
func RecordedReminder(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "reminder-live",
		Revision: "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 256},
		Limits: dashi.Limits{
			ModelAttempts:  1,
			ProviderPauses: 1,
		},
	}
	out, err := (dashi.Runner{
		Agent:    def,
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(
		ctx,
		dashi.Turn{ID: "reminder-live"},
		dashi.Start{
			CommandID: "start",
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
		strings.ToLower(out.Answer),
		"indigo",
	) {
		t.Fatalf(
			"outcome=%+v answer=%q err=%v",
			out.Stop,
			out.Answer,
			err,
		)
	}
	projected, err := dashi.Project(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range projected {
		for _, part := range message.Content {
			if part.Reminder != nil && part.Reminder.Name == "answer-word" && part.Reminder.Content == "The answer word is indigo." {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("recorded reminder missing from projected history")
	}
}

// IteratorTurn makes one paid streamed model call through Runner.Stream.
func IteratorTurn(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "iterator-live",
		Revision: "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 256},
	}
	var outcome *dashi.Outcome
	previews, committed := 0, 0
	for event, err := range (dashi.Runner{
		Agent:    def,
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Stream(
		ctx,
		dashi.Turn{ID: "iterator-live"},
		dashi.Start{
			CommandID: "start",
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
		if event.Outcome != nil {
			outcome = event.Outcome
		}
	}
	if outcome == nil || outcome.Stop.Kind != dashi.StopCompleted || !strings.Contains(
		strings.ToLower(outcome.Answer),
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

// ReservedFinalAnswer makes exactly two paid calls. The second call must see
// the recorded tool result while no longer advertising application tools.
func ReservedFinalAnswer(t *testing.T, client llm.Client, modelName string) {
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
	limits := dashi.Limits{ModelAttempts: 2}
	def := dashi.Agent{
		ID:           "limit-live",
		Revision:     "1",
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
		Agent:    def,
		Client:   counted,
		Tools:    tool.List{typed},
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(
		ctx,
		dashi.Turn{ID: "limit-live"},
		dashi.Start{
			CommandID: "start",
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8, then answer with the result.")),
			},
		},
	)
	if err != nil || out.Stop.Kind != dashi.StopLimited || out.Stop.Reason != "model_attempts" ||
		!strings.Contains(
			out.Answer,
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
	view, err := dashi.Replay(out.Turn)
	if err != nil || view.ModelAttempts != 2 {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelIntent && step.Model.FinalAnswerOnly {
			if len(step.Model.Tools) != 0 {
				t.Fatalf("final request advertised tools: %+v", step.Model.Tools)
			}
			t.Logf(
				"model=%s attempts=%d answer=%q",
				modelName,
				view.ModelAttempts,
				out.Answer,
			)
			return
		}
	}
	t.Fatal("missing final-answer intent")
}

// StructuredOutput makes one paid call and checks that a per-turn schema is
// satisfied through a recorded dashi_submit_output call.
func StructuredOutput(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := dashi.Agent{
		ID:       "live-structured",
		Revision: "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    modelName,
		Settings: llm.Settings{MaxTokens: 512},
		Limits:   dashi.Limits{ModelAttempts: 1},
	}
	start := dashi.Start{
		CommandID:    "start",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`),
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Call dashi_submit_output with answer equal to 42. Do not write a final text answer.")),
		},
	}
	out, err := (dashi.Runner{
		Agent:    def,
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}).Run(ctx, dashi.Turn{ID: "live-structured"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted || out.Output == nil || out.Output.Source != dashi.OutputFromTool || string(out.Output.Value) != `{"answer":42}` {
		t.Fatalf(
			"stop=%+v output=%+v",
			out.Stop,
			out.Output,
		)
	}
	view, err := dashi.Replay(out.Turn)
	if err != nil || view.Output == nil || view.ModelAttempts != 1 {
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

// AgentTurn makes three paid calls: a streamed final answer, then a local
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
		Revision: "1",
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
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Reply with exactly one word: indigo.")),
			},
		}
		out, err := streamOutcome(
			ctx,
			dashi.Runner{Agent: agent, Client: client, Recorder: recorder},
			dashi.Turn{ID: provider + "-live-stream"},
			start,
		)
		if err != nil {
			t.Fatal(err)
		}
		if out.Stop.Kind != dashi.StopCompleted ||
			!strings.Contains(strings.ToLower(out.Answer), "indigo") {
			t.Fatalf(
				"streamed turn: stop=%+v answer=%q",
				out.Stop,
				out.Answer,
			)
		}
		view, err := dashi.Replay(out.Turn)
		if err != nil || view.ModelAttempts != 1 || len(view.Pending) != 0 {
			t.Fatalf("streamed record: view=%+v err=%v", view, err)
		}
		if out.Turn.Steps[2].Model.Result.Message.Source == nil ||
			out.Turn.Steps[2].Model.Result.Message.Source.Requested.Model != modelName {
			t.Fatalf("streamed answer lost %s message provenance", provider)
		}
		t.Logf(
			"streamed turn: attempts=%d steps=%d answer=%q",
			view.ModelAttempts,
			len(out.Turn.Steps),
			out.Answer,
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
				Input: []llm.Message{
					llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8. Answer with the result.")),
				},
			}
			counted := &countingClient{Client: client}
			runner := dashi.Runner{
				Agent:    agent,
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
					out.Answer,
					"56",
				) || runs != 1 || counted.sends != 2 {
				t.Fatalf(
					"tool turn: stop=%+v answer=%q runs=%d sends=%d",
					out.Stop,
					out.Answer,
					runs,
					counted.sends,
				)
			}
			view, err := dashi.Replay(out.Turn)
			if err != nil || view.ModelAttempts != 2 || len(view.Tools) != 1 || len(view.Pending) != 0 {
				t.Fatalf("tool record: view=%+v err=%v", view, err)
			}
			for _, observed := range view.Tools {
				if observed.Status != dashi.ToolSettled || observed.Outcome == nil ||
					observed.Outcome.State != tool.StateSucceeded {
					t.Fatalf("tool was not settled: %+v", observed)
				}
			}
			expected := []dashi.StepKind{
				dashi.StepStarted,
				dashi.StepModelIntent,
				dashi.StepModelSettled,
				dashi.StepToolDecision,
				dashi.StepToolStarted,
				dashi.StepToolSettled,
				dashi.StepModelIntent,
				dashi.StepModelSettled,
				dashi.StepStopped,
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
				"tool turn: attempts=%d steps=%d tool_runs=%d answer=%q",
				view.ModelAttempts,
				len(out.Turn.Steps),
				runs,
				out.Answer,
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
// Outcome, so a check can exercise a streamed model request.
func streamOutcome(
	ctx context.Context,
	runner dashi.Runner,
	turn dashi.Turn,
	command dashi.Command,
) (dashi.Outcome, error) {
	for event, err := range runner.Stream(ctx, turn, command) {
		if event.Outcome != nil {
			return *event.Outcome, err
		}
		if err != nil {
			return dashi.Outcome{}, err
		}
	}
	return dashi.Outcome{}, errors.New(
		"providertest: stream ended without an outcome",
	)
}
