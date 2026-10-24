// Package providerhttp supplies shared HTTP dispatch and replay checks for
// providers. It does not own agent state or retry policy. HTTP lifecycle and
// error classification are adapted from Dive (65c393f), Apache-2.0.
package providerhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Client executes at most one HTTP request and owns configured model knowledge.
// HTTP transports supplied by applications must not add hidden retries.
type Client struct {
	// Provider names the configured destination.
	Provider string

	// API identifies the wire format used to qualify private replay.
	API string

	// Endpoint is the dispatch URL pinned at construction.
	Endpoint string

	// HTTP supplies transport behavior; its transport must not hide retries.
	HTTP *http.Client
	// timeouts is an owned snapshot of the model-call timeout policy.
	timeouts llm.HTTPTimeouts

	// Headers is an owned template cloned for each dispatch.
	Headers http.Header
	// Classify refines rejected responses with an llm.ErrorReason. Nil
	// applies only the provider-independent rules.
	Classify Classifier
	// NoKey, when set, is returned by Post before dispatch. Constructors set
	// it with NoAPIKey when the public endpoint has no key.
	NoKey  error
	scope  string
	models map[string]llm.ModelInfo
	policy llm.CapabilityPolicy
}

// NewClient snapshots configuration and derives a replay namespace from the
// endpoint and credential. Credential rotation conservatively invalidates old
// private replay data. Neither the credential nor endpoint appears in Origin.
func NewClient(
	provider, api, endpoint, credential string,
	httpClient *http.Client,
	headers http.Header,
	models map[string]llm.ModelInfo,
	policy llm.CapabilityPolicy,
	timeouts llm.HTTPTimeouts,
) (Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "https" &&
			u.Scheme != "http") ||
		u.User != nil ||
		u.Fragment != "" {
		return Client{}, errors.New("providerhttp: invalid endpoint")
	}
	if strings.TrimSpace(provider) == "" || api == "" {
		return Client{}, errors.New(
			"providerhttp: provider identity and API are required",
		)
	}
	if !policy.Valid() {
		return Client{}, errors.New("providerhttp: invalid capability policy")
	}
	if !ValidTimeouts(timeouts) {
		return Client{}, errors.New("providerhttp: timeouts must be nonnegative")
	}
	if timeouts.Total != nil {
		total := *timeouts.Total
		timeouts.Total = &total
	}
	catalog := make(map[string]llm.ModelInfo, len(models))
	for model, info := range models {
		if model == "" {
			return Client{}, errors.New("providerhttp: model name is required")
		}
		if err := info.Validate(); err != nil {
			return Client{}, fmt.Errorf(
				"providerhttp: model %q: %w",
				model,
				err,
			)
		}
		if e := info.Evidence; e.Model != model ||
			e.API != api ||
			e.Endpoint != endpoint {
			return Client{}, fmt.Errorf(
				"providerhttp: model %q evidence does not match model, API, and endpoint",
				model,
			)
		}
		catalog[model] = info.Clone()
	}
	scopeData, _ := json.Marshal([]string{api, endpoint, credential})
	scope := fmt.Sprintf("%x", sha256.Sum256(scopeData))
	return Client{
		Provider: provider,
		API:      api,
		Endpoint: endpoint,
		HTTP:     httpClient,
		timeouts: timeouts,
		Headers:  headers.Clone(),
		scope:    scope,
		models:   catalog,
		policy:   policy.Resolved(),
	}, nil
}

// Origin binds model replay to the configured API, endpoint, and credentials.
func (c Client) Origin(model string) content.Origin {
	return content.Origin{
		Provider: c.Provider,
		API:      c.API,
		Model:    model,
		Scope:    c.scope,
	}
}

// ModelInfo returns an independent snapshot and reports whether it was known.
// Configured knowledge wins; otherwise the catalog supplies documented facts
// when it covers this exact API and endpoint, so known models on public
// endpoints get limits and prices without application setup.
func (c Client) ModelInfo(model string) (llm.ModelInfo, bool) {
	if info, ok := c.models[model]; ok {
		return info.Clone(), true
	}
	return catalog.Lookup(c.API, c.Endpoint, model)
}

// Policy reports the resolved capability policy, for provider rules that Fit
// cannot express. It is never the zero value.
func (c Client) Policy() llm.CapabilityPolicy {
	return c.policy
}

// Fit brings effective settings within the model's known limits combined with
// limits api imposes on every model, under the configured policy. Providers
// call it before encoding, so the body carries the fitted values.
func (c Client) Fit(
	effective llm.Input,
	api llm.Capabilities,
	adjustments []llm.Adjustment,
) (llm.Input, []llm.Adjustment, error) {
	info, _ := c.ModelInfo(effective.Model)
	limits := info.Capabilities.Intersect(api)
	fitted, adjustments, err := limits.Fit(
		effective,
		c.policy,
		adjustments,
	)
	if err != nil {
		failure := Failure(c.Provider, llm.ErrorUnsupported, "%w", err)
		return llm.Input{}, nil, failure
	}
	return fitted, adjustments, nil
}

// Prepare checks effective capabilities and snapshots the single wire body.
func (c Client) Prepare(
	requested, effective llm.Input,
	body map[string]any,
	adjustments []llm.Adjustment,
	changes []llm.ContentChange,
) (llm.Request, error) {
	info, known := c.ModelInfo(requested.Model)
	unverified, err := info.Capabilities.Check(effective, c.policy)
	if err != nil {
		return llm.Request{}, Failure(
			c.Provider,
			llm.ErrorUnsupported,
			"%w",
			err,
		)
	}
	body["stream"] = requested.Stream
	raw, err := json.Marshal(body)
	if err != nil {
		return llm.Request{}, err
	}
	var snapshot *llm.ModelInfo
	if known {
		snapshot = &info
	}
	return llm.NewRequest(llm.RequestSnapshot{
		Origin:           c.Origin(requested.Model),
		Input:            requested,
		Effective:        effective.Settings,
		Body:             raw,
		Adjustments:      adjustments,
		ContentChanges:   changes,
		ModelInfo:        snapshot,
		CapabilityPolicy: c.policy,
		Unverified:       unverified,
	})
}

// Post performs at most one dispatch, without retries or redirects. Errors
// include retry hints for the caller that owns recording each model call and
// reserving its cost.
func (c Client) Post(
	ctx context.Context,
	p llm.Request,
) (*http.Response, error) {
	if c.NoKey != nil {
		return nil, c.NoKey
	}
	request := p.Input()
	if p.Origin() != c.Origin(request.Model) {
		return nil, NotDispatched(
			c.Provider,
			llm.ErrorInvalidRequest,
			"prepared request belongs to another destination",
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, NotDispatched(
			c.Provider,
			llm.ErrorTransport,
			"%w",
			err,
		)
	}
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.Endpoint,
		bytes.NewReader(p.Body()),
	)
	if err != nil {
		return nil, NotDispatched(
			c.Provider,
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	r.Header = c.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Content-Type", "application/json")
	if request.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	resp, err := bound(
		r,
		c.timeouts,
		callTotal(c.timeouts, c.HTTP != nil),
		request.Stream,
		client.Do,
	)
	if err != nil {
		return nil, ReadFailure(c.Provider, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, ReadFailure(c.Provider, err)
	}
	pe := NewError(
		resp.StatusCode,
		string(body),
		WithErrorHeader(resp.Header),
	)
	category := llm.ErrorInvalidRequest
	switch {
	case pe.statusCode == 401 || pe.statusCode == 403:
		category = llm.ErrorAuthentication
	case pe.statusCode == 429:
		category = llm.ErrorRateLimited
	case pe.statusCode >= 500:
		category = llm.ErrorOverloaded
	}
	sendStatus := llm.SendStatusUnknown
	if KnownRejection(pe.statusCode) {
		sendStatus = llm.SendStatusProviderRejected
	}
	rejection := ParseRejection(pe.statusCode, body)
	rejection.HasMedia = HasMedia(request)
	return nil, &llm.Error{
		Provider: c.Provider,
		Category: category,
		Reason: Reason(
			c.Classify,
			rejection,
		),
		SendStatus: sendStatus,
		Status:     pe.statusCode,
		Code:       pe.code,
		Body:       body,
		RetryAfter: pe.retryAfter,
		Retryable:  shouldRetry(pe.statusCode, pe.code),
		Cause:      pe,
	}
}

// NoAPIKey returns the not-dispatched authentication error for a client with
// no key, or nil when none is needed. A key is needed only when key is empty
// and the destination is the provider's public one: a custom endpoint or a
// custom HTTP client may supply credentials itself. env names the variables
// the constructor read, such as "ANTHROPIC_API_KEY".
func NoAPIKey(provider, key string, public bool, env string) error {
	if key != "" || !public {
		return nil
	}
	return &llm.Error{
		Provider:   provider,
		Category:   llm.ErrorAuthentication,
		SendStatus: llm.SendStatusNotSent,
		Cause: fmt.Errorf(
			"%w: set %s or Config.APIKey",
			llm.ErrNoAPIKey,
			env,
		),
	}
}

// MaybeSent reports whether the request behind err may have reached the
// provider. Only an error marked SendStatusNotSent rules that out, such as a
// missing key or a context cancelled before dispatch. An HTTP status, a
// dropped connection, or a response-start timeout all leave it possible.
func MaybeSent(err error) bool {
	failure, ok := errors.AsType[*llm.Error](err)
	return !ok || failure.SendStatus != llm.SendStatusNotSent
}

// KnownRejection is limited to HTTP statuses that affirmatively reject a
// request. Timeout, conflict, and too-early responses remain ambiguous.
func KnownRejection(status int) bool {
	return status >= 400 &&
		status < 500 &&
		status != 408 &&
		status != 409 &&
		status != 425
}

// NotDispatched marks failures that occur before the HTTP attempt begins.
func NotDispatched(
	provider string,
	category llm.ErrorCategory,
	format string,
	args ...any,
) error {
	err := Failure(provider, category, format, args...)
	err.(*llm.Error).SendStatus = llm.SendStatusNotSent
	return err
}

// Failure attaches provider identity and category, preserving a cause with %w.
func Failure(
	provider string,
	category llm.ErrorCategory,
	format string,
	args ...any,
) error {
	if len(args) == 0 {
		return &llm.Error{
			Provider: provider,
			Category: category,
			Cause:    errors.New(format),
		}
	}
	return &llm.Error{
		Provider: provider,
		Category: category,
		Cause:    fmt.Errorf(format, args...),
	}
}

// MaxResponseBytes bounds a non-streaming model response body. It leaves room
// for inline images and audio while stopping a runaway body from exhausting
// memory.
const MaxResponseBytes = 256 << 20

// ReadResponse reads a successful non-streaming response body, failing once
// it passes MaxResponseBytes.
func ReadResponse(provider string, body io.Reader) ([]byte, error) {
	return readResponse(provider, body, MaxResponseBytes)
}

func readResponse(provider string, body io.Reader, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, ReadFailure(provider, err)
	}
	if len(raw) > limit {
		return nil, Failure(
			provider,
			llm.ErrorProtocol,
			"response body exceeds %d bytes",
			limit,
		)
	}
	return raw, nil
}

// Overlay retains private fields while replacing the public values with the
// caller's current content. Replay never silently crosses providers.
func Overlay(
	origin content.Origin,
	raw *llm.Opaque,
	fields map[string]any,
) (map[string]any, error) {
	if raw == nil {
		return fields, nil
	}
	if raw.Origin != origin {
		return nil, Failure(
			origin.Provider,
			llm.ErrorUnsupported,
			"cannot replay %s data from %s",
			raw.Type,
			raw.Origin.Provider,
		)
	}
	var result map[string]any
	if err := Decode(raw.Raw, &result); err != nil || result == nil {
		return nil, Failure(
			origin.Provider,
			llm.ErrorInvalidRequest,
			"invalid replay object",
		)
	}
	maps.Copy(result, fields)
	return result, nil
}

// Decode preserves JSON numbers when a provider object contains unknown fields.
func Decode(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("providerhttp: trailing provider JSON")
	}
	return nil
}

// Protocol reports a failure to decode provider output as a protocol error,
// keeping an error that is already an *llm.Error as it is.
func Protocol(provider string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*llm.Error](err); ok {
		return err
	}
	return Failure(provider, llm.ErrorProtocol, "decode response: %w", err)
}

// Emit isolates mutable preview data so a sink cannot change the final result.
func Emit(sink llm.Sink, d llm.Delta) {
	if sink == nil {
		return
	}
	if d.Content != nil {
		v := d.Content.Clone()
		d.Content = &v
	}
	if d.Usage != nil {
		d.Usage = d.Usage.Clone()
	}
	sink(d)
}
