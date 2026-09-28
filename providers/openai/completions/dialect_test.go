package completions

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// These Chat Completions wire cases are synthesized, not captured from a
// vendor. They pin the parser's safety and parity behavior for compatible
// endpoints without claiming that a particular live model emits these bytes.

func dialectTool(index int, id, name, arguments string) string {
	return fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
		index,
		id,
		name,
		arguments,
	)
}

func dialectComplete(calls ...string) string {
	return `{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
		strings.Join(calls, ",") +
		`]}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
}

func dialectCompleteCall(id, name, arguments string) string {
	return fmt.Sprintf(
		`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`,
		id,
		name,
		arguments,
	)
}

func dialectEvents(chunks ...string) string {
	return "data: " + strings.Join(chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
}

func dialectSend(
	t *testing.T,
	response string,
	stream bool,
	sink ...llm.Sink,
) (llm.Result, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	request, err := client.Prepare(t.Context(), testInput(stream))
	if err != nil {
		t.Fatal(err)
	}
	call := llm.Call{Request: request}
	if len(sink) > 0 {
		call.Sink = sink[0]
	}
	return client.Send(t.Context(), call)
}

func dialectCalls(t *testing.T, result llm.Result) []string {
	t.Helper()
	var got []string
	for _, content := range result.Message.Content {
		if content.ToolCall == nil {
			continue
		}
		got = append(
			got,
			fmt.Sprintf(
				"%s/%s/%s",
				content.ToolCall.ID,
				content.ToolCall.Name,
				content.ToolCall.Args,
			),
		)
	}
	return got
}

func TestDialectInterleavedTwoToolsMatchBlocking(t *testing.T) {
	complete := dialectComplete(
		dialectCompleteCall("call-a", "search", `{"q":"yes"}`),
		dialectCompleteCall("call-b", "lookup", `{"x":1}`),
	)
	stream := dialectEvents(
		dialectTool(0, "", "", `{"q":`),
		dialectTool(1, "call-b", "lookup", `{"x":`),
		dialectTool(1, "", "", `1}`),
		dialectTool(0, "call-a", "search", `"yes"}`),
		`{"id":"chat-1","model":"served","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
	)
	want := []string{
		`call-a/search/{"q":"yes"}`,
		`call-b/lookup/{"x":1}`,
	}
	var results []llm.Result
	for _, tc := range []struct {
		name     string
		response string
		stream   bool
	}{
		{"blocking", complete, false},
		{"stream", stream, true},
	} {
		t.Run("completions/interleaved-two-tools/"+tc.name, func(t *testing.T) {
			result, err := dialectSend(t, tc.response, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			if got := dialectCalls(t, result); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %v, want %v", got, want)
			}
			if result.Served.Model != "served" ||
				result.Stop.Kind != llm.StopToolUse ||
				result.Message.Source == nil ||
				result.Message.Source.API != API ||
				result.Message.Source.ResponseID != "chat-1" ||
				result.Usage == nil ||
				result.Usage.InputTokens != 3 ||
				result.Usage.OutputTokens != 2 {
				t.Fatalf("terminal facts: %+v", result)
			}
			results = append(results, result)
		})
	}
	if !reflect.DeepEqual(results[0].Message.Content, results[1].Message.Content) ||
		!reflect.DeepEqual(results[0].Usage, results[1].Usage) ||
		results[0].Stop != results[1].Stop {
		t.Fatalf("blocking and stream disagree: %+v / %+v", results[0], results[1])
	}
}

func TestDialectMissingIndexResolvesOnlyByExistingID(t *testing.T) {
	stream := dialectEvents(
		dialectTool(1, "call-b", "lookup", `{"x":`),
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call-b","function":{"arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call-b","function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	var argumentIndices []int
	result, err := dialectSend(t, stream, true, func(delta llm.Delta) {
		if delta.Kind == llm.DeltaToolArguments {
			argumentIndices = append(argumentIndices, delta.Index)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`call-b/lookup/{"x":1}`}
	if got := dialectCalls(t, result); !reflect.DeepEqual(got, want) ||
		result.Usage != nil ||
		!reflect.DeepEqual(argumentIndices, []int{1, 1}) {
		t.Fatalf(
			"completions/missing-index-resolved/stream: calls=%v usage=%+v indices=%v",
			got,
			result.Usage,
			argumentIndices,
		)
	}
}

func TestDialectRejectsAmbiguousStreamFragments(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name: "missing-index-no-id-one-active",
			chunks: []string{
				dialectTool(1, "call-b", "lookup", `{"x":`),
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]}}]}`,
			},
			want: "no index or matching ID",
		},
		{
			name: "missing-index-ambiguous",
			chunks: []string{
				dialectTool(0, "call-a", "search", `{"q":`),
				dialectTool(1, "call-b", "lookup", `{"x":`),
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]}}]}`,
			},
			want: "no index or matching ID",
		},
		{
			name: "explicit-zero-conflict",
			chunks: []string{
				dialectTool(1, "call-b", "lookup", `{"x":`),
				dialectTool(0, "call-b", "lookup", `1}`),
			},
			want: "at indices 1 and 0",
		},
		{
			name: "reused-index-conflicting-id",
			chunks: []string{
				dialectTool(0, "call-a", "search", `{"q":`),
				dialectTool(0, "call-b", "search", `"yes"}`),
			},
			want: "changed ID",
		},
		{
			name: "reused-index-conflicting-name",
			chunks: []string{
				dialectTool(0, "call-a", "search", `{"q":`),
				dialectTool(0, "call-a", "lookup", `"yes"}`),
			},
			want: "changed name",
		},
	}
	for _, tc := range tests {
		t.Run("completions/"+tc.name+"/stream", func(t *testing.T) {
			result, err := dialectSend(t, dialectEvents(tc.chunks...), true)
			var providerErr *llm.Error
			if !errors.As(err, &providerErr) ||
				providerErr.Category != llm.ErrorProtocol ||
				!strings.Contains(err.Error(), tc.want) ||
				len(providerErr.Body) == 0 ||
				result.Stop.Kind != llm.StopIncomplete {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if calls := dialectCalls(t, result); len(calls) != 0 {
				t.Fatalf("ambiguous calls became executable: %v", calls)
			}
		})
	}
}

func TestDialectRejectsDuplicateIDsAndFinishedInvalidArguments(t *testing.T) {
	tests := []struct {
		name     string
		complete string
		stream   string
		want     string
	}{
		{
			name: "duplicate-id-across-indices",
			complete: dialectComplete(
				dialectCompleteCall("same", "search", `{}`),
				dialectCompleteCall("same", "lookup", `{}`),
			),
			stream: dialectEvents(
				dialectTool(0, "same", "search", `{}`),
				dialectTool(1, "same", "lookup", `{}`),
			),
			want: "duplicate",
		},
		{
			name:     "finished-invalid-arguments-empty",
			complete: dialectComplete(dialectCompleteCall("call-a", "search", "")),
			stream: dialectEvents(
				dialectTool(0, "call-a", "search", ""),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			),
			want: "invalid",
		},
		{
			name:     "finished-invalid-arguments-malformed",
			complete: dialectComplete(dialectCompleteCall("call-a", "search", "{")),
			stream: dialectEvents(
				dialectTool(0, "call-a", "search", "{"),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			),
			want: "invalid",
		},
	}
	for _, tc := range tests {
		for _, transport := range []struct {
			name     string
			response string
			stream   bool
		}{
			{"blocking", tc.complete, false},
			{"stream", tc.stream, true},
		} {
			t.Run("completions/"+tc.name+"/"+transport.name, func(t *testing.T) {
				result, err := dialectSend(t, transport.response, transport.stream)
				var providerErr *llm.Error
				if !errors.As(err, &providerErr) ||
					providerErr.Category != llm.ErrorProtocol ||
					!strings.Contains(err.Error(), tc.want) ||
					result.Stop.Kind != llm.StopIncomplete {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if calls := dialectCalls(t, result); len(calls) != 0 {
					t.Fatalf("invalid calls became executable: %v", calls)
				}
			})
		}
	}
}

func TestDialectInterruptedArgumentsRemainPartial(t *testing.T) {
	stream := "data: " + dialectTool(0, "call-a", "search", `{"q":`) + "\n\n"
	result, err := dialectSend(t, stream, true)
	if err == nil || result.Stop.Kind != llm.StopIncomplete ||
		len(dialectCalls(t, result)) != 0 ||
		len(result.Message.Content) != 1 ||
		result.Message.Content[0].Opaque == nil ||
		!result.Message.Content[0].Opaque.Partial {
		t.Fatalf("completions/interrupted-arguments/stream: result=%+v err=%v", result, err)
	}
}

func TestDialectInvalidCallReportsItsOwnFragment(t *testing.T) {
	stream := dialectEvents(
		dialectTool(0, "call-a", "search", "{"),
		dialectTool(1, "call-b", "lookup", `{}`),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	result, err := dialectSend(t, stream, true)
	var providerErr *llm.Error
	if !errors.As(err, &providerErr) ||
		providerErr.Category != llm.ErrorProtocol ||
		!strings.Contains(string(providerErr.Body), "call-a") ||
		strings.Contains(string(providerErr.Body), "call-b") ||
		result.Message.Source == nil ||
		result.Message.Source.API != API ||
		len(dialectCalls(t, result)) != 0 {
		t.Fatalf("completions/invalid-call-own-fragment/stream: result=%+v err=%v", result, err)
	}
}

func TestDialectEmptySuccessHasNoMessageSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		stream   bool
	}{
		{
			name:     "blocking",
			response: `{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
		},
		{
			name: "stream",
			response: dialectEvents(
				`{"id":"chat-1","model":"served","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			),
			stream: true,
		},
	} {
		t.Run("completions/empty-success/"+tc.name, func(t *testing.T) {
			result, err := dialectSend(t, tc.response, tc.stream)
			if err != nil || result.Stop.Kind != llm.StopFinished ||
				result.ID != "chat-1" || result.Served.Model != "served" ||
				len(result.Message.Content) != 0 || result.Message.Source != nil {
				t.Fatalf("empty success has unexpected message provenance: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestDialectNoOutputProtocolErrorHasNoMessageSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		stream   bool
	}{
		{
			name:     "blocking",
			response: `{"id":"chat-1","model":"served","choices":[]}`,
		},
		{
			name: "stream",
			response: dialectEvents(
				`{"id":"chat-1","model":"served","choices":[{"index":1,"delta":{}}]}`,
			),
			stream: true,
		},
	} {
		t.Run("completions/no-output-protocol-error/"+tc.name, func(t *testing.T) {
			result, err := dialectSend(t, tc.response, tc.stream)
			var providerErr *llm.Error
			if !errors.As(err, &providerErr) ||
				providerErr.Category != llm.ErrorProtocol ||
				result.Stop.Kind != llm.StopIncomplete ||
				len(result.Message.Content) != 0 || result.Message.Source != nil {
				t.Fatalf("no-output error has unexpected message provenance: result=%+v err=%v", result, err)
			}
		})
	}
}
