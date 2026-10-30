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
	"time"

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
			// The SDK sends the credential's quota project only on a client
			// it builds itself; user credentials need it for Vertex.
			quota, err := config.Credentials.QuotaProjectID(ctx)
			if err != nil {
				return nil, err
			}
			httpClient.Transport = withQuotaProject(httpClient.Transport, quota)
		}
	}
	return genai.NewClient(ctx, config)
}

// withQuotaProject sends project as X-Goog-User-Project, which bills and
// meters the call to that project. An empty project leaves next unchanged.
func withQuotaProject(next http.RoundTripper, project string) http.RoundTripper {
	if project == "" {
		return next
	}
	return quotaTransport{next: next, project: project}
}

type quotaTransport struct {
	next    http.RoundTripper
	project string
}

func (t quotaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("X-Goog-User-Project") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("X-Goog-User-Project", t.project)
	}
	return t.next.RoundTrip(r)
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
		url:  p.sdkURL(model, stream),
		body: bytes.Clone(body),
		base: providerhttp.TimeoutTransport(
			base,
			p.config.Timeouts,
			p.config.Client != nil,
			stream,
		),
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
	// dispatched reports that the request reached the real transport;
	// status and header describe the response it returned, if any.
	dispatched bool
	status     int
	header     http.Header
	// raw is the real transport's response body, before the copy.
	raw io.ReadCloser
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
	// A canceled call never reaches the transport, so it is known unsent.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.dispatched = true
	g.mu.Unlock()
	response, err := g.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.status, g.header = response.StatusCode, response.Header.Clone()
	g.raw = response.Body
	g.mu.Unlock()
	response.Body = &captureBody{
		ReadCloser: response.Body,
		output:     &g.resp,
		mu:         &g.mu,
		limit:      maxCaptureBytes,
	}
	return response, nil
}

// rawBody returns the real transport's response body, or nil before one.
func (g *sdkGuard) rawBody() io.ReadCloser {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.raw
}

func (g *sdkGuard) responseBytes() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return bytes.Clone(g.resp.Bytes())
}

// maxCaptureBytes bounds the response copy, streamed or not. It leaves room
// for inline media while stopping a runaway body from exhausting memory.
const maxCaptureBytes = 256 << 20

var errCaptureTooLarge = errors.New("gemini: response body exceeds 256 MiB")

type captureBody struct {
	io.ReadCloser
	output *bytes.Buffer
	mu     *sync.Mutex
	limit  int
}

// Read retains the bytes observed while callers consume the SDK response,
// and fails once they pass the limit.
func (r *captureBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.output.Len()+n > r.limit {
		return 0, errCaptureTooLarge
	}
	r.output.Write(p[:n])
	return n, err
}

// sdkResponse is what the guard saw of the real HTTP exchange.
type sdkResponse struct {
	dispatched bool
	status     int
	header     http.Header
	body       []byte
}

func (g *sdkGuard) response() sdkResponse {
	g.mu.Lock()
	defer g.mu.Unlock()
	return sdkResponse{
		dispatched: g.dispatched,
		status:     g.status,
		header:     g.header,
		body:       bytes.Clone(g.resp.Bytes()),
	}
}

// sdkError converts an SDK failure. hasMedia reports whether the request
// carried media, which media classification requires; the provider-independent
// rules apply after Gemini's own, as they do for other HTTP providers.
//
// An HTTP error is classified from the status and body the guard saw, as other
// providers classify theirs: the SDK's APIError loses the status when the
// body's error has no code, and the error fields when the body is an array or
// plain text. An APIError after a successful status is an error reported
// inside the stream: the provider accepted the request, so it is not a known
// rejection.
func sdkError(provider string, err error, seen sdkResponse, hasMedia bool) error {
	// A Dashi timeout is reported as the other HTTP providers report it.
	if providerhttp.TimeoutReason(err) != "" {
		return providerhttp.ReadFailure(provider, err)
	}
	var api genai.APIError
	var apiPointer *genai.APIError
	if errors.As(err, &api) || errors.As(err, &apiPointer) {
		if apiPointer != nil {
			api = *apiPointer
		}
		inStream := seen.status >= 200 && seen.status < 300
		status := api.Code
		rejection := providerhttp.Rejection{Type: api.Status, Message: api.Message}
		var retryAfter time.Duration
		if seen.status >= 300 {
			status = seen.status
			rejection = providerhttp.ParseRejection(status, seen.body)
			if rejection.Message == "" {
				rejection.Message = api.Message
			}
			retryAfter, _ = providerhttp.NewError(
				status,
				string(seen.body),
				providerhttp.WithErrorHeader(seen.header),
			).RetryAfter()
		} else {
			// The classifier reads the error's own code, so an error inside a
			// stream with code 500 is not a context-window rejection.
			rejection.Status = status
		}
		rejection.HasMedia = hasMedia
		category := llm.ErrorInvalidRequest
		switch {
		case status == 401 || status == 403:
			category = llm.ErrorAuthentication
		case status == 429:
			category = llm.ErrorRateLimited
		case status >= 500:
			category = llm.ErrorOverloaded
		}
		sendStatus := llm.SendStatusUnknown
		httpStatus := status
		if inStream {
			// The HTTP status was a success; report none, as other providers
			// do for an error inside a stream.
			httpStatus = 0
		} else if providerhttp.KnownRejection(status) {
			sendStatus = llm.SendStatusProviderRejected
		}
		return &llm.Error{
			Provider:   provider,
			Category:   category,
			Reason:     providerhttp.Reason(errorReason, rejection),
			SendStatus: sendStatus,
			Status:     httpStatus,
			Body:       seen.body,
			RetryAfter: retryAfter,
			Retryable:  status == 408 || status == 429 || status >= 500,
			Cause:      err,
		}
	}
	if errors.Is(err, errCaptureTooLarge) {
		// Classified as other providers classify an oversized body.
		return providerhttp.Failure(provider, llm.ErrorProtocol, "SDK call: %w", err)
	}
	if !seen.dispatched {
		return providerhttp.NotDispatched(provider, llm.ErrorTransport, "SDK call: %w", err)
	}
	return providerhttp.Failure(provider, llm.ErrorTransport, "SDK call: %w", err)
}

// sdkStream forwards SDK stream chunks to a sink as provisional deltas. It
// keeps state across chunks so usage deltas carry cumulative provider-tool
// counts, including counts from metadata that arrives after the last
// usageMetadata, and so content deltas carry the index of their part in the
// assembled message.
type sdkStream struct {
	sink    llm.Sink
	meters  streamMeters
	usage   *llm.Usage
	indexes partIndexes
}

// emit forwards one chunk.
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
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		// The part's wire fields decide how it joins the assembled message.
		var fields map[string]any
		raw, err := json.Marshal(part)
		if err != nil || providerhttp.Decode(raw, &fields) != nil {
			fields = nil
		}
		if sdkPartEmpty(part) {
			continue
		}
		index, _ := s.indexes.next(fields)
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
			args, _ := callArgs(part.FunctionCall.Args)
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

// observe counts one chunk's provider tool work. The SDK's typed metadata is
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
