package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"google.golang.org/genai"
)

// sdkArguments passes the familiar typed request through the SDK. The final
// body hook retains provider-private parts that its typed model may not know.
func sdkArguments(
	body []byte,
) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	var request struct {
		Contents          []*genai.Content  `json:"contents"`
		SystemInstruction *genai.Content    `json:"systemInstruction"`
		GenerationConfig  json.RawMessage   `json:"generationConfig"`
		Tools             []*genai.Tool     `json:"tools"`
		ToolConfig        *genai.ToolConfig `json:"toolConfig"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, nil, err
	}
	config := &genai.GenerateContentConfig{
		SystemInstruction: request.SystemInstruction,
		Tools:             request.Tools,
		ToolConfig:        request.ToolConfig,
	}
	if len(request.GenerationConfig) > 0 {
		if err := json.Unmarshal(request.GenerationConfig, config); err != nil {
			return nil, nil, err
		}
	}
	var frozen map[string]any
	if err := providerhttp.Decode(body, &frozen); err != nil {
		return nil, nil, err
	}
	config.HTTPOptions = &genai.HTTPOptions{
		ExtrasRequestProvider: func(map[string]any) map[string]any {
			return frozen
		},
	}
	return request.Contents, config, nil
}

// sdkEndpoint pins the URL grammar that the SDK generates. A custom endpoint
// must use the same model path; otherwise a prepared origin could name a
// different destination from the one the SDK calls.
func sdkEndpoint(
	endpoint string,
	vertex bool,
) (baseURL, version string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("gemini: invalid SDK endpoint")
	}
	const modelSuffix = "/models/{model}:generateContent"
	if !strings.HasSuffix(u.Path, modelSuffix) {
		return "", "", errors.New("gemini: SDK endpoint must end in /models/{model}:generateContent")
	}
	prefix := strings.TrimSuffix(u.Path, modelSuffix)
	if vertex {
		index := strings.Index(prefix, "/projects/")
		if index < 0 || !strings.HasSuffix(prefix, "/publishers/google") {
			return "", "", errors.New("gemini: invalid Vertex endpoint")
		}
		prefix = prefix[:index]
	}
	last := strings.LastIndex(prefix, "/")
	if last < 0 {
		return "", "", errors.New("gemini: SDK endpoint requires an API version")
	}
	version = prefix[last+1:]
	if version != "v1" && version != "v1beta" {
		return "", "", errors.New("gemini: SDK endpoint requires v1 or v1beta")
	}
	u.Path = prefix[:last]
	return u.String(), version, nil
}

func (p *Client) newSDKClient(
	ctx context.Context,
	transport http.RoundTripper,
	authenticate bool,
) (*genai.Client, error) {
	baseURL, version, err := sdkEndpoint(p.config.Endpoint, p.config.Vertex)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{}
	if p.config.Client != nil {
		copy := *p.config.Client
		httpClient = &copy
	}
	httpClient.Transport = transport
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	config := &genai.ClientConfig{
		Backend:     genai.BackendGeminiAPI,
		APIKey:      p.config.APIKey,
		HTTPClient:  httpClient,
		HTTPOptions: genai.HTTPOptions{BaseURL: baseURL, APIVersion: version},
	}
	// The SDK treats an empty APIKey as permission to read the process
	// environment. Resolve keys only in New; a custom keyless route must not
	// acquire Google's credentials here. The guard removes this placeholder
	// before real dispatch, so keyless custom transports remain supported.
	if !p.config.Vertex && config.APIKey == "" {
		config.APIKey = "dashi-sdk-keyless"
	}
	if p.config.Vertex {
		config.Backend = genai.BackendVertexAI
		config.Project = p.config.Project
		config.Location = p.config.Location
		if authenticate && p.config.Client == nil {
			if err := config.UseDefaultCredentials(); err != nil {
				return nil, err
			}
		}
	}
	return genai.NewClient(ctx, config)
}

func (p *Client) sdkURL(model string, stream bool) string {
	endpoint := p.endpoint(model)
	if stream {
		return strings.Replace(
			endpoint,
			":generateContent",
			":streamGenerateContent?alt=sse",
			1,
		)
	}
	return endpoint
}

// sdkModel preserves one exact model ID as one URL path segment. Only the
// SDK receives this representation; recorded model and replay identities stay raw.
func sdkModel(model string) string { return url.PathEscape(model) }

// sdkBody uses an in-memory HTTP transport. Preparation invokes the same SDK
// serializer as Send but cannot reach the network or charge for a model call.
func (p *Client) sdkBody(
	ctx context.Context,
	model string,
	stream bool,
	body []byte,
) ([]byte, error) {
	guard := &sdkGuard{url: p.sdkURL(model, stream), dryRun: true}
	client, err := p.newSDKClient(ctx, guard, false)
	if err != nil {
		return nil, err
	}
	contents, config, err := sdkArguments(body)
	if err != nil {
		return nil, err
	}
	if stream {
		for _, err := range client.Models.GenerateContentStream(
			ctx,
			sdkModel(model),
			contents,
			config,
		) {
			if err != nil {
				return nil, err
			}
		}
	} else if _, err := client.Models.GenerateContent(
		ctx,
		sdkModel(model),
		contents,
		config,
	); err != nil {
		return nil, err
	}
	if !guard.seen {
		return nil, errors.New("gemini: SDK did not prepare an HTTP request")
	}
	return bytes.Clone(guard.body), nil
}

func (p *Client) sdkClient(
	ctx context.Context,
	model string,
	stream bool,
	body []byte,
) (*genai.Client, *sdkGuard, error) {
	base := http.DefaultTransport
	if p.config.Client != nil && p.config.Client.Transport != nil {
		base = p.config.Client.Transport
	}
	guard := &sdkGuard{
		url:       p.sdkURL(model, stream),
		body:      bytes.Clone(body),
		base:      base,
		pinAPIKey: !p.config.Vertex,
		apiKey:    p.config.APIKey,
	}
	client, err := p.newSDKClient(ctx, guard, true)
	return client, guard, err
}

// sdkGuard rejects URL or body drift before the real RoundTrip. The response
// copy retains raw fields the SDK's typed response may not yet represent.
type sdkGuard struct {
	url       string
	body      []byte
	base      http.RoundTripper
	dryRun    bool
	pinAPIKey bool
	apiKey    string
	seen      bool
	mu        sync.Mutex
	resp      bytes.Buffer
}

// RoundTrip dispatches only when the SDK request matches the prepared request.
func (g *sdkGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	if g.seen {
		g.mu.Unlock()
		return nil, errors.New("gemini: SDK attempted more than one dispatch")
	}
	g.seen = true
	g.mu.Unlock()
	actual, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if r.URL.String() != g.url || (!g.dryRun && !bytes.Equal(actual, g.body)) {
		return nil, errors.New("gemini: SDK request differs from prepared URL or body")
	}
	if g.dryRun {
		g.body = actual
		response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
		contentType := "application/json"
		if strings.Contains(g.url, ":streamGenerateContent") {
			contentType = "text/event-stream"
			response = "data: " + response + "\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    r,
		}, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(actual))
	if g.pinAPIKey {
		r.Header.Del("X-Goog-Api-Key")
		if g.apiKey != "" {
			r.Header.Set("X-Goog-Api-Key", g.apiKey)
		}
	}
	response, err := g.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	response.Body = &captureBody{
		ReadCloser: response.Body,
		output:     &g.resp,
		mu:         &g.mu,
	}
	return response, nil
}

func (g *sdkGuard) responseBytes() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return bytes.Clone(g.resp.Bytes())
}

type captureBody struct {
	io.ReadCloser
	output *bytes.Buffer
	mu     *sync.Mutex
}

// Read retains the bytes observed while callers consume the SDK response.
func (r *captureBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	r.output.Write(p[:n])
	r.mu.Unlock()
	return n, err
}

// sdkError converts an SDK failure. hasMedia reports whether the request
// carried media, which media classification requires; the provider-independent
// rules apply after Gemini's own, as they do for other HTTP providers.
func sdkError(provider string, err error, body []byte, hasMedia bool) error {
	var api genai.APIError
	var apiPointer *genai.APIError
	if errors.As(err, &api) || errors.As(err, &apiPointer) {
		if apiPointer != nil {
			api = *apiPointer
		}
		category := llm.ErrorInvalidRequest
		switch {
		case api.Code == 401 || api.Code == 403:
			category = llm.ErrorAuthentication
		case api.Code == 429:
			category = llm.ErrorRateLimited
		case api.Code >= 500:
			category = llm.ErrorOverloaded
		}
		disposition := llm.SendStatusUnknown
		if providerhttp.KnownRejection(api.Code) {
			disposition = llm.SendStatusProviderRejected
		}
		rejection := providerhttp.Rejection{
			Status:   api.Code,
			Type:     api.Status,
			Message:  api.Message,
			HasMedia: hasMedia,
		}
		return &llm.Error{
			Provider:   provider,
			Category:   category,
			Reason:     providerhttp.Reason(errorReason, rejection),
			SendStatus: disposition,
			Status:     api.Code,
			Body:       bytes.Clone(body),
			Retryable:  api.Code == 408 || api.Code == 429 || api.Code >= 500,
			Cause:      err,
		}
	}
	return providerhttp.Failure(provider, llm.ErrorTransport, "SDK call: %w", err)
}

// sdkStream forwards SDK stream chunks to a sink as provisional deltas. It
// keeps state across chunks so usage deltas carry cumulative hosted-tool
// counts, including counts from metadata that arrives after the last
// usageMetadata.
type sdkStream struct {
	sink   llm.Sink
	meters streamMeters
	usage  *llm.Usage
}

// emit forwards one chunk. Content deltas are indexed within the chunk.
func (s *sdkStream) emit(response *genai.GenerateContentResponse) {
	if response == nil {
		return
	}
	changed := false
	if len(response.Candidates) > 0 && response.Candidates[0] != nil {
		candidate := response.Candidates[0]
		s.emitContent(candidate.Content)
		changed = s.observe(candidate)
	}
	if response.UsageMetadata != nil {
		usage := usageWire{
			PromptTokenCount:        int(response.UsageMetadata.PromptTokenCount),
			CandidatesTokenCount:    int(response.UsageMetadata.CandidatesTokenCount),
			CachedContentTokenCount: int(response.UsageMetadata.CachedContentTokenCount),
			ThoughtsTokenCount:      int(response.UsageMetadata.ThoughtsTokenCount),
			ToolUsePromptTokenCount: int(response.UsageMetadata.ToolUsePromptTokenCount),
			TotalTokenCount:         int(response.UsageMetadata.TotalTokenCount),
		}
		if normalized, err := usage.normalize(); err == nil {
			s.usage = normalized
			changed = true
		}
	}
	if changed && s.usage != nil {
		providerhttp.Emit(s.sink, llm.Delta{
			Kind:  llm.DeltaUsage,
			Usage: usageWithMeters(s.usage, s.meters.counts()),
		})
	}
}

func (s *sdkStream) emitContent(content *genai.Content) {
	if content == nil {
		return
	}
	for index, part := range content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			kind := llm.DeltaText
			if part.Thought {
				kind = llm.DeltaReasoning
			}
			providerhttp.Emit(
				s.sink,
				llm.Delta{
					Kind:  kind,
					Index: index,
					Text:  part.Text,
				},
			)
		}
		if part.FunctionCall != nil {
			args, _ := json.Marshal(part.FunctionCall.Args)
			providerhttp.Emit(
				s.sink,
				llm.Delta{
					Kind:  llm.DeltaToolArguments,
					Index: index,
					Text:  string(args),
				},
			)
		}
	}
}

// observe counts one chunk's hosted work. The SDK's typed metadata is
// re-encoded to its wire JSON so both stream paths count with one set of
// rules.
func (s *sdkStream) observe(candidate *genai.Candidate) bool {
	var grounding, urlContext json.RawMessage
	if candidate.GroundingMetadata != nil {
		grounding, _ = json.Marshal(candidate.GroundingMetadata)
	}
	if candidate.URLContextMetadata != nil {
		urlContext, _ = json.Marshal(candidate.URLContextMetadata)
	}
	runs := 0
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part != nil && part.ExecutableCode != nil {
				runs++
			}
		}
	}
	return s.meters.observe(grounding, urlContext, runs)
}
