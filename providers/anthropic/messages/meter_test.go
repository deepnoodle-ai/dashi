package messages

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Fixtures in testdata are real responses captured 2026-09-28 from
// claude-haiku-4-5 (served as claude-haiku-4-5-20251001) with
// web_search_20250305. Encrypted search content and citation indexes are
// trimmed to short strings; everything else is as received.
//
//   - websearch-parallel.json: two parallel searches, max_uses 2.
//   - websearch-sequential.json: two sequential searches, max_uses 3.
//   - websearch-max-uses-exceeded.json: max_uses 1; the second search
//     returned web_search_tool_result_error max_uses_exceeded, and usage
//     counts one search.
//   - websearch-max-uses-exceeded-stream.sse: the same scenario streamed.

func TestProviderToolRequestsFromResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     map[string]int
	}{
		{
			// Captured.
			name:     "parallel searches counted from usage",
			response: readFixture(t, "websearch-parallel.json"),
			want:     map[string]int{llm.MeterWebSearch: 2},
		},
		{
			// Captured.
			name:     "sequential searches counted from usage",
			response: readFixture(t, "websearch-sequential.json"),
			want:     map[string]int{llm.MeterWebSearch: 2},
		},
		{
			// Captured.
			name:     "search refused by max_uses is not counted",
			response: readFixture(t, "websearch-max-uses-exceeded.json"),
			want:     map[string]int{llm.MeterWebSearch: 1},
		},
		{
			// Built from the MCP connector documentation.
			name: "mcp tool use counted from blocks",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}`,
				`{"type":"mcp_tool_use","id":"mcptoolu_1","name":"echo","server_name":"docs","input":{}}`,
				`{"type":"mcp_tool_result","tool_use_id":"mcptoolu_1","is_error":false,"content":[{"type":"text","text":"ok"}]}`,
			),
			want: map[string]int{llm.MeterMCP: 1},
		},
		{
			// Built from documentation; the tool name is invented.
			name: "unknown server tool is unrecognized",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3,"server_tool_use":{"web_search_requests":0}}`,
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"future_tool","input":{}}`,
			),
			want: map[string]int{llm.UnrecognizedMeter("future_tool"): 1},
		},
		{
			// Built from documentation; the counter name is invented.
			name: "unknown usage counter is unrecognized",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3,"server_tool_use":{"web_search_requests":1,"future_requests":3}}`,
			),
			want: map[string]int{
				llm.MeterWebSearch:                       1,
				llm.UnrecognizedMeter("future_requests"): 3,
			},
		},
		{
			// Built from documentation; names invented. A new tool that has
			// both an unknown counter and blocks is counted once.
			name: "unknown tool with its counter is counted once",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3,"server_tool_use":{"future_requests":2}}`,
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"future","input":{}}`,
				`{"type":"server_tool_use","id":"srvtoolu_2","name":"future","input":{}}`,
			),
			want: map[string]int{llm.UnrecognizedMeter("future_requests"): 2},
		},
		{
			// Built from the tool search documentation, which bills it as
			// tokens only.
			name: "tool search is not counted",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3}`,
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"pattern":"weather"}}`,
				`{"type":"server_tool_use","id":"srvtoolu_2","name":"tool_search_tool_bm25","input":{"query":"weather"}}`,
			),
			want: nil,
		},
		{
			// Built from documentation.
			name: "blocks counted when usage has no counters",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3}`,
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"a"}}`,
				`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}`,
				`{"type":"server_tool_use","id":"srvtoolu_2","name":"web_search","input":{"query":"b"}}`,
				`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_2","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}`,
				`{"type":"server_tool_use","id":"srvtoolu_3","name":"web_fetch","input":{"url":"https://example.com"}}`,
			),
			want: map[string]int{
				llm.MeterWebSearch: 1,
				llm.MeterWebFetch:  1,
			},
		},
		{
			// Built from documentation: usage reports search and fetch
			// counters but none for code execution.
			name: "code execution counted from blocks beside counters",
			response: meterResponse(
				`{"input_tokens":5,"output_tokens":3,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":0}}`,
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"a"}}`,
				`{"type":"server_tool_use","id":"srvtoolu_2","name":"web_fetch","input":{"url":"https://example.com"}}`,
				`{"type":"server_tool_use","id":"srvtoolu_3","name":"bash_code_execution","input":{"command":"ls"}}`,
				`{"type":"server_tool_use","id":"srvtoolu_4","name":"text_editor_code_execution","input":{"command":"view"}}`,
			),
			want: map[string]int{
				llm.MeterWebSearch:     1,
				llm.MeterCodeExecution: 2,
			},
		},
	}
	wire := meterCodec()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := wire.decode(
				[]byte(test.response),
				llm.ModelRef{Provider: "anthropic", Model: "model"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil {
				t.Fatal("usage was lost")
			}
			if !maps.Equal(result.Usage.Requests, test.want) {
				t.Fatalf(
					"requests = %v, want %v",
					result.Usage.Requests,
					test.want,
				)
			}
		})
	}
}

func TestProviderToolRequestsWithoutUsage(t *testing.T) {
	response := meterResponse(
		"",
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"a"}}`,
	)
	result, err := meterCodec().decode(
		[]byte(response),
		llm.ModelRef{Provider: "anthropic", Model: "model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage != nil {
		t.Fatalf("usage invented for a response without one: %+v", result.Usage)
	}
}

func TestProviderToolRequestsStreamMatchBlocking(t *testing.T) {
	wire := meterCodec()
	requested := llm.ModelRef{Provider: "anthropic", Model: "model"}
	var live []map[string]int
	streaming, err := wire.readStream(
		io.NopCloser(strings.NewReader(
			readFixture(t, "websearch-max-uses-exceeded-stream.sse"),
		)),
		llm.Result{Requested: requested},
		func(d llm.Delta) {
			if d.Kind == llm.DeltaUsage {
				live = append(live, d.Usage.Requests)
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{llm.MeterWebSearch: 1}
	if streaming.Usage == nil ||
		!maps.Equal(streaming.Usage.Requests, want) {
		t.Fatalf("streamed usage = %+v, want requests %v", streaming.Usage, want)
	}
	if !streaming.Usage.RequestsComplete {
		t.Fatal("a complete stream's counts are not marked final")
	}
	if streaming.Usage.InputTokens != 10982 ||
		streaming.Usage.OutputTokens != 160 {
		t.Fatalf("streamed tokens lost: %+v", streaming.Usage)
	}
	if len(live) == 0 || !maps.Equal(live[len(live)-1], want) {
		t.Fatalf("live usage requests = %v, want %v", live, want)
	}

	// The streamed Raw is the complete response the stream reassembled;
	// decoding it as a blocking response must count the same.
	blocking, err := wire.decode(streaming.Raw, requested)
	if err != nil {
		t.Fatal(err)
	}
	if blocking.Usage == nil ||
		!maps.Equal(blocking.Usage.Requests, want) {
		t.Fatalf("blocking usage = %+v, want requests %v", blocking.Usage, want)
	}
	var raw struct {
		Usage struct {
			Requests      json.RawMessage `json:"requests"`
			ServerToolUse struct {
				WebSearchRequests int `json:"web_search_requests"`
			} `json:"server_tool_use"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(streaming.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Usage.Requests != nil ||
		raw.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Fatalf("streamed Raw usage is not the native object: %+v", raw.Usage)
	}
}

// meterCodec returns a codec for decoding fixtures offline.
func meterCodec() codec {
	return codec{
		origin: content.Origin{
			Provider: "anthropic",
			API:      API,
			Model:    "model",
			Scope:    "scope",
		},
	}
}

// meterResponse builds a complete Messages response from a usage object,
// omitted when empty, and content blocks followed by a closing text block.
func meterResponse(usage string, blocks ...string) string {
	blocks = append(blocks, `{"type":"text","text":"Done."}`)
	response := `{"id":"msg_1","type":"message","model":"model",` +
		`"stop_reason":"end_turn","content":[` +
		strings.Join(blocks, ",") + `]`
	if usage != "" {
		response += `,"usage":` + usage
	}
	return response + `}`
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A stream cut off before message_delta has no final server_tool_use
// counters. The visible block is counted, but the counts are not final, so
// the runner treats the call's provider tool billing as possibly incomplete.
func TestProviderToolRequestsFromCutStreamAreNotFinal(t *testing.T) {
	fixture := readFixture(t, "websearch-max-uses-exceeded-stream.sse")
	marker := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	end := strings.Index(fixture, marker)
	if end < 0 {
		t.Fatal("fixture lacks the first block's stop event")
	}
	wire := meterCodec()
	result, _ := wire.readStream(
		io.NopCloser(strings.NewReader(fixture[:end+len(marker)])),
		llm.Result{Requested: llm.ModelRef{Provider: "anthropic", Model: "model"}},
		func(llm.Delta) {},
	)
	if result.Usage == nil ||
		result.Usage.Requests[llm.MeterWebSearch] != 1 {
		t.Fatalf("usage = %+v, want one inferred search", result.Usage)
	}
	if result.Usage.RequestsComplete {
		t.Fatal("counts inferred from a cut stream are marked final")
	}
}
