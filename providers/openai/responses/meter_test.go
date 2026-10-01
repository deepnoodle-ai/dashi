package responses

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Fixtures in testdata are real responses captured 2026-09-28. Encrypted
// reasoning is replaced with "trimmed", source and annotation lists are cut to
// two entries, and long strings are shortened; everything else is as
// received.
//
//   - openai-web-search.json: gpt-5-nano (served gpt-5-nano-2025-08-07),
//     web_search. Two search items and one open_page item; tool_usage
//     reports two searches.
//   - openai-web-search-preview.json: gpt-5-nano, web_search_preview only.
//   - openai-web-search-both-stream.txt: gpt-5-nano, streamed, with both
//     web_search and web_search_preview declared. The response echoes only
//     web_search_preview.
//   - openai-code-mcp.json: gpt-5-nano, code_interpreter and an MCP server
//     (mcp.deepwiki.com). One mcp_list_tools, two code_interpreter_call, and
//     one mcp_call item.
//   - grok-web-x-search.json: grok-4.20-0309-non-reasoning, web_search and
//     x_search. X search arrives as a custom_tool_call item; usage carries
//     server_side_tool_usage_details and cost_in_usd_ticks.
//   - grok-web-search-stream.txt: grok-4.20-0309-non-reasoning, streamed,
//     web_search.
//   - meta-web-search.json: muse-spark-1.3, web_search. Four search items
//     and two open_page items.

func TestProviderToolRequestsFromCapturedResponses(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		declared []string
		fixture  string
		want     map[string]int
	}{
		{
			name:     "openai searches exclude open_page",
			provider: "openai",
			declared: []string{"web_search"},
			fixture:  "openai-web-search.json",
			want:     map[string]int{llm.MeterWebSearch: 2},
		},
		{
			name:     "openai preview declared",
			provider: "openai",
			declared: []string{"web_search_preview"},
			fixture:  "openai-web-search-preview.json",
			want:     map[string]int{llm.MeterWebSearchPreview: 1},
		},
		{
			name:     "openai preview read from the tools echo",
			provider: "openai",
			fixture:  "openai-web-search-preview.json",
			want:     map[string]int{llm.MeterWebSearchPreview: 1},
		},
		{
			name:     "openai code and mcp without list tools",
			provider: "openai",
			declared: []string{"code_interpreter", "mcp"},
			fixture:  "openai-code-mcp.json",
			want: map[string]int{
				llm.MeterCodeExecution: 2,
				llm.MeterMCP:           1,
			},
		},
		{
			name:     "grok counters",
			provider: "grok",
			declared: []string{"web_search", "x_search"},
			fixture:  "grok-web-x-search.json",
			want: map[string]int{
				llm.MeterWebSearch:   1,
				llm.MeterXSearchPost: 1,
			},
		},
		{
			name:     "meta queries exclude open_page",
			provider: "meta",
			declared: []string{"web_search"},
			fixture:  "meta-web-search.json",
			want:     map[string]int{llm.MeterWebSearch: 4},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire := meterCodec(tc.provider, tc.declared)
			result, err := wire.decode(readFixture(t, tc.fixture), llm.ModelRef{})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result.Usage == nil {
				t.Fatal("reported usage was dropped")
			}
			if !result.Usage.RequestsComplete {
				t.Fatal("a terminal response's counts are not marked final")
			}
			if !maps.Equal(result.Usage.Requests, tc.want) {
				t.Fatalf(
					"requests = %v, want %v",
					result.Usage.Requests,
					tc.want,
				)
			}
		})
	}
}

func TestProviderToolRequestsFromItems(t *testing.T) {
	const usage = `"usage":{"input_tokens":1,"output_tokens":1}`
	tests := []struct {
		name     string
		provider string
		declared []string
		body     string
		want     map[string]int
	}{
		{
			name:     "search actions only",
			provider: "openai",
			declared: []string{"web_search"},
			body: `"output":[
				{"id":"item_1","type":"web_search_call","action":{"type":"search","query":"a"}},
				{"id":"item_2","type":"web_search_call","action":{"type":"open_page","url":"https://example.com"}},
				{"id":"item_3","type":"web_search_call","action":{"type":"find_in_page","pattern":"x","url":"https://example.com"}},
				{"id":"item_4","type":"web_search_call","status":"completed"}
			],` + usage,
			want: map[string]int{llm.MeterWebSearch: 2},
		},
		{
			name:     "preview only declared",
			provider: "openai",
			declared: []string{"web_search_preview"},
			body: `"output":[
				{"id":"item_5","type":"web_search_call","action":{"type":"search","query":"a"}}
			],` + usage,
			want: map[string]int{llm.MeterWebSearchPreview: 1},
		},
		{
			name:     "both declared count as preview",
			provider: "openai",
			declared: []string{"web_search", "web_search_preview"},
			body: `"output":[
				{"id":"item_6","type":"web_search_call","action":{"type":"search","query":"a"}}
			],` + usage,
			want: map[string]int{llm.MeterWebSearchPreview: 1},
		},
		{
			name:     "tool_usage counter wins over items",
			provider: "openai",
			declared: []string{"web_search"},
			body: `"output":[
				{"id":"item_7","type":"web_search_call","action":{"type":"search","query":"a"}}
			],"tool_usage":{"web_search":{"num_requests":3}},` + usage,
			want: map[string]int{llm.MeterWebSearch: 3},
		},
		{
			name:     "zero tool_usage counter wins over items",
			provider: "openai",
			declared: []string{"web_search"},
			body: `"output":[
				{"id":"item_8","type":"web_search_call","action":{"type":"search","query":"a"}}
			],"tool_usage":{"web_search":{"num_requests":0}},` + usage,
			want: nil,
		},
		{
			name:     "each provider tool item type",
			provider: "openai",
			body: `"output":[
				{"id":"item_9","type":"file_search_call","queries":["a"]},
				{"id":"item_10","type":"code_interpreter_call","code":"1"},
				{"id":"item_11","type":"image_generation_call","result":""},
				{"id":"item_12","type":"mcp_call","name":"read","server_label":"docs","arguments":"{}"},
				{"id":"item_13","type":"mcp_list_tools","server_label":"docs","tools":[]},
				{"type":"mcp_approval_request","id":"a1","name":"read","server_label":"docs","arguments":"{}"},
				{"id":"item_14","type":"x_search_call","status":"completed"}
			],` + usage,
			want: map[string]int{
				llm.MeterFileSearch:                    1,
				llm.MeterCodeExecution:                 1,
				llm.MeterImageGeneration:               1,
				llm.MeterMCP:                           1,
				llm.UnrecognizedMeter("x_search_call"): 1,
			},
		},
		{
			name:     "xai counters win over items",
			provider: "grok",
			body: `"output":[
				{"id":"item_15","type":"web_search_call","action":{"type":"search","query":"a"}},
				{"id":"item_16","type":"x_search_call","status":"completed"},
				{"id":"item_17","type":"code_interpreter_call","code":"1"},
				{"id":"item_18","type":"image_generation_call","result":""}
			],"usage":{"input_tokens":1,"output_tokens":1,
				"server_side_tool_usage_details":{
					"web_search_calls":2,"x_search_calls":3,
					"x_posts_fetched":4,"x_users_fetched":1,
					"code_interpreter_calls":0,"file_search_calls":1,
					"mcp_calls":1,"image_generation_calls":1,
					"document_search_calls":2,"future_calls":1}}`,
			want: map[string]int{
				llm.MeterWebSearch:                             2,
				llm.MeterXSearchPost:                           4,
				llm.MeterXSearchProfile:                        1,
				llm.MeterFileSearch:                            1,
				llm.MeterMCP:                                   1,
				llm.MeterImageGeneration:                       1,
				llm.UnrecognizedMeter("document_search_calls"): 2,
				llm.UnrecognizedMeter("future_calls"):          1,
			},
		},
		{
			name:     "xai items without counters",
			provider: "grok",
			body: `"output":[
				{"id":"item_19","type":"web_search_call","action":{"type":"search","query":"a"}}
			],` + usage,
			want: map[string]int{llm.MeterWebSearch: 1},
		},
		{
			name:     "meta counts queries",
			provider: "meta",
			body: `"output":[
				{"id":"item_20","type":"web_search_call","action":{"type":"search","queries":["a","b","c"],"query":"a"}},
				{"id":"item_21","type":"web_search_call","action":{"type":"search","query":"d"}},
				{"id":"item_22","type":"web_search_call","action":{"type":"search"}},
				{"id":"item_23","type":"web_search_call","status":"completed"},
				{"id":"item_24","type":"web_search_call","action":{"type":"open_page","url":"https://example.com"}}
			],` + usage,
			want: map[string]int{llm.MeterWebSearch: 6},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire := meterCodec(tc.provider, tc.declared)
			raw := `{"object":"response","status":"completed",` + tc.body + `}`
			result, err := wire.decode([]byte(raw), llm.ModelRef{})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result.Usage == nil {
				t.Fatal("reported usage was dropped")
			}
			if !maps.Equal(result.Usage.Requests, tc.want) {
				t.Fatalf(
					"requests = %v, want %v",
					result.Usage.Requests,
					tc.want,
				)
			}
		})
	}
}

func TestProviderToolRequestsNeedReportedUsage(t *testing.T) {
	wire := meterCodec("openai", []string{"web_search"})
	raw := `{"object":"response","status":"completed","output":[
		{"id":"item_25","type":"web_search_call","action":{"type":"search","query":"a"}}
	]}`
	result, err := wire.decode([]byte(raw), llm.ModelRef{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Usage != nil {
		t.Fatal("usage was invented for a response that reported none")
	}
}

func TestReportedCostFromXAITicks(t *testing.T) {
	wire := meterCodec("grok", nil)
	result, err := wire.decode(
		readFixture(t, "grok-web-x-search.json"),
		llm.ModelRef{},
	)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cost := result.Cost
	if cost == nil {
		t.Fatal("reported cost was dropped")
	}
	// The fixture reports 166303000 ticks, which is $0.0166303.
	if got := cost.Total.Nanos(); got != 16_630_300 {
		t.Fatalf("total = %d nanos, want 16630300", got)
	}
	if cost.Currency != "USD" ||
		cost.Source != llm.CostSourceProviderReported ||
		!cost.BreakdownUnavailable {
		t.Fatalf("cost metadata = %+v", cost)
	}
	if err := cost.Validate(); err != nil {
		t.Fatalf("reported cost is invalid: %v", err)
	}
}

func TestReportedCostNeedsTicks(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":1,"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":1,"cost_in_usd_ticks":-1}`,
		`{"input_tokens":1,"output_tokens":1,"cost_in_nano_usd":5}`,
	} {
		if cost := reportedCost([]byte(usage)); cost != nil {
			t.Fatalf("cost %+v invented from %s", cost, usage)
		}
	}
}

func TestTicksToNanosRoundsHalfToEven(t *testing.T) {
	for _, tc := range []struct {
		ticks int64
		want  int64
	}{
		{ticks: 0, want: 0},
		{ticks: 4, want: 0},
		{ticks: 5, want: 0},
		{ticks: 6, want: 1},
		{ticks: 15, want: 2},
		{ticks: 25, want: 2},
		{ticks: 35, want: 4},
		{ticks: 166_303_000, want: 16_630_300},
	} {
		if got := ticksToNanos(tc.ticks); got != tc.want {
			t.Fatalf("ticksToNanos(%d) = %d, want %d", tc.ticks, got, tc.want)
		}
	}
}

func TestStreamedProviderToolRequestsMatchBlocking(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		declared []string
		fixture  string
		want     map[string]int
	}{
		{
			name:     "openai both declared",
			provider: "openai",
			declared: []string{"web_search", "web_search_preview"},
			fixture:  "openai-web-search-both-stream.txt",
			want:     map[string]int{llm.MeterWebSearchPreview: 1},
		},
		{
			name:     "grok counters",
			provider: "grok",
			declared: []string{"web_search"},
			fixture:  "grok-web-search-stream.txt",
			want:     map[string]int{llm.MeterWebSearch: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire := meterCodec(tc.provider, tc.declared)
			stream := string(readFixture(t, tc.fixture))
			streamed, err := wire.readStream(
				io.NopCloser(strings.NewReader(stream)),
				llm.Result{},
				nil,
			)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			blocking, err := wire.decode(
				[]byte(terminalResponse(t, stream)),
				llm.ModelRef{},
			)
			if err != nil {
				t.Fatalf("decode terminal response: %v", err)
			}
			if streamed.Usage == nil || blocking.Usage == nil {
				t.Fatal("reported usage was dropped")
			}
			if !maps.Equal(streamed.Usage.Requests, tc.want) {
				t.Fatalf(
					"streamed requests = %v, want %v",
					streamed.Usage.Requests,
					tc.want,
				)
			}
			if !maps.Equal(streamed.Usage.Requests, blocking.Usage.Requests) {
				t.Fatal("streamed and blocking requests differ")
			}
			if !reflect.DeepEqual(streamed.Cost, blocking.Cost) {
				t.Fatal("streamed and blocking costs differ")
			}
		})
	}
}

func TestSendMetersSearchByDeclaredTool(t *testing.T) {
	// The response omits its tools echo, so only the request's declaration
	// can select the preview meter.
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(w, `{
			"object":"response","status":"completed","model":"test-model",
			"output":[{"id":"ws_1","type":"web_search_call",
				"action":{"type":"search","query":"a"}}],
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer server.Close()
	client, err := New(Config{
		Provider:         "openai",
		APIKey:           "test",
		Endpoint:         server.URL,
		Client:           server.Client(),
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No constructor offers the legacy preview type, so build it directly.
	preview := newProviderTool(
		"web_search",
		"web_search_preview",
		WebSearchToolOptions{},
	).Definition
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Search"))},
			Tools:    []tool.Definition{preview},
		},
	)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := client.Send(t.Context(), llm.Call{Request: request})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	want := map[string]int{llm.MeterWebSearchPreview: 1}
	if result.Usage == nil || !maps.Equal(result.Usage.Requests, want) {
		t.Fatalf("usage = %+v, want requests %v", result.Usage, want)
	}
}

func meterCodec(provider string, declared []string) codec {
	return codec{
		origin: content.Origin{
			Provider: provider,
			API:      API,
			Model:    "test-model",
			Scope:    "test-scope",
		},
		providerToolTypes: declared,
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// terminalResponse returns the response object of a stream's
// response.completed event, which the blocking path would have received.
func terminalResponse(t *testing.T, stream string) string {
	t.Helper()
	for line := range strings.Lines(stream) {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(data), &event) == nil &&
			event.Type == "response.completed" {
			return string(event.Response)
		}
	}
	t.Fatal("stream has no response.completed event")
	return ""
}
