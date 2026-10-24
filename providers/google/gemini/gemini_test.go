package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{
		Provider:         "google",
		APIKey:           "test",
		Endpoint:         server.URL + "/v1beta/models/{model}:generateContent",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func sampleInput(stream bool) llm.Input {
	return llm.Input{
		Model:    "gemini-test",
		Stream:   stream,
		System:   "Be concise.",
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		Tools: []tool.Definition{
			{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}
}

func TestClientFunctionCallingUsesV1Beta(t *testing.T) {
	stable, err := New(Config{
		Provider:         "google",
		APIKey:           "test",
		Endpoint:         strings.Replace(DefaultEndpoint, "/v1beta/", "/v1/", 1),
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stable.Prepare(
		t.Context(),
		sampleInput(false),
	); err == nil ||
		!strings.Contains(err.Error(), "v1beta") {
		t.Fatalf(
			"v1 client function calling was not rejected before dispatch: %v",
			err,
		)
	}
	history := geminiToolHistory("call-1")
	history.Tools = nil
	if _, err := stable.Prepare(t.Context(), history); err == nil ||
		!strings.Contains(err.Error(), "v1beta") {
		t.Fatalf(
			"v1 client function history was not rejected before dispatch: %v",
			err,
		)
	}
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(t.Context(), sampleInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(client.endpoint("gemini-test"), "/v1beta/") ||
		!bytes.Contains(request.Body(), []byte(`"parametersJsonSchema"`)) {
		t.Fatalf("v1beta request lost JSON Schema: %s", request.Body())
	}
}

func TestPrepareSendAndReplaySignedParts(t *testing.T) {
	var attempts atomic.Int32
	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/v1beta/models/gemini-test:generateContent" ||
			r.Header.Get("X-Goog-Api-Key") != "test" {
			t.Errorf("wrong Gemini route or key: %s %+v", r.URL, r.Header)
		}
		sent, _ = io.ReadAll(r.Body)
		io.WriteString(
			w,
			`{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"Hello","thoughtSignature":"c2ln"},{"functionCall":{"id":"call-1","name":"search","args":{"q":"yes"}},"thoughtSignature":"c2ln"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":5,"candidatesTokenCount":4,"thoughtsTokenCount":2,"totalTokenCount":18}}`,
		)
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 0 || !strings.Contains(
		string(prepared.Body()),
		`"functionDeclarations"`,
	) {
		t.Fatalf("preparation sent or lost tools: %s", prepared.Body())
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 ||
		string(sent) != string(prepared.Body()) ||
		result.ID != "r1" ||
		result.Served.Model != "served" ||
		result.Stop.Kind != llm.StopFinished ||
		result.Message.Text() != "Hello" ||
		result.Message.Source == nil || result.Message.Source.ResponseID != "r1" ||
		len(result.Message.Content) != 2 ||
		result.Message.Content[0].Replay == nil ||
		result.Message.Content[1].ToolCall.ID != "call-1" || result.Usage == nil ||
		result.Usage.InputTokens != 7 || result.Usage.CacheReadInputTokens != 5 ||
		result.Usage.OutputTokens != 6 || result.Usage.ReasoningTokens != 2 {
		t.Fatalf("Gemini response lost data: %+v", result)
	}
	replay := sampleInput(false)
	replay.Messages = append(
		replay.Messages,
		result.Message,
		llm.UserMessage(llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: "call-1",
				Output: tool.Output{Content: []content.Block{content.TextBlock("found")}},
			},
		}),
	)
	preparedReplay, err := p.Prepare(t.Context(), replay)
	if err != nil {
		t.Fatalf("signed Gemini turn did not replay: %v", err)
	}
	if !strings.Contains(
		string(preparedReplay.Body()),
		`"thoughtSignature":"c2ln"`,
	) ||
		!strings.Contains(string(preparedReplay.Body()), `"functionResponse"`) ||
		!strings.Contains(string(preparedReplay.Body()), `"name":"search"`) {
		t.Fatalf(
			"signed part or named tool result was lost: %s",
			preparedReplay.Body(),
		)
	}
}

func TestStreamCollectsTextSignatureAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:streamGenerateContent" ||
			r.URL.Query().Get("alt") != "sse" {
			t.Errorf("wrong Gemini stream route: %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"lo"},{"thoughtSignature":"c2ln"},{"functionCall":{"id":"call-1","name":"search","args":{"q":"yes"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
		} {
			io.WriteString(w, "data: "+chunk+"\n\n")
		}
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(true))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []llm.Delta
	result, err := p.Send(
		t.Context(),
		llm.Call{
			Request: prepared,
			Sink: func(d llm.Delta) {
				deltas = append(deltas, d)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Text() != "Hello" || len(result.Message.Content) != 2 ||
		result.Message.Content[0].Replay == nil ||
		result.Message.Content[1].ToolCall.ID != "call-1" ||
		result.Usage == nil || result.Usage.InputTokens != 3 || len(deltas) < 3 {
		t.Fatalf(
			"Gemini stream lost final data: result=%+v deltas=%+v",
			result,
			deltas,
		)
	}
}

func TestVertexUsesSDKRouteAndFrozenBody(t *testing.T) {
	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/project/locations/global/publishers/google/models/gemini-test:generateContent" {
			t.Errorf("wrong Vertex route: %s", r.URL)
		}
		sent, _ = io.ReadAll(r.Body)
		io.WriteString(
			w,
			`{"responseId":"vertex-1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}]}`,
		)
	}))
	defer server.Close()
	p, err := New(Config{
		Provider:         "vertex-test",
		Vertex:           true,
		Project:          "project",
		Location:         "global",
		CredentialScope:  "test-principal",
		Endpoint:         server.URL + "/v1/projects/project/locations/global/publishers/google/models/{model}:generateContent",
		Client:           &http.Client{Transport: http.DefaultTransport},
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := p.Prepare(t.Context(), sampleInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if sent != nil {
		t.Fatal("Vertex preparation made a real request")
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != string(prepared.Body()) ||
		result.ID != "vertex-1" ||
		result.Message.Text() != "Hello" {
		t.Fatalf("Vertex SDK call lost frozen body or response: %+v", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return f(request)
}

func TestSDKGuardRejectsDriftBeforeDispatch(t *testing.T) {
	var dispatched bool
	guard := &sdkGuard{
		url:  "https://example.test/v1/models/m:generateContent",
		body: []byte(`{"contents":[]}`),
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			dispatched = true
			return nil, nil
		}),
	}
	request, err := http.NewRequest(
		http.MethodPost,
		guard.url,
		bytes.NewBufferString(`{"contents":[1]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.RoundTrip(request); err == nil || dispatched {
		t.Fatalf(
			"SDK body drift reached transport: err=%v dispatched=%v",
			err,
			dispatched,
		)
	}
}

func TestVertexCredentialScopesPrivateReplay(t *testing.T) {
	first, err := New(Config{
		Vertex:          true,
		Project:         "project",
		CredentialScope: "principal-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Config{
		Vertex:          true,
		Project:         "project",
		CredentialScope: "principal-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := first.httpClient("gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.httpClient("gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	if a.Origin("gemini-test") == b.Origin("gemini-test") {
		t.Fatal("different Vertex principals share private replay scope")
	}
}

func TestSDKAPIErrorPreservesStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(
			w,
			`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`,
		)
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(false))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok ||
		failure.Category != llm.ErrorRateLimited ||
		failure.Status != 429 ||
		!failure.Retryable ||
		!bytes.Contains(failure.Body, []byte("slow down")) ||
		result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("SDK API error was misclassified: result=%+v err=%v", result, err)
	}
}

// A model known only from the built-in catalog is fitted to the catalog's
// limits, as a model supplied in Config.Models is.
func TestPrepareFitsCatalogModel(t *testing.T) {
	client, err := New(Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ModelInfo("gemini-4-argon"); !ok {
		t.Fatal("catalog lacks gemini-4-argon")
	}
	input := sampleInput(false)
	input.Model = "gemini-4-argon"
	input.Settings.Temperature = new(0.5)
	input.Settings.MaxTokens = 100_000
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatalf("default policy rejected fittable settings: %v", err)
	}
	effective := request.Effective()
	if effective.Temperature != nil {
		t.Fatalf("temperature = %g, want it removed", *effective.Temperature)
	}
	if effective.MaxTokens != 65_536 {
		t.Fatalf("max tokens = %d, want 65536", effective.MaxTokens)
	}
	if bytes.Contains(request.Body(), []byte(`"temperature"`)) {
		t.Fatalf("body kept an unsupported temperature: %s", request.Body())
	}
	if len(request.Warnings()) != 2 {
		t.Fatalf("warnings = %+v, want one per fitted setting", request.Warnings())
	}
}

// The default Vertex host follows the Google GenAI SDK for each location.
func TestVertexDefaultEndpointForLocation(t *testing.T) {
	for location, host := range map[string]string{
		"global":      "https://aiplatform.googleapis.com/",
		"us":          "https://aiplatform.us.rep.googleapis.com/",
		"eu":          "https://aiplatform.eu.rep.googleapis.com/",
		"us-central1": "https://us-central1-aiplatform.googleapis.com/",
	} {
		p, err := New(Config{
			Vertex:          true,
			Project:         "project",
			Location:        location,
			CredentialScope: "principal",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := host + "v1/projects/project/locations/" + location + "/"
		if !strings.HasPrefix(p.config.Endpoint, want) {
			t.Fatalf("%s: endpoint = %s", location, p.config.Endpoint)
		}
	}
}

// Gemini needs each function call answered before the next model turn. The
// answers may span several user messages, as Dashi records one per result.
func TestPrepareRejectsCallsUnansweredBeforeNextModelTurn(t *testing.T) {
	call := llm.Content{
		Kind:     llm.ContentToolCall,
		ToolCall: &tool.Call{ID: "c1", Name: "search", Args: json.RawMessage(`{}`)},
	}
	result := llm.Content{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: "c1"},
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newTestClient(t, server)
	input := sampleInput(false)
	input.Messages = []llm.Message{
		llm.UserMessage(llm.Text("hi")),
		llm.AssistantMessage(call),
		llm.AssistantMessage(llm.Text("wait")),
		llm.UserMessage(result),
	}
	_, err := p.Prepare(t.Context(), input)
	if err == nil || !strings.Contains(err.Error(), "unanswered function calls") {
		t.Fatalf("model turn before the answer: err = %v", err)
	}
	second := call
	second.ToolCall = &tool.Call{ID: "c2", Name: "search", Args: json.RawMessage(`{}`)}
	secondResult := result
	secondResult.ToolResult = &llm.ToolResult{CallID: "c2"}
	input.Messages = []llm.Message{
		llm.UserMessage(llm.Text("hi")),
		llm.AssistantMessage(call, second),
		llm.UserMessage(result),
		llm.UserMessage(secondResult),
	}
	if _, err := p.Prepare(t.Context(), input); err != nil {
		t.Fatalf("answers in separate user messages: %v", err)
	}
}
