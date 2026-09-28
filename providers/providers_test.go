package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

// providerCase holds one provider's constructor and the wire fixtures that
// exercise the same behavior in its dialect, so each contract test runs
// against every provider.
type providerCase struct {
	name string
	new  func(url string) llm.Client

	// complete and stream are equivalent complete and streamed responses.
	complete string
	stream   string

	// schemaKey is the tool definition field that carries the JSON schema.
	schemaKey string

	// privateReplay lists wire values that exist only in replay data and
	// must survive a round trip.
	privateReplay []string

	// partialToolStream ends while tool arguments are incomplete JSON.
	partialToolStream string

	// partialTextStream delivers some text and then stalls.
	partialTextStream string

	// empty is a finished response with no output and no usage.
	empty string

	// invalidToolCall is a finished response with an unusable tool call.
	invalidToolCall string
}

const anthropicStart = `{"type":"message_start","message":{` +
	`"id":"msg-1","type":"message","model":"served","content":[],"stop_reason":null,` +
	`"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":0}}}`

const anthropicComplete = `{"id":"msg-1","type":"message","model":"served","content":[` +
	`{"type":"text","text":"Hello","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"Example","cited_text":"Hello"}]},` +
	`{"type":"thinking","thinking":"Consider","signature":"signed"},` +
	`{"type":"tool_use","id":"call-1","name":"search","input":{"query":"yes"}},` +
	`{"type":"redacted_thinking","data":"encrypted"}` +
	`],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":9}}`

func TestCompleteAndStreamingAgree(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			p, complete := completeCall(t, tc)
			req := preparedRequest(t, p, true)
			var text strings.Builder
			// The sink mutates every delta to prove deltas do not alias the
			// streamed result.
			streamed, err := p.Send(context.Background(), req, func(d llm.Delta) {
				if d.Kind == llm.DeltaText {
					text.WriteString(d.Text)
				}
				if d.Content != nil {
					if d.Content.Block != nil && d.Content.Block.Text != nil {
						d.Content.Block.Text.Text = "corrupt"
					}
					if d.Content.Replay != nil {
						d.Content.Replay.Raw[0] = '['
					}
				}
				if d.Usage != nil {
					d.Usage.OutputTokens = 99999
				}
			})
			if err != nil {
				t.Fatalf("streamed fixture call failed: %v", err)
			}
			complete.Raw = nil
			streamed.Raw = nil
			if !reflect.DeepEqual(jsonValue(t, complete), jsonValue(t, streamed)) {
				t.Fatalf("complete=%s\nstreamed=%s", mustJSON(complete), mustJSON(streamed))
			}
			if text.String() != complete.Message.Text() {
				t.Fatalf("deltas=%q final=%q", text.String(), complete.Message.Text())
			}
		})
	}
}

func TestCompleteResultCarriesIdentityUsageAndValidContent(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			_, complete := completeCall(t, tc)
			if complete.Requested.Model != "requested" || complete.Served.Model == "" || complete.Usage == nil {
				t.Fatal("missing model identity or usage")
			}
			for _, c := range complete.Message.Content {
				if err := c.Validate(); err != nil {
					t.Fatalf("provider returned invalid canonical content: %v", err)
				}
			}
		})
	}
}

func TestReplayUsesEditedContentAndKeepsPrivateData(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			p, complete := completeCall(t, tc)
			// Known content can be edited without retaining stale text or citations.
			for i := range complete.Message.Content {
				c := &complete.Message.Content[i]
				if c.Block != nil && c.Block.Text != nil {
					c.Block.Text.Text = "edited"
					c.Block.Text.Citations = nil
				}
				if c.Reasoning != nil {
					c.Reasoning.Text = "display only"
				}
			}
			r := modelInput()
			r.Messages = append(r.Messages, complete.Message)
			r.Messages = append(r.Messages, toolReplies(complete.Message)...)
			replay, err := p.Prepare(context.Background(), r)
			if err != nil {
				t.Fatalf("prepare edited response for replay: %v", err)
			}
			wire := string(replay.Body())
			if !strings.Contains(wire, "edited") ||
				strings.Contains(wire, "web_search_result_location") ||
				strings.Contains(wire, "display only") {
				t.Fatal("stale canonical data or modified signed reasoning:", wire)
			}
			for _, private := range tc.privateReplay {
				if !strings.Contains(wire, private) {
					t.Fatal("lost private replay data")
				}
			}
		})
	}
}

func TestPreparePreservesToolSchema(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.new("http://unused.invalid")
			r := modelInput()
			schema := json.RawMessage(` {"type":"object","$defs":{"q":{"type":"string"}},` +
				`"properties":{"query":{"$ref":"#/$defs/q"}},"required":["query"],"additionalProperties":false}`)
			r.Tools = []tool.Definition{{Name: "search", Schema: schema}}
			out, err := p.Prepare(context.Background(), r)
			if err != nil {
				t.Fatalf("prepare tool schema: %v", err)
			}
			var wire map[string]any
			if err := json.Unmarshal(out.Body(), &wire); err != nil {
				t.Fatalf("decode prepared tool request: %v", err)
			}
			var original any
			if err := json.Unmarshal(schema, &original); err != nil {
				t.Fatalf("encode tool schema for comparison: %v", err)
			}
			if !reflect.DeepEqual(wire["tools"].([]any)[0].(map[string]any)[tc.schemaKey], original) {
				t.Fatal("schema changed")
			}
		})
	}
}

func TestPreparePreservesMedia(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.new("http://unused.invalid")
			r := modelInput()
			r.Messages[0].Content = append(r.Messages[0].Content,
				llm.Block(content.ImageBlock(content.Source{Data: []byte{1, 2}, MIMEType: "image/png"})),
				llm.Block(content.DocumentBlock(content.Source{URL: "https://example.com/doc.pdf"})),
			)
			result := &llm.ToolResult{
				CallID: "call-1",
				Output: tool.Output{Content: []content.Block{
					content.ImageBlock(content.Source{ID: "file-1", Origin: preparedRequest(t, p).Origin()}),
					content.TextBlock("image"),
				}},
			}
			r.Messages = append(r.Messages, llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "call-1", Name: "image", Args: json.RawMessage(`{}`)}}))
			r.Messages = append(r.Messages, llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.Content{{Kind: llm.ContentToolResult, ToolResult: result}},
			})
			out, err := p.Prepare(context.Background(), r)
			if err != nil {
				t.Fatalf("prepare media content: %v", err)
			}
			wire := string(out.Body())
			if !strings.Contains(wire, "file-1") || !strings.Contains(wire, "AQI=") {
				t.Fatal("lost media")
			}
		})
	}
}

func TestPrepareRejectsUnsupportedContent(t *testing.T) {
	bad := []struct {
		name    string
		content llm.Content
	}{
		{
			name: "audio",
			content: llm.Block(content.Block{
				Kind:  content.KindAudio,
				Audio: &content.Audio{Source: content.Source{URL: "https://example.com/a.wav"}},
			}),
		},
		{
			name: "foreign opaque",
			content: llm.Content{
				Kind: llm.ContentOpaque,
				Opaque: &llm.Opaque{Version: 1,
					Origin: content.Origin{Provider: "foreign", API: "foreign", Model: "requested", Scope: "scope"},
					Type:   "private",
					Unit:   llm.OpaqueUnitItem,
					Raw:    json.RawMessage(`{"type":"private"}`),
				},
			},
		},
	}
	for _, tc := range cases(t) {
		for _, b := range bad {
			t.Run(tc.name+"/"+b.name, func(t *testing.T) {
				p := tc.new("http://unused.invalid")
				r := modelInput()
				r.Messages[0].Content = []llm.Content{b.content}
				_, err := p.Prepare(context.Background(), r)
				var e *llm.Error
				if !errors.As(err, &e) || e.Category != llm.ErrorUnsupported {
					t.Fatalf("unsupported content: %v", err)
				}
			})
		}
	}
}

func TestCallerOwnsEachRetryAttempt(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 {
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"message":"busy"}}`)
					return
				}
				io.WriteString(w, tc.complete)
			})
			p := tc.new(server.URL)
			req := preparedRequest(t, p)
			_, err := p.Send(context.Background(), req, nil)
			var failure *llm.Error
			if !errors.As(err, &failure) || !failure.Retryable || attempts.Load() != 1 {
				t.Fatalf("first attempt was hidden or retry hint lost: %v (%d)", err, attempts.Load())
			}
			// The caller can now record failure and another intent before dispatch.
			_, err = p.Send(context.Background(), req, nil)
			if err != nil || attempts.Load() != 2 {
				t.Fatalf("caller retry failed: error=%v attempts=%d, want 2", err, attempts.Load())
			}
		})
	}
}

func TestHTTPFailuresAreNotRetried(t *testing.T) {
	failures := []struct {
		name    string
		stream  bool
		respond func(http.ResponseWriter)

		// Zero values leave the corresponding error field unchecked.
		category llm.ErrorCategory
		status   int
		code     string
	}{
		{
			name: "quota",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(429)
				io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"no quota"}}`)
			},
			category: llm.ErrorRateLimited,
			status:   429,
			code:     "insufficient_quota",
		},
		{
			name:   "stream",
			stream: true,
			respond: func(w http.ResponseWriter) {
				io.WriteString(w, "data: {}\n\n")
			},
		},
		{
			name: "malformed",
			respond: func(w http.ResponseWriter) {
				io.WriteString(w, `{broken`)
			},
			category: llm.ErrorProtocol,
		},
	}
	for _, tc := range cases(t) {
		for _, f := range failures {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				var attempts atomic.Int32
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					f.respond(w)
				})
				p := tc.new(server.URL)
				var sink llm.Sink
				if f.stream {
					sink = func(llm.Delta) {
					}
				}
				_, err := p.Send(context.Background(), preparedRequest(t, p, f.stream), sink)
				var e *llm.Error
				if !errors.As(err, &e) || attempts.Load() != 1 {
					t.Fatalf("err=%v attempts=%d", err, attempts.Load())
				}
				if f.category != "" && e.Category != f.category {
					t.Fatalf("HTTP failure lost expected classification or details: %+v", e)
				}
				if f.status != 0 && e.Status != f.status {
					t.Fatalf("HTTP failure lost expected classification or details: %+v", e)
				}
				if f.code != "" && e.Code != f.code {
					t.Fatalf("HTTP failure lost expected classification or details: %+v", e)
				}
			})
		}
	}
}

func TestInterruptedToolArgumentsRemainPartial(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				io.WriteString(w, tc.partialToolStream)
			})
			p := tc.new(server.URL)
			result, err := p.Send(context.Background(), preparedRequest(t, p, true), func(llm.Delta) {
			})
			if err == nil ||
				attempts.Load() != 1 ||
				result.Stop.Kind != llm.StopIncomplete ||
				len(result.Message.Content) != 1 {
				t.Fatalf("result=%+v err=%v attempts=%d", result, err, attempts.Load())
			}
			c := result.Message.Content[0]
			if c.ToolCall != nil || c.Opaque == nil || !c.Opaque.Partial {
				t.Fatalf("unfinished call is executable: %+v", c)
			}
			unit := llm.OpaqueUnitItem
			if tc.name == "anthropic" {
				unit = llm.OpaqueUnitContentBlock
			}
			if c.Opaque.Unit != unit || c.Validate() != nil {
				t.Fatal("partial stream data lost its recordable structural unit")
			}
			r := modelInput()
			r.Messages = append(r.Messages, result.Message)
			r.Messages = append(r.Messages, toolReplies(result.Message)...)
			if _, err := p.Prepare(context.Background(), r); err == nil {
				t.Fatal("accepted partial replay")
			}
		})
	}
}

func TestSendHonorsCanceledContext(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				fmt.Fprintln(w, tc.complete)
			})
			p := tc.new(server.URL)
			req := preparedRequest(t, p)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.Send(ctx, req, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call did not return context cancellation: %v", err)
			}
			if attempts.Load() != 0 {
				t.Fatal("unexpected HTTP request")
			}
		})
	}
}

func TestCallRejectsForeignPreparedRequest(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				fmt.Fprintln(w, tc.complete)
			})
			p := tc.new(server.URL)
			foreign, err := llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "foreign", API: "api", Model: "requested", Scope: "scope"}, Input: modelInput(), Body: []byte(`{}`)})
			if err != nil {
				t.Fatalf("construct foreign preparation for rejection check: %v", err)
			}
			if _, err := p.Send(context.Background(), foreign, nil); err == nil {
				t.Fatal("accepted foreign prepared request")
			}
			if attempts.Load() != 0 {
				t.Fatal("unexpected HTTP request")
			}
		})
	}
}

func TestOpenAIReplayPreservesItemIdentityAndPrivateFields(t *testing.T) {
	response := `{"id":"r","object":"response","status":"completed","model":"served","output":[` +
		`{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"Thinking"}],"encrypted_content":"secret"},` +
		`{"type":"message","id":"msg","role":"assistant","status":"completed","phase":"commentary","content":[` +
		`{"type":"output_text","text":"First","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":5}]},` +
		`{"type":"output_text","text":"Second","annotations":[]}]},` +
		`{"type":"function_call","id":"fc","call_id":"call-1","name":"search","arguments":"{\"query\":\"yes\"}","status":"completed"},` +
		`{"type":"future_item","id":"future","private":{"key":"value","counter":9007199254740993}}` +
		`],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},` +
		`"output_tokens":20,"output_tokens_details":{"reasoning_tokens":10}}}`
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, response)
	})
	p := newResponses(t, responses.Config{Provider: "openai", Endpoint: server.URL, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
	result, err := p.Send(context.Background(), preparedRequest(t, p), nil)
	if err != nil {
		t.Fatalf("collect response with private fields: %v", err)
	}
	if len(result.Message.Content) != 5 ||
		result.Stop.Kind != llm.StopToolUse ||
		result.Stop.Raw != "completed" {
		t.Fatalf("response lost output items or tool-use stop: %+v", result)
	}
	u := result.Usage
	if u.InputTokens != 60 || u.CacheReadInputTokens != 40 || u.OutputTokens != 20 || u.ReasoningTokens != 10 {
		t.Fatalf("response lost disjoint usage accounting: %+v", result.Usage)
	}
	result.Message.Content[3].ToolCall.Args = json.RawMessage(`{"query":"edited"}`)
	r := modelInput()
	r.Messages = append(r.Messages, result.Message)
	r.Messages = append(r.Messages, toolReplies(result.Message)...)
	prep, err := p.Prepare(context.Background(), r)
	if err != nil {
		t.Fatalf("prepare private response for replay: %v", err)
	}
	body := string(prep.Body())
	if !strings.Contains(body, `9007199254740993`) {
		t.Fatal("private numeric field lost precision")
	}
	var wire struct{ Input []map[string]any }
	if err := json.Unmarshal(prep.Body(), &wire); err != nil {
		t.Fatalf("decode prepared replay body: %v", err)
	}
	if len(wire.Input) != 6 {
		t.Fatal("message parts were not regrouped", body)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"reasoning encrypted content", wire.Input[1]["encrypted_content"] == "secret"},
		{"message phase", wire.Input[2]["phase"] == "commentary"},
		{"message id", wire.Input[2]["id"] == "msg"},
		{"message parts", len(wire.Input[2]["content"].([]any)) == 2},
		{"function call id", wire.Input[3]["id"] == "fc"},
		{"edited arguments", wire.Input[3]["arguments"] == `{"query":"edited"}`},
		{"unknown item", wire.Input[4]["type"] == "future_item"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Fatalf("lost replay fields (%s): %s", c.name, body)
		}
	}
}

func TestEmptyOutputsAndUnknownUsageAreValid(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.empty)
			})
			p := tc.new(server.URL)
			r, err := p.Send(context.Background(), preparedRequest(t, p), nil)
			if err != nil ||
				r.Usage != nil ||
				r.Stop.Kind != llm.StopFinished ||
				len(r.Message.Content) != 0 {
				t.Fatalf("r=%+v err=%v", r, err)
			}
		})
	}
}

func TestMalformedTerminalResponsesFail(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.invalidToolCall)
			})
			p := tc.new(server.URL)
			_, err := p.Send(context.Background(), preparedRequest(t, p), nil)
			var e *llm.Error
			if !errors.As(err, &e) || e.Category != llm.ErrorProtocol {
				t.Fatalf("malformed response did not return a protocol error: %v", err)
			}
		})
	}
}

func TestCancellationDuringStreamReturnsPartialText(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.partialTextStream)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			p := tc.new(server.URL)
			result, err := p.Send(ctx, preparedRequest(t, p, true), func(d llm.Delta) {
				if d.Kind == llm.DeltaText {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) ||
				result.Message.Text() != "partial" ||
				result.Stop.Kind != llm.StopIncomplete ||
				attempts.Load() != 1 {
				t.Fatalf("result=%+v err=%v attempts=%d", result, err, attempts.Load())
			}
		})
	}
}

func cases(t *testing.T) []providerCase {
	t.Helper()
	sse, err := os.ReadFile("openai/responses/testdata/events-hello.txt")
	if err != nil {
		t.Fatalf("read recorded Responses fixture: %v", err)
	}
	var final struct{ Response json.RawMessage }
	for _, line := range bytes.Split(sse, []byte("\n")) {
		if bytes.Contains(line, []byte(`"type":"response.completed"`)) && bytes.HasPrefix(line, []byte("data: ")) {
			if err := json.Unmarshal(line[6:], &final); err != nil {
				t.Fatalf("decode terminal fixture response: %v", err)
			}
		}
	}
	if len(final.Response) == 0 {
		t.Fatal("fixture has no completed response")
	}
	return []providerCase{
		{
			name: "anthropic",
			new: func(url string) llm.Client {
				return newMessages(t, messages.Config{
					Provider: "anthropic", CapabilityPolicy: llm.CapabilityAdjust,
					APIKey:   "test",
					Endpoint: url,
				})
			},
			complete: anthropicComplete,
			stream: events(
				anthropicStart,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","citations":[]}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com","title":"Example","cited_text":"Hello"}}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"Consider"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"signed"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-1","name":"search","input":{}}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"yes\"}"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"content_block_start","index":3,"content_block":{"type":"redacted_thinking","data":"encrypted"}}`,
				`{"type":"content_block_stop","index":3}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
				`{"type":"message_stop"}`,
			),
			schemaKey:     "input_schema",
			privateReplay: []string{"signed", "encrypted"},
			partialToolStream: events(
				anthropicStart,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"search","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
			),
			partialTextStream: events(
				anthropicStart,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			),
			empty:           `{"type":"message","id":"m","model":"served","stop_reason":"end_turn","content":[]}`,
			invalidToolCall: `{"type":"message","id":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"","name":"search","input":{}}]}`,
		},
		{
			name: "openai",
			new: func(url string) llm.Client {
				return newResponses(t, responses.Config{
					Provider: "openai", CapabilityPolicy: llm.CapabilityAdjust,
					APIKey:   "test",
					Endpoint: url,
				})
			},
			complete:  string(final.Response),
			stream:    string(sse),
			schemaKey: "parameters",
			partialToolStream: events(
				`{"type":"response.created","response":{"object":"response","id":"r","model":"served","status":"in_progress"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"search","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":"}`,
			),
			partialTextStream: events(
				`{"type":"response.created","response":{"object":"response","status":"in_progress","id":"r"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","content":[]}}`,
				`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
				`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`,
			),
			empty:           `{"object":"response","id":"r","model":"served","status":"completed","output":[]}`,
			invalidToolCall: `{"object":"response","status":"completed","output":[{"type":"function_call","call_id":"id","name":"search","arguments":"{"}]}`,
		},
	}
}

// completeCall serves tc's fixtures and returns the provider along with the
// result of a complete (non-streaming) call.
func completeCall(t *testing.T, tc providerCase) (llm.Client, llm.Result) {
	t.Helper()
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode outgoing fixture request: %v", err)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, tc.stream)
		} else {
			io.WriteString(w, tc.complete)
		}
	})
	p := tc.new(server.URL)
	complete, err := p.Send(context.Background(), preparedRequest(t, p), nil)
	if err != nil {
		t.Fatalf("collect complete fixture response: %v", err)
	}
	return p, complete
}

// serve starts a test server that is closed when the test ends.
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// events encodes values as a server-sent event stream.
func events(values ...string) string {
	return "data: " + strings.Join(values, "\n\ndata: ") + "\n\n"
}

func modelInput() llm.Input {
	return llm.Input{
		Model: "requested",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.Content{llm.Text("hello")},
		}},
	}
}

func preparedRequest(t *testing.T, p llm.Client, stream ...bool) llm.Request {
	t.Helper()
	req := modelInput()
	if len(stream) > 0 {
		req.Stream = stream[0]
	}
	r, err := p.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("prepare fixture request: %v", err)
	}
	return r
}

// jsonValue round-trips v through JSON so values compare by wire meaning
// rather than Go representation.
func jsonValue(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode value for semantic comparison: %v", err)
	}
	var result any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("decode value for semantic comparison: %v", err)
	}
	return result
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newMessages(t *testing.T, config messages.Config) *messages.Client {
	t.Helper()
	p, err := messages.New(config)
	if err != nil {
		t.Fatalf("create Messages test client: %v", err)
	}
	return p
}

func newResponses(t *testing.T, config responses.Config) *responses.Client {
	t.Helper()
	p, err := responses.New(config)
	if err != nil {
		t.Fatalf("create Responses test client: %v", err)
	}
	return p
}

// toolReplies supplies explicit fixture outcomes when selecting generated calls
// as completed history. Production code must use its recorded execution results.
func toolReplies(message llm.Message) []llm.Message {
	var parts []llm.Content
	for _, c := range message.Content {
		if c.ToolCall != nil {
			parts = append(parts, llm.Content{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: c.ToolCall.ID, Output: tool.Output{Content: []content.Block{content.TextBlock("done")}}}})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []llm.Message{llm.UserMessage(parts...)}
}
