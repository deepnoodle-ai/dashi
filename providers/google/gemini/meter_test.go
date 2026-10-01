package gemini

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// No Gemini API key was available when these fixtures were written, so every
// fixture below is built from documentation, not captured. The shapes follow
// the GenerateContentResponse, GroundingMetadata, and UrlContextMetadata
// reference (https://ai.google.dev/api/generate-content), the guides at
// https://ai.google.dev/gemini-api/docs/google-search, url-context,
// code-execution, and file-search, and the field names of the
// google.golang.org/genai v1.71.0 types.

// Built from documentation (https://ai.google.dev/gemini-api/docs/google-search),
// not captured. Gemini 2.5 style: billed per grounded prompt.
const groundedGemini25 = `{
  "responseId": "resp-25",
  "modelVersion": "gemini-2.5-flash",
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "Spain won Euro 2024, beating England 2-1."}]},
    "finishReason": "STOP",
    "groundingMetadata": {
      "webSearchQueries": ["UEFA Euro 2024 winner", "Spain vs England Euro 2024 final score"],
      "searchEntryPoint": {"renderedContent": "<style></style><div></div>"},
      "groundingChunks": [
        {"web": {"uri": "https://vertexaisearch.cloud.google.com/grounding-api-redirect/a", "title": "uefa.com"}},
        {"web": {"uri": "https://vertexaisearch.cloud.google.com/grounding-api-redirect/b", "title": "aljazeera.com"}}
      ],
      "groundingSupports": [{
        "segment": {"startIndex": 0, "endIndex": 41, "text": "Spain won Euro 2024, beating England 2-1."},
        "groundingChunkIndices": [0, 1]
      }]
    }
  }],
  "usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 14, "toolUsePromptTokenCount": 180, "totalTokenCount": 206}
}`

// Built from documentation (https://ai.google.dev/gemini-api/docs/google-search),
// not captured. Gemini 3 style: billed per query, with a signed thought part.
// The repeated query counts twice because the documentation does not say
// repeats are merged; the empty query is ignored, as the documentation says.
const groundedGemini3 = `{
  "responseId": "resp-3",
  "modelVersion": "gemini-3-flash-preview",
  "candidates": [{
    "content": {"role": "model", "parts": [
      {"text": "Spain won.", "thoughtSignature": "c2ln"}
    ]},
    "finishReason": "STOP",
    "groundingMetadata": {
      "webSearchQueries": ["euro 2024 winner", "euro 2024 winner", "", "euro 2024 final score"],
      "groundingChunks": [{"web": {"uri": "https://vertexaisearch.cloud.google.com/grounding-api-redirect/c", "title": "uefa.com"}}]
    }
  }],
  "usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 3, "thoughtsTokenCount": 40, "toolUsePromptTokenCount": 90, "totalTokenCount": 143}
}`

// Built from documentation (https://ai.google.dev/api/generate-content,
// UrlContextMetadata), not captured.
const urlContextResponse = `{
  "responseId": "resp-url",
  "modelVersion": "gemini-2.5-flash",
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "Both pages describe recipes."}]},
    "finishReason": "STOP",
    "urlContextMetadata": {"urlMetadata": [
      {"retrievedUrl": "https://example.com/a", "urlRetrievalStatus": "URL_RETRIEVAL_STATUS_SUCCESS"},
      {"retrievedUrl": "https://example.com/b", "urlRetrievalStatus": "URL_RETRIEVAL_STATUS_ERROR"}
    ]}
  }],
  "usageMetadata": {"promptTokenCount": 20, "candidatesTokenCount": 5, "toolUsePromptTokenCount": 800, "totalTokenCount": 825}
}`

// Built from documentation (https://ai.google.dev/gemini-api/docs/code-execution),
// not captured.
const codeExecutionResponse = `{
  "responseId": "resp-code",
  "modelVersion": "gemini-2.5-flash",
  "candidates": [{
    "content": {"role": "model", "parts": [
      {"text": "I'll compute it."},
      {"executableCode": {"language": "PYTHON", "code": "print(sum(range(10)))"}},
      {"codeExecutionResult": {"outcome": "OUTCOME_OK", "output": "45\n"}},
      {"executableCode": {"language": "PYTHON", "code": "print(2**10)"}},
      {"codeExecutionResult": {"outcome": "OUTCOME_OK", "output": "1024\n"}},
      {"text": "45 and 1024."}
    ]},
    "finishReason": "STOP"
  }],
  "usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 30, "totalTokenCount": 38}
}`

// Built from documentation (https://ai.google.dev/gemini-api/docs/file-search
// and the genai GroundingChunkRetrievedContext type), not captured.
const fileSearchResponse = `{
  "responseId": "resp-files",
  "modelVersion": "gemini-2.5-flash",
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "The policy allows 20 days."}]},
    "finishReason": "STOP",
    "groundingMetadata": {
      "groundingChunks": [
        {"retrievedContext": {"title": "handbook.pdf", "text": "20 days", "fileSearchStore": "fileSearchStores/store-1"}},
        {"retrievedContext": {"title": "handbook.pdf", "text": "carry over", "fileSearchStore": "fileSearchStores/store-1"}}
      ]
    }
  }],
  "usageMetadata": {"promptTokenCount": 30, "candidatesTokenCount": 7, "totalTokenCount": 37}
}`

// Built from documentation (the genai GroundingChunk maps field), not
// captured. Maps grounding is hosted work this adapter does not price.
const mapsResponse = `{
  "responseId": "resp-maps",
  "modelVersion": "gemini-2.5-flash",
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "Try the cafe on Main St."}]},
    "finishReason": "STOP",
    "groundingMetadata": {"groundingChunks": [{"maps": {"title": "Cafe", "placeId": "p1"}}]}
  }],
  "usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 6, "totalTokenCount": 15}
}`

// Built from documentation (https://ai.google.dev/gemini-api/docs/google-search),
// not captured. Grounded, but the provider sent no usageMetadata.
const groundedWithoutUsage = `{
  "responseId": "resp-nousage",
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "Spain."}]},
    "finishReason": "STOP",
    "groundingMetadata": {"webSearchQueries": ["euro 2024 winner"]}
  }]
}`

func TestDecodeCountsProviderToolMeters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    map[string]int
		noUsage bool
	}{
		{
			name: "gemini 2.5 google search",
			raw:  groundedGemini25,
			want: map[string]int{
				llm.MeterWebSearch:      2,
				llm.MeterGroundedPrompt: 1,
			},
		},
		{
			name: "gemini 3 google search",
			raw:  groundedGemini3,
			want: map[string]int{
				llm.MeterWebSearch:      3,
				llm.MeterGroundedPrompt: 1,
			},
		},
		{
			name: "web chunks without queries",
			raw: strings.Replace(
				groundedGemini25,
				`"webSearchQueries": ["UEFA Euro 2024 winner", "Spain vs England Euro 2024 final score"],`,
				"",
				1,
			),
			want: map[string]int{
				llm.MeterWebSearch:      1,
				llm.MeterGroundedPrompt: 1,
			},
		},
		{
			name: "url context",
			raw:  urlContextResponse,
			want: map[string]int{llm.MeterURLContext: 2},
		},
		{
			name: "code execution",
			raw:  codeExecutionResponse,
			want: map[string]int{llm.MeterCodeExecution: 2},
		},
		{
			name: "file search",
			raw:  fileSearchResponse,
			want: map[string]int{llm.MeterFileSearch: 1},
		},
		{
			name: "unrecognized grounding source",
			raw:  mapsResponse,
			want: map[string]int{
				llm.UnrecognizedMeter("groundingChunk.maps"): 1,
			},
		},
		{
			name:    "grounding without usage",
			raw:     groundedWithoutUsage,
			noUsage: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := codec{origin: meterOrigin(t)}
			result, err := wire.decode([]byte(tc.raw), llm.ModelRef{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.noUsage {
				if result.Usage != nil {
					t.Fatalf("invented usage: %+v", result.Usage)
				}
				return
			}
			if result.Usage == nil ||
				!maps.Equal(result.Usage.Requests, tc.want) {
				t.Fatalf("requests = %+v, want %v", result.Usage, tc.want)
			}
		})
	}
}

// Built from documentation (https://ai.google.dev/gemini-api/docs/google-search),
// not captured. The query list repeats across chunks and grows by one web
// chunk; usageMetadata rides on every chunk, as Gemini streams send it.
var streamRepeatedQueries = []string{
	`{"responseId":"s1","modelVersion":"gemini-3-flash-preview","candidates":[{"content":{"role":"model","parts":[{"text":"Spain "}]}}],"usageMetadata":{"promptTokenCount":10,"totalTokenCount":10}}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"won."}]},"groundingMetadata":{"webSearchQueries":["euro 2024 winner","euro 2024 final"],"groundingChunks":[{"web":{"uri":"https://a","title":"a"}}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`,
	`{"candidates":[{"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["euro 2024 winner","euro 2024 final"],"groundingChunks":[{"web":{"uri":"https://a","title":"a"}},{"web":{"uri":"https://b","title":"b"}}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"toolUsePromptTokenCount":50,"totalTokenCount":63}}`,
}

// Built from documentation (https://ai.google.dev/gemini-api/docs/google-search
// and code-execution), not captured. Usage arrives before the grounding
// metadata, and code runs arrive in separate chunks.
var streamLateGrounding = []string{
	`{"responseId":"s2","modelVersion":"gemini-2.5-flash","candidates":[{"content":{"role":"model","parts":[{"executableCode":{"language":"PYTHON","code":"print(1)"}}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14}}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1\n"}},{"executableCode":{"language":"PYTHON","code":"print(2)"}}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["q"]}}]}`,
}

func TestStreamCountsProviderToolMeters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   map[string]int
	}{
		{
			name:   "queries repeat across chunks",
			chunks: streamRepeatedQueries,
			want: map[string]int{
				llm.MeterWebSearch:      2,
				llm.MeterGroundedPrompt: 1,
			},
		},
		{
			name:   "grounding after last usage",
			chunks: streamLateGrounding,
			want: map[string]int{
				llm.MeterWebSearch:      1,
				llm.MeterGroundedPrompt: 1,
				llm.MeterCodeExecution:  2,
			},
		},
	} {
		t.Run(tc.name+" via SDK", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, chunk := range tc.chunks {
						io.WriteString(w, "data: "+chunk+"\n\n")
					}
				},
			))
			defer server.Close()
			client := newTestClient(t, server)
			prepared, err := client.Prepare(t.Context(), sampleInput(true))
			if err != nil {
				t.Fatal(err)
			}
			live := &llm.Usage{}
			result, err := client.Send(t.Context(), llm.Call{
				Request: prepared,
				Sink: func(delta llm.Delta) {
					if delta.Kind == llm.DeltaUsage {
						live.Absorb(delta.Usage)
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil ||
				!maps.Equal(result.Usage.Requests, tc.want) {
				t.Fatalf("result requests = %+v, want %v", result.Usage, tc.want)
			}
			if !maps.Equal(live.Requests, tc.want) {
				t.Fatalf("delta requests = %v, want %v", live.Requests, tc.want)
			}
		})
		t.Run(tc.name+" via REST reader", func(t *testing.T) {
			var body strings.Builder
			for _, chunk := range tc.chunks {
				body.WriteString("data: " + chunk + "\n\n")
			}
			live := &llm.Usage{}
			wire := codec{origin: meterOrigin(t)}
			result, err := wire.readStream(
				t.Context(),
				io.NopCloser(strings.NewReader(body.String())),
				llm.Result{},
				func(delta llm.Delta) {
					if delta.Kind == llm.DeltaUsage {
						live.Absorb(delta.Usage)
					}
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil ||
				!maps.Equal(result.Usage.Requests, tc.want) {
				t.Fatalf("result requests = %+v, want %v", result.Usage, tc.want)
			}
			if !result.Usage.RequestsComplete {
				t.Fatal("a finished stream's counts are not marked final")
			}
			if !maps.Equal(live.Requests, tc.want) {
				t.Fatalf("delta requests = %v, want %v", live.Requests, tc.want)
			}
		})
	}
}

func TestStreamMetersKeepLongestGrounding(t *testing.T) {
	var meters streamMeters
	meters.observe(
		[]byte(`{"webSearchQueries":["a","b","c"]}`),
		nil,
		0,
	)
	meters.observe([]byte(`{"webSearchQueries":["a"]}`), nil, 0)
	if got := meters.counts()[llm.MeterWebSearch]; got != 3 {
		t.Fatalf("web searches = %d, want the longest list's 3", got)
	}
}

func meterOrigin(t *testing.T) content.Origin {
	t.Helper()
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpClient, err := client.httpClient("test-model")
	if err != nil {
		t.Fatal(err)
	}
	return httpClient.Origin("test-model")
}

// Built from documentation, not captured. A stream cut off before its
// finishReason may lack grounding metadata, so its counts are not final.
func TestStreamWithoutFinishReasonIsNotFinal(t *testing.T) {
	chunk := `{"responseId":"cut","modelVersion":"gemini-3-flash-preview",` +
		`"candidates":[{"content":{"role":"model","parts":[{"text":"Spain"}]}}],` +
		`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1,"totalTokenCount":13}}`
	wire := codec{origin: meterOrigin(t)}
	result, _ := wire.readStream(
		t.Context(),
		io.NopCloser(strings.NewReader("data: "+chunk+"\n\n")),
		llm.Result{},
		func(llm.Delta) {},
	)
	if result.Usage == nil {
		t.Fatal("usage was lost")
	}
	if result.Usage.RequestsComplete {
		t.Fatal("counts from an unfinished stream are marked final")
	}
}
