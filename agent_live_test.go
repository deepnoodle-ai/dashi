//go:build live

package kernel_test

import (
	"context"
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
	client, err := gemini.New(gemini.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "gemini", client, modelName)
}

func TestLiveOpenAIAgentTurn(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "OPENAI")
	client, err := responses.New(responses.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "openai", client, modelName)
}

func TestLiveAnthropicAgentTurn(t *testing.T) {
	key, modelName := liveAgentCredentials(t, "ANTHROPIC")
	client, err := messages.New(messages.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	runLiveAgentTurn(t, "anthropic", client, modelName)
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
					return tool.Outcome{}, err
				}
				if len(saved.Steps) == 0 || saved.Steps[len(saved.Steps)-1].Kind != kernel.StepToolStarted {
					return tool.Outcome{}, errors.New("tool started before its step was acknowledged")
				}
				if args.A != 7 || args.B != 8 {
					return tool.Outcome{}, errors.New(provider + " supplied unexpected tool arguments")
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

func (c *liveCountingClient) Send(ctx context.Context, request llm.Request, sink llm.Sink) (llm.Result, error) {
	c.sends++
	return c.Client.Send(ctx, request, sink)
}
