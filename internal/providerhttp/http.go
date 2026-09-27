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
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
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

	// Headers is an owned template cloned for each dispatch.
	Headers http.Header
	scope   string
	models  map[string]llm.ModelInfo
	unknown llm.UnknownPolicy
}

// NewClient snapshots configuration and derives a replay namespace from the
// endpoint and credential. Credential rotation conservatively invalidates old
// private replay data. Neither the credential nor endpoint appears in Origin.
func NewClient(provider, api, endpoint, credential string, httpClient *http.Client,
	headers http.Header, models map[string]llm.ModelInfo, unknown llm.UnknownPolicy) (Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return Client{}, errors.New("providerhttp: invalid endpoint")
	}
	if strings.TrimSpace(provider) == "" || api == "" {
		return Client{}, errors.New("providerhttp: provider identity and API are required")
	}
	if unknown != llm.UnknownReject && unknown != llm.UnknownAllow {
		return Client{}, errors.New("providerhttp: invalid unknown capability policy")
	}
	catalog := make(map[string]llm.ModelInfo, len(models))
	for model, info := range models {
		if model == "" {
			return Client{}, errors.New("providerhttp: model name is required")
		}
		if err := info.Validate(); err != nil {
			return Client{}, fmt.Errorf("providerhttp: model %q: %w", model, err)
		}
		if e := info.Evidence; e.Model != model || e.API != api || e.Endpoint != endpoint {
			return Client{}, fmt.Errorf("providerhttp: model %q evidence does not match model, API, and endpoint", model)
		}
		catalog[model] = info.Clone()
	}
	scopeData, _ := json.Marshal([]string{api, endpoint, credential})
	scope := fmt.Sprintf("%x", sha256.Sum256(scopeData))
	return Client{Provider: provider, API: api, Endpoint: endpoint, HTTP: httpClient,
		Headers: headers.Clone(), scope: scope, models: catalog, unknown: unknown}, nil
}

// Origin binds model replay to the configured API, endpoint, and credentials.
func (c Client) Origin(model string) content.Origin {
	return content.Origin{Provider: c.Provider, API: c.API, Model: model, Scope: c.scope}
}

// ModelInfo returns an independent snapshot and reports whether it was known.
func (c Client) ModelInfo(model string) (llm.ModelInfo, bool) {
	info, ok := c.models[model]
	return info.Clone(), ok
}

// Prepare checks effective capabilities and snapshots the single wire body.
func (c Client) Prepare(requested, effective llm.Input, body map[string]any, adjustments []llm.Adjustment, changes []llm.ContentChange) (llm.Request, error) {
	info, known := c.ModelInfo(requested.Model)
	unverified, err := info.Capabilities.Check(effective, c.unknown)
	if err != nil {
		return llm.Request{}, Failure(c.Provider, llm.ErrorUnsupported, "%w", err)
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
	return llm.NewRequest(llm.RequestSnapshot{Origin: c.Origin(requested.Model),
		Input: requested, Effective: effective.Settings, Body: raw,
		Adjustments: adjustments, ContentChanges: changes, ModelInfo: snapshot,
		UnknownCapabilities: c.unknown, Unverified: unverified})
}

// Post performs at most one dispatch, without retries or redirects. Errors
// include retry hints for the caller that owns attempt recording and admission.
func (c Client) Post(ctx context.Context, p llm.Request) (*http.Response, error) {
	request := p.Input()
	if p.Origin() != c.Origin(request.Model) {
		return nil, NotDispatched(c.Provider, llm.ErrorInvalidRequest, "prepared request belongs to another destination")
	}
	if err := ctx.Err(); err != nil {
		return nil, NotDispatched(c.Provider, llm.ErrorTransport, "%w", err)
	}
	client := http.Client{Timeout: 5 * time.Minute}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(p.Body()))
	if err != nil {
		return nil, NotDispatched(c.Provider, llm.ErrorInvalidRequest, "%w", err)
	}
	r.Header = c.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Content-Type", "application/json")
	if request.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(r)
	if err != nil {
		return nil, Failure(c.Provider, llm.ErrorTransport, "%w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, Failure(c.Provider, llm.ErrorTransport, "%w", err)
	}
	pe := NewError(resp.StatusCode, string(body), WithErrorHeader(resp.Header))
	category := llm.ErrorInvalidRequest
	switch {
	case pe.statusCode == 401 || pe.statusCode == 403:
		category = llm.ErrorAuthentication
	case pe.statusCode == 429:
		category = llm.ErrorRateLimited
	case pe.statusCode >= 500:
		category = llm.ErrorOverloaded
	}
	disposition := llm.FailureUnknown
	if KnownRejection(pe.statusCode) {
		disposition = llm.FailureRejected
	}
	return nil, &llm.Error{Provider: c.Provider, Category: category, Disposition: disposition, Status: pe.statusCode,
		Code: pe.code, Body: body, RetryAfter: pe.retryAfter,
		Retryable: shouldRetry(pe.statusCode, pe.code), Cause: pe}
}

// KnownRejection is limited to HTTP statuses that affirmatively reject a
// request. Timeout, conflict, and too-early responses remain ambiguous.
func KnownRejection(status int) bool {
	return status >= 400 && status < 500 && status != 408 && status != 409 && status != 425
}

// NotDispatched marks failures that occur before the HTTP attempt begins.
func NotDispatched(provider string, category llm.ErrorCategory, format string, args ...any) error {
	err := Failure(provider, category, format, args...)
	err.(*llm.Error).Disposition = llm.FailureNotDispatched
	return err
}

// Failure attaches provider identity and category, preserving a cause with %w.
func Failure(provider string, category llm.ErrorCategory, format string, args ...any) error {
	if len(args) == 0 {
		return &llm.Error{Provider: provider, Category: category, Cause: errors.New(format)}
	}
	return &llm.Error{Provider: provider, Category: category, Cause: fmt.Errorf(format, args...)}
}

// Overlay retains private fields while replacing the public values with the
// caller's current content. Replay never silently crosses providers.
func Overlay(origin content.Origin, raw *llm.Opaque, fields map[string]any) (map[string]any, error) {
	if raw == nil {
		return fields, nil
	}
	if raw.Origin != origin {
		return nil, Failure(origin.Provider, llm.ErrorUnsupported, "cannot replay %s data from %s", raw.Type, raw.Origin.Provider)
	}
	var result map[string]any
	if err := Decode(raw.Raw, &result); err != nil || result == nil {
		return nil, Failure(origin.Provider, llm.ErrorInvalidRequest, "invalid replay object")
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
		d.Usage = d.Usage.Copy()
	}
	sink(d)
}
