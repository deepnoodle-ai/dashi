// Package images implements OpenAI's dedicated Images API. It keeps image
// generation and editing separate from Responses API hosted tools and performs
// one HTTP attempt for each prepared request. No agent or retry state lives
// here.
package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

// API identifies the Images wire format at a configured destination.
const API = "openai.images"

const defaultEndpoint = "https://api.openai.com/v1"

// Config identifies an Images API destination and its model evidence. Endpoint
// is the API base URL; generation and editing use fixed paths below it.
type Config struct {
	// Provider is the application's stable name for this destination. The
	// public endpoint defaults to "openai"; custom endpoints require a name.
	Provider string

	// APIKey defaults to OPENAI_API_KEY when New is called.
	APIKey string

	// Endpoint defaults to the public /v1 base URL.
	Endpoint string

	// Client supplies transport and timeout behavior. Redirects are disabled;
	// its transport must not hide retries.
	Client *http.Client

	// Models supplies exact-model evidence scoped to API and Endpoint. New
	// validates and copies it; empty means support is unknown.
	Models map[string]media.ModelInfo

	// CapabilityPolicy defaults to sending unknown support and recording it;
	// CapabilityStrict rejects it. Known rejection is never overridden.
	CapabilityPolicy model.CapabilityPolicy
}

// Client is safe for concurrent calls. It owns model evidence and the
// destination identity; the configured HTTP transport remains shared.
type Client struct {
	provider string
	endpoint string
	apiKey   string
	http     *http.Client
	scope    string
	models   map[string]media.ModelInfo
	unknown  model.CapabilityPolicy
}

var _ media.ImageClient = (*Client)(nil)

// New snapshots configuration. A custom endpoint needs a provider identity so
// recorded requests cannot be mistaken for the public OpenAI destination.
func New(config Config) (*Client, error) {
	if config.APIKey == "" {
		config.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if config.APIKey == "" {
		return nil, errors.New("images: API key is required")
	}
	if config.Endpoint == "" {
		config.Endpoint = defaultEndpoint
	}
	if config.Provider == "" {
		if config.Endpoint != defaultEndpoint {
			return nil, errors.New("images: custom endpoint requires provider identity")
		}
		config.Provider = "openai"
	}
	if strings.TrimSpace(config.Provider) != config.Provider ||
		config.Provider == "" {
		return nil, errors.New("images: invalid provider identity")
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, errors.New("images: invalid API base endpoint")
	}
	if !config.CapabilityPolicy.Valid() {
		return nil, errors.New("images: invalid capability policy")
	}
	catalog := make(map[string]media.ModelInfo, len(config.Models))
	for name, info := range config.Models {
		if err := info.Validate(); err != nil {
			return nil, fmt.Errorf("images: model %q: %w", name, err)
		}
		if info.Evidence.Model != name || info.Evidence.API != API ||
			info.Evidence.Endpoint != config.Endpoint {
			return nil, fmt.Errorf(
				"images: model %q evidence does not match this endpoint",
				name,
			)
		}
		catalog[name] = info.Clone()
	}
	scopeBytes, _ := json.Marshal([]string{API, config.Endpoint, config.APIKey})
	return &Client{
		provider: config.Provider,
		endpoint: config.Endpoint,
		apiKey:   config.APIKey,
		http:     config.Client,
		scope:    fmt.Sprintf("%x", sha256.Sum256(scopeBytes)),
		models:   catalog,
		unknown:  config.CapabilityPolicy.Resolved(),
	}, nil
}

// Provider returns the configured destination name.
func (c *Client) Provider() string {
	return c.provider
}

// API returns the Images wire format name.
func (*Client) API() string {
	return API
}

// ModelInfo returns an independently owned capability snapshot when supplied.
func (c *Client) ModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := c.models[name]
	return info.Clone(), ok
}

// PrepareImage validates caller intent, checks model evidence, and freezes the
// exact body SendImage will dispatch. It performs no network request.
func (c *Client) PrepareImage(ctx context.Context,
	input media.ImageInput) (media.ImageRequest, error) {
	return c.PrepareImageWithOptions(ctx, input, Options{})
}

// PrepareImageWithOptions freezes OpenAI-specific controls with the image
// request. The returned request owns both the exact wire body and options.
func (c *Client) PrepareImageWithOptions(ctx context.Context,
	input media.ImageInput, options Options) (media.ImageRequest, error) {
	options = options.clone()
	if err := ctx.Err(); err != nil {
		return media.ImageRequest{}, err
	}
	if err := input.Validate(); err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	prompt, err := encodePrompt(input.Prompt)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	info, known := c.ModelInfo(input.Model)
	var capabilities media.ImageCapabilities
	if known && info.Image != nil {
		capabilities = *info.Image
	}
	unverified, err := capabilities.Check(input, c.unknown)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	effective := input.Clone()
	var adjustments []media.Adjustment
	if effective.Settings.Count == 0 {
		effective.Settings.Count = 1
		adjustments = append(
			adjustments,
			media.Adjustment{
				Setting:   "count",
				Effective: "1",
				Reason:    "Images API default made explicit in the prepared body",
			},
		)
	}
	body, contentType, err := encodeImage(effective, prompt, options)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	optionBytes, err := json.Marshal(options)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	snapshot := media.ImageRequestSnapshot{
		Origin:           c.origin(input.Model),
		Input:            input,
		Effective:        effective.Settings,
		Body:             body,
		ContentType:      contentType,
		ProviderOptions:  optionBytes,
		Adjustments:      adjustments,
		CapabilityPolicy: c.unknown,
		Unverified:       unverified,
	}
	if known {
		snapshot.ModelInfo = &info
	}
	request, err := media.NewImageRequest(snapshot)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	return request, nil
}

// SendImage makes one HTTP attempt against the prepared destination. A
// transport error does not establish whether OpenAI received the request.
func (c *Client) SendImage(ctx context.Context,
	request media.ImageRequest) (media.Result, error) {
	input := request.Input()
	if request.Origin() != c.origin(input.Model) {
		return media.Result{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("prepared request belongs to another destination"),
		)
	}
	if err := ctx.Err(); err != nil {
		return media.Result{}, err
	}
	path := "images/generations"
	if input.Operation == media.ImageEdit {
		path = "images/edits"
	}
	endpoint, err := url.JoinPath(c.endpoint, path)
	if err != nil {
		return media.Result{}, c.failure(media.ErrorInvalidRequest, err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(request.Body()),
	)
	if err != nil {
		return media.Result{}, c.failure(media.ErrorInvalidRequest, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpRequest.Header.Set("Content-Type", request.ContentType())
	httpClient := http.Client{Timeout: 5 * time.Minute}
	if c.http != nil {
		httpClient = *c.http
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return media.Result{}, c.failure(media.ErrorTransport, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return media.Result{}, c.httpError(response)
	}
	const maxResponseBytes = 256 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return media.Result{}, c.failure(media.ErrorTransport, err)
	}
	if len(body) > maxResponseBytes {
		return media.Result{}, c.failure(
			media.ErrorProtocol,
			errors.New("image response exceeds 256 MiB"),
		)
	}
	result, err := decodeImageResponse(
		body,
		input,
		request.Origin(),
		response.Header.Get("X-Request-Id"),
	)
	if err != nil {
		return media.Result{}, c.failure(media.ErrorProtocol, err)
	}
	return result, nil
}

func (c *Client) origin(modelName string) content.Origin {
	return content.Origin{
		Provider: c.provider,
		API:      API,
		Model:    modelName,
		Scope:    c.scope,
	}
}

func (c *Client) failure(category media.ErrorCategory, err error) error {
	return &media.Error{Provider: c.provider, Category: category, Cause: err}
}

func (c *Client) httpError(response *http.Response) error {
	const maxErrorBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes))
	if err != nil {
		return c.failure(media.ErrorTransport, err)
	}
	providerError := providerhttp.NewError(
		response.StatusCode,
		string(body),
		providerhttp.WithErrorHeader(response.Header),
	)
	category := media.ErrorInvalidRequest
	switch {
	case response.StatusCode == http.StatusUnauthorized ||
		response.StatusCode == http.StatusForbidden:
		category = media.ErrorAuthentication
	case response.StatusCode == http.StatusTooManyRequests:
		category = media.ErrorRateLimited
	case response.StatusCode >= http.StatusInternalServerError:
		category = media.ErrorOverloaded
	}
	retryAfter, _ := providerError.RetryAfter()
	return &media.Error{
		Provider:   c.provider,
		Category:   category,
		Status:     response.StatusCode,
		Code:       providerError.Code(),
		Body:       slices.Clone(body),
		RetryAfter: retryAfter,
		Retryable:  retryableStatus(response.StatusCode, providerError.Code()),
	}
}

func retryableStatus(status int, code string) bool {
	switch code {
	case "credit_balance_exhausted", "insufficient_quota",
		"organization_spend_limit_exceeded",
		"organization_usage_limit_exceeded",
		"project_spend_limit_exceeded":
		return false
	}
	switch status {
	case 429, 500, 502, 503, 504, 520, 529:
		return true
	}
	return false
}
