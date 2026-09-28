//go:build live

package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

// Run with KERNEL_LIVE_AGENT_TEST=1 and the corresponding provider key/model.
// Each streamed final turn makes one paid call; each tool turn makes two.
func TestLiveGeminiAgentTurn(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "GEMINI")
	client, err := gemini.New(gemini.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "gemini", client, modelName)
}

func TestLiveOpenAIAgentTurn(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "OPENAI")
	client, err := responses.New(responses.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "openai", client, modelName)
}

func TestLiveAnthropicAgentTurn(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "ANTHROPIC")
	client, err := messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "anthropic", client, modelName)
}

// Each subtest makes one paid call with a recorded reminder.
func TestLiveRecordedReminders(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(string) (llm.Client, error)
	}{
		{"gemini", func(key string) (llm.Client, error) {
			return gemini.New(gemini.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"openai", func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"anthropic", func(key string) (llm.Client, error) {
			return messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, modelName := liveAgentCredentials(t, strings.ToUpper(tc.name))
			client, err := tc.new(key)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			def := kernel.Definition{ID: "reminder-live", Revision: "1", Provider: client.Provider(), API: client.API(), Model: modelName,
				Settings: llm.Settings{MaxTokens: 256}, Limits: kernel.Limits{ModelAttempts: 1, ProviderPauses: 1, ToolConcurrency: 1}}
			out, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: "reminder-live"}, def,
				kernel.Start{ID: "start", Origin: kernel.StartOperator, Input: []llm.Message{
					llm.UserMessage(llm.Text("Reply with exactly the one word specified by the application reminder.")),
					llm.ReminderMessage(llm.Reminder{Name: "answer-word", Tier: llm.ReminderOperator, Content: "The answer word is indigo."}),
				}}, kernel.Bindings{Client: client, Recorder: kernel.NewMemoryRecorder()})
			if err != nil || out.Persistence != kernel.PersistenceAcknowledged || out.Stop.Kind != kernel.StopCompleted || !strings.Contains(strings.ToLower(out.Turn.AnswerText()), "indigo") {
				t.Fatalf("outcome=%+v answer=%q err=%v", out.Stop, out.Turn.AnswerText(), err)
			}
			projected, err := kernel.Project(out.Turn)
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
		})
	}
}

// These probes verify the shared turn contract over real provider tool calls.
// Each test makes one paid call. Run them separately from the broader agent
// turn probes with KERNEL_LIVE_AGENT_TEST=1.
func TestLiveGeminiStructuredOutput(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "GEMINI")
	client, err := gemini.New(gemini.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveStructuredOutput(t, client, modelName)
}

func TestLiveOpenAIStructuredOutput(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "OPENAI")
	client, err := responses.New(responses.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveStructuredOutput(t, client, modelName)
}

func TestLiveAnthropicStructuredOutput(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "ANTHROPIC")
	client, err := messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	runLiveStructuredOutput(t, client, modelName)
}

// Each subtest makes one paid streamed model call through Engine.Stream.
func TestLiveIteratorAgentTurns(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(string) (llm.Client, error)
	}{
		{"gemini", func(key string) (llm.Client, error) {
			return gemini.New(gemini.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"openai", func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"anthropic", func(key string) (llm.Client, error) {
			return messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, modelName := liveAgentCredentials(t, strings.ToUpper(tc.name))
			client, err := tc.new(key)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			def := kernel.Definition{ID: "iterator-live", Revision: "1", Provider: client.Provider(),
				API: client.API(), Model: modelName, Settings: llm.Settings{MaxTokens: 256}, Limits: kernel.DefaultLimits()}
			var outcome *kernel.Outcome
			previews, committed := 0, 0
			for event, err := range (kernel.Engine{}).Stream(ctx, kernel.Turn{ID: "iterator-live"}, def,
				kernel.Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("Reply with exactly one word: indigo."))}},
				kernel.Bindings{Client: client, Recorder: kernel.NewMemoryRecorder()}) {
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
			if outcome == nil || outcome.Stop.Kind != kernel.StopCompleted || !strings.Contains(strings.ToLower(outcome.Turn.AnswerText()), "indigo") || previews == 0 || committed < 4 {
				t.Fatalf("outcome=%+v previews=%d committed=%d", outcome, previews, committed)
			}
			t.Logf("provider=%s model=%s previews=%d steps=%d", client.Provider(), modelName, previews, committed)
		})
	}
}

// Each subtest makes exactly two paid calls. The second call must see the
// recorded tool result while no longer advertising application tools.
func TestLiveReservedFinalAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(string) (llm.Client, error)
	}{
		{"gemini", func(key string) (llm.Client, error) {
			return gemini.New(gemini.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"openai", func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
		{"anthropic", func(key string) (llm.Client, error) {
			return messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityAdjust})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, modelName := liveAgentCredentials(t, strings.ToUpper(tc.name))
			client, err := tc.new(key)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			runs := 0
			typed, err := tool.Func[struct {
				A int `json:"a"`
				B int `json:"b"`
			}]("multiply_numbers", "Multiply two integers and return the product.",
				func(_ context.Context, _ tool.Call, args struct {
					A int `json:"a"`
					B int `json:"b"`
				}) (tool.Outcome, error) {
					if args.A != 7 || args.B != 8 {
						return tool.Outcome{}, tool.Fatal(errors.New("unexpected multiplication arguments"))
					}
					runs++
					output := tool.TextOutput("56")
					return tool.Outcome{State: tool.StateSucceeded, Output: &output}, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			counted := &liveCountingClient{Client: client}
			limits := kernel.DefaultLimits()
			limits.ModelAttempts = 2
			def := kernel.Definition{ID: "limit-live", Revision: "1", Provider: client.Provider(), API: client.API(),
				Model: modelName, Instructions: "Use multiply_numbers to calculate products. Report the result after the tool returns.",
				Settings: llm.Settings{MaxTokens: 1024, ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceAny}},
				Limits:   limits}
			out, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: "limit-live"}, def,
				kernel.Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8, then answer with the result."))}},
				kernel.Bindings{Client: counted, Tools: liveToolset{typed}, Recorder: kernel.NewMemoryRecorder(), Policy: kernel.AllowLocalPolicy{}})
			if err != nil || out.Stop.Kind != kernel.StopLimited || out.Stop.Reason != "model_attempts" ||
				!strings.Contains(out.Turn.AnswerText(), "56") || counted.sends != 2 || runs != 1 {
				t.Fatalf("out=%+v err=%v sends=%d runs=%d", out.Stop, err, counted.sends, runs)
			}
			view, err := kernel.Replay(out.Turn)
			if err != nil || view.ModelAttempts != 2 {
				t.Fatalf("replay=%+v err=%v", view, err)
			}
			for _, step := range out.Turn.Steps {
				if step.Kind == kernel.StepModelIntent && step.Model.FinalAnswerOnly {
					if len(step.Model.Tools) != 0 {
						t.Fatalf("final request advertised tools: %+v", step.Model.Tools)
					}
					t.Logf("model=%s attempts=%d answer=%q", modelName, view.ModelAttempts, out.Turn.AnswerText())
					return
				}
			}
			t.Fatal("missing final-answer intent")
		})
	}
}

func runLiveStructuredOutput(t *testing.T, client llm.Client, modelName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	def := kernel.Definition{ID: "live-structured", Revision: "1", Provider: client.Provider(),
		API: client.API(), Model: modelName, Settings: llm.Settings{MaxTokens: 512},
		Limits: kernel.Limits{ModelAttempts: 1, ToolConcurrency: 1}}
	start := kernel.Start{ID: "start", OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`),
		Input: []llm.Message{llm.UserMessage(llm.Text("Call kernel_submit_output with answer equal to 42. Do not write a final text answer."))}}
	out, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: "live-structured"}, def, start,
		kernel.Bindings{Client: client, Recorder: kernel.NewMemoryRecorder()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != kernel.StopCompleted || out.Output == nil || out.Output.Source != kernel.OutputFromTool || string(out.Output.Value) != `{"answer":42}` {
		t.Fatalf("stop=%+v output=%+v persistence=%s write=%v", out.Stop, out.Output, out.Persistence, out.WriteError)
	}
	view, err := kernel.Replay(out.Turn)
	if err != nil || view.Output == nil || view.ModelAttempts != 1 {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	t.Logf("provider=%s model=%s source=%s steps=%d", client.Provider(), modelName, out.Output.Source, len(out.Turn.Steps))
}

func liveAgentCredentials(t *testing.T, provider string) (string, string) {
	t.Helper()
	if os.Getenv("KERNEL_LIVE_AGENT_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_AGENT_TEST=1 to make paid agent calls")
	}
	key, modelName := os.Getenv(provider+"_API_KEY"), os.Getenv(provider+"_MODEL")
	if key == "" || modelName == "" {
		t.Fatalf("%s_API_KEY and %s_MODEL are required", provider, provider)
	}
	return key, modelName
}

func runLiveAgentTurn(t *testing.T, provider string, client llm.Client, modelName string) {
	t.Helper()
	definition := kernel.Definition{ID: provider + "-live", Revision: "1", Provider: client.Provider(),
		API: client.API(), Model: modelName, Settings: llm.Settings{MaxTokens: 1024},
		Limits: kernel.DefaultLimits()}

	t.Run("streamed_final", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		recorder := kernel.NewMemoryRecorder()
		start := kernel.Start{ID: "start", Input: []llm.Message{
			llm.UserMessage(llm.Text("Reply with exactly one word: indigo.")),
		}}
		out, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: provider + "-live-stream"},
			definition, start, kernel.Bindings{Client: client, Recorder: recorder, Stream: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Persistence != kernel.PersistenceAcknowledged || out.Stop.Kind != kernel.StopCompleted ||
			!strings.Contains(strings.ToLower(out.Turn.AnswerText()), "indigo") {
			t.Fatalf("streamed turn: persistence=%s stop=%+v answer=%q write=%v",
				out.Persistence, out.Stop, out.Turn.AnswerText(), out.WriteError)
		}
		view, err := kernel.Replay(out.Turn)
		if err != nil || view.ModelAttempts != 1 || len(view.Pending) != 0 {
			t.Fatalf("streamed record: view=%+v err=%v", view, err)
		}
		if out.Turn.Steps[2].Model.Result.Message.Source == nil ||
			out.Turn.Steps[2].Model.Result.Message.Source.Requested.Model != modelName {
			t.Fatalf("streamed answer lost %s message provenance", provider)
		}
		t.Logf("streamed turn: attempts=%d steps=%d answer=%q", view.ModelAttempts,
			len(out.Turn.Steps), out.Turn.AnswerText())
	})

	t.Run("local_tool_and_duplicate_start", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		turnID := provider + "-live-tool"
		recorder := kernel.NewMemoryRecorder()
		runs := 0
		typed, err := tool.Func[struct {
			A int `json:"a"`
			B int `json:"b"`
		}]("multiply_numbers", "Multiply two integers and return the product.",
			func(_ context.Context, _ tool.Call, args struct {
				A int `json:"a"`
				B int `json:"b"`
			}) (tool.Outcome, error) {
				saved, err := recorder.Load(turnID)
				if err != nil {
					return tool.Outcome{}, tool.Fatal(err)
				}
				if len(saved.Steps) == 0 || saved.Steps[len(saved.Steps)-1].Kind != kernel.StepToolStarted {
					return tool.Outcome{}, tool.Fatal(errors.New("tool started before its step was acknowledged"))
				}
				if args.A != 7 || args.B != 8 {
					return tool.Outcome{}, tool.Fatal(errors.New(provider + " supplied unexpected tool arguments"))
				}
				runs++
				output := tool.TextOutput(strconv.Itoa(args.A * args.B))
				return tool.Outcome{State: tool.StateSucceeded, Output: &output}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		definition.Instructions = "For multiplication, use multiply_numbers. After the tool result, answer with only the product."
		start := kernel.Start{ID: "start", Input: []llm.Message{
			llm.UserMessage(llm.Text("Use multiply_numbers to calculate 7 times 8. Answer with the result.")),
		}}
		counted := &liveCountingClient{Client: client}
		bindings := kernel.Bindings{Client: counted, Tools: liveToolset{typed}, Recorder: recorder,
			Policy: kernel.AllowLocalPolicy{}}
		out, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: turnID}, definition, start, bindings)
		if err != nil {
			t.Fatal(err)
		}
		if out.Persistence != kernel.PersistenceAcknowledged || out.Stop.Kind != kernel.StopCompleted ||
			!strings.Contains(out.Turn.AnswerText(), "56") || runs != 1 || counted.sends != 2 {
			t.Fatalf("tool turn: persistence=%s stop=%+v answer=%q runs=%d sends=%d write=%v",
				out.Persistence, out.Stop, out.Turn.AnswerText(), runs, counted.sends, out.WriteError)
		}
		view, err := kernel.Replay(out.Turn)
		if err != nil || view.ModelAttempts != 2 || len(view.Tools) != 1 || len(view.Pending) != 0 {
			t.Fatalf("tool record: view=%+v err=%v", view, err)
		}
		for _, observed := range view.Tools {
			if observed.Status != kernel.ToolSettled || observed.Outcome == nil ||
				observed.Outcome.State != tool.StateSucceeded {
				t.Fatalf("tool was not settled: %+v", observed)
			}
		}
		expected := []kernel.StepKind{
			kernel.StepStarted, kernel.StepModelIntent, kernel.StepModelSettled,
			kernel.StepToolDecision, kernel.StepToolStarted, kernel.StepToolSettled,
			kernel.StepModelIntent, kernel.StepModelSettled, kernel.StepStopped,
		}
		if len(out.Turn.Steps) != len(expected) {
			t.Fatalf("tool turn recorded %d steps, want %d", len(out.Turn.Steps), len(expected))
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
		duplicate, err := (kernel.Engine{}).Apply(ctx, kernel.Turn{ID: turnID}, definition, start, bindings)
		if err != nil || duplicate.Stop.Kind != kernel.StopCompleted ||
			len(duplicate.Turn.Steps) != len(out.Turn.Steps) || runs != 1 || counted.sends != 2 {
			t.Fatalf("duplicate start repeated work: stop=%+v err=%v runs=%d sends=%d",
				duplicate.Stop, err, runs, counted.sends)
		}
		t.Logf("tool turn: attempts=%d steps=%d tool_runs=%d answer=%q", view.ModelAttempts,
			len(out.Turn.Steps), runs, out.Turn.AnswerText())
	})
}

type liveToolset []tool.Tool

func (s liveToolset) Tools(context.Context) ([]tool.Tool, error) { return []tool.Tool(s), nil }

type liveCountingClient struct {
	llm.Client
	sends int
}

func (c *liveCountingClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.sends++
	return c.Client.Send(ctx, call)
}
