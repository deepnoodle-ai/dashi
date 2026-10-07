package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

func TestAnthropicRunsEmptySchemaMCPTool(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "completed"
		if !complete {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			srv, log := httpServer(t, nil)
			ts := open(t, map[string]Server{"test": srv}, Options{})
			definition := find(t, ts, "mcp__test__structured").Definition
			if string(definition.Schema) != `{"type":"object"}` {
				t.Fatalf("fixture lost its empty input schema: %s", definition.Schema)
			}
			var calls atomic.Int32
			bodies := make(chan []byte, 2)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				select {
				case bodies <- body:
				default:
					t.Error("runner made an unexpected extra model request")
					http.Error(w, "unexpected request", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					stream := `data: {"type":"message_start","message":{"id":"m1","type":"message","model":"fixture","content":[]}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-empty","name":"mcp__test__structured","input":{},"caller":{"type":"direct"}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}

`
					if complete {
						stream += `data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`
					}
					_, _ = io.WriteString(w, stream)
					return
				}
				_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"m2","type":"message","model":"fixture","content":[]}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"finished"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

data: {"type":"message_stop"}

`)
			}))
			defer model.Close()
			client, err := messages.New(messages.Config{
				Provider: "anthropic",
				Endpoint: model.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := (dashi.Runner{
				Client:   client,
				Tools:    ts,
				Observer: func(dashi.Event) {},
			}).Run(t.Context(), dashi.NewTurn("empty-mcp"), dashi.Start{
				Agent: dashi.Agent{
					Model:  "fixture",
					Tools:  []string{definition.Name},
					Limits: dashi.Limits{ModelCalls: 3},
				},
				Input: []llm.Message{llm.UserMessage(llm.Text("Run the tool."))},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !complete {
				if out.Stop.Kind == dashi.StopCompleted || calls.Load() != 1 {
					t.Fatalf("interrupted model result continued: %+v", out.Stop)
				}
				if strings.Contains(readLog(log), "call structured") {
					t.Fatal("interrupted arguments executed the MCP tool")
				}
				return
			}
			if out.Stop.Kind != dashi.StopCompleted || out.Reply != "finished" {
				t.Fatalf("empty MCP tool did not finish the turn: %+v", out.Stop)
			}
			if strings.Count(readLog(log), "call structured\n") != 1 {
				t.Fatal("empty MCP tool did not execute exactly once")
			}
			view, err := out.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			call := view.Tools["call-empty"]
			if call.Status != dashi.ToolFinished || string(call.Call.Args) != `{}` {
				t.Fatalf("recorded tool call lost empty arguments: %+v", call)
			}
			if call.Result().IsError || len(call.Result().Output.Content) == 0 {
				t.Fatal("recorded tool call lost its successful result")
			}
			if calls.Load() != 2 {
				t.Fatalf("model calls = %d, want 2", calls.Load())
			}
			first, second := <-bodies, <-bodies
			var request struct{ Stream bool }
			if json.Unmarshal(first, &request) != nil || !request.Stream {
				t.Fatal("runner did not exercise Anthropic streaming")
			}
			if !strings.Contains(string(second), `"tool_use_id":"call-empty"`) ||
				!strings.Contains(string(second), "done") {
				t.Fatal("continuation lost the correlated MCP result")
			}
		})
	}
}
