// Package images implements Meta's Images API for Muse Image
// (https://dev.meta.ai/docs/image-generation). Each prepared request is sent
// in one HTTP attempt; no agent or retry state lives here.
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

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

const (
	// API identifies Meta's Images wire format at a configured destination.
	API = "meta.images"
	// FilesAPI identifies Meta Files API IDs used as edit inputs.
	FilesAPI = "meta.files"
	// DefaultEndpoint is Meta's Model API base URL.
	DefaultEndpoint = "https://api.meta.ai/v1"
)

// Config identifies an Images API destination and its model evidence.
type Config struct {
	// Provider is the application's stable name for this destination. The
	// public endpoint defaults to "meta"; a custom endpoint requires a name.
	Provider string

	// APIKey defaults to MODEL_API_KEY, then META_API_KEY, read only when
	// Provider is "meta". With no key for the public endpoint, New and
	// PrepareImage still succeed and SendImage fails before dispatch with an
	// error wrapping llm.ErrNoAPIKey.
	APIKey string

	// Endpoint defaults to DefaultEndpoint.
	Endpoint string

	// Client supplies transport and timeout behavior. Redirects are disabled;
	// its transport must not hide retries.
	Client *http.Client

	// Models adds or replaces exact-model evidence scoped to API and
	// Endpoint. The public endpoint has built-in evidence for muse-image-1.0.
	Models map[string]media.ModelInfo

	// CapabilityPolicy defaults to sending unknown support and recording it;
	// CapabilityStrict rejects it. Known rejection is never overridden.
	CapabilityPolicy model.CapabilityPolicy
}

// Client is safe for concurrent calls.
type Client struct {
	provider string
	endpoint string
	apiKey   string
	noKey    error
	http     *http.Client
	scope    string
	files    content.Origin
	models   map[string]media.ModelInfo
	unknown  model.CapabilityPolicy
}

var _ media.ImageClient = (*Client)(nil)

// New snapshots configuration.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.Provider == "" {
		if config.Endpoint != DefaultEndpoint {
			return nil, errors.New("images: custom endpoint requires provider identity")
		}
		config.Provider = "meta"
	}
	if strings.TrimSpace(config.Provider) != config.Provider {
		return nil, errors.New("images: invalid provider identity")
	}
	// Meta keys are read only for the meta destination, so a client for
	// another provider never sends them elsewhere.
	if config.APIKey == "" && config.Provider == "meta" {
		config.APIKey = os.Getenv("MODEL_API_KEY")
		if config.APIKey == "" {
			config.APIKey = os.Getenv("META_API_KEY")
		}
	}
	var noKey error
	if config.APIKey == "" && config.Endpoint == DefaultEndpoint &&
		config.Client == nil {
		noKey = fmt.Errorf(
			"%w: set MODEL_API_KEY, META_API_KEY, or Config.APIKey",
			llm.ErrNoAPIKey,
		)
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
	models := map[string]media.ModelInfo{}
	if config.Endpoint == DefaultEndpoint {
		models[museImage] = builtInModel(config.Endpoint)
	}
	for name, info := range config.Models {
		if err := info.Validate(); err != nil {
			return nil, fmt.Errorf("images: model %q: %w", name, err)
		}
		if info.Image == nil {
			return nil, fmt.Errorf("images: model %q has no image capabilities", name)
		}
		if info.Evidence.Model != name || info.Evidence.API != API ||
			info.Evidence.Endpoint != config.Endpoint {
			return nil, fmt.Errorf(
				"images: model %q evidence does not match this endpoint",
				name,
			)
		}
		models[name] = info.Clone()
	}
	return &Client{
		provider: config.Provider,
		endpoint: config.Endpoint,
		apiKey:   config.APIKey,
		noKey:    noKey,
		http:     config.Client,
		scope:    credentialScope(API, config.Endpoint, config.APIKey),
		files: content.Origin{
			Provider: config.Provider,
			API:      FilesAPI,
			Scope:    credentialScope(FilesAPI, config.Endpoint, config.APIKey),
		},
		models:  models,
		unknown: config.CapabilityPolicy.Resolved(),
	}, nil
}

const museImage = "muse-image-1.0"

// builtInModel records Muse Image limits from Meta's docs. Muse accepts any
// aspect ratio up to 16:1, which a list cannot state, so aspect ratios stay
// unclaimed: the adjust policy sends them and records them as unverified, and
// the adapter itself rejects ratios beyond 16:1.
func builtInModel(endpoint string) media.ModelInfo {
	return media.ModelInfo{
		Image: &media.ImageCapabilities{
			Generate:        model.SupportSupported,
			Edit:            model.SupportSupported,
			Mask:            model.SupportUnsupported,
			SizeBounds:      &media.ImageSizeBounds{MaxAspectRatio: maxAspectRatio},
			OutputMIMETypes: []string{"image/webp", "image/png", "image/jpeg"},
			MaxCount:        maxCount,
		},
		Evidence: model.ModelEvidence{
			Model:       museImage,
			API:         API,
			Endpoint:    endpoint,
			Owner:       "Dashi",
			Source:      "https://dev.meta.ai/docs/image-generation",
			Method:      model.EvidenceDocumentation,
			QualifiedAt: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC),
		},
	}
}

func credentialScope(api, endpoint, credential string) string {
	data, _ := json.Marshal([]string{api, endpoint, credential})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// Provider returns the configured destination name.
func (c *Client) Provider() string {
	return c.provider
}

// API returns the Images wire format name.
func (*Client) API() string {
	return API
}

// FileOrigin binds a Meta Files API ID to this endpoint and credential.
// Set Source.Origin to this value to send an uploaded file as an edit input;
// the client does not upload or check the file.
func (c *Client) FileOrigin() content.Origin {
	return c.files
}

// ModelInfo returns an independently owned capability snapshot.
func (c *Client) ModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := c.models[name]
	return info.Clone(), ok
}

// PrepareImage validates the input, checks model evidence, and freezes the
// exact body SendImage will dispatch. It performs no network request.
func (c *Client) PrepareImage(ctx context.Context,
	input media.ImageInput) (media.ImageRequest, error) {
	return c.PrepareImageWithOptions(ctx, input, Options{})
}

// PrepareImageWithOptions freezes Meta-specific controls with the request.
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
	if known {
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
		adjustments = append(adjustments, media.Adjustment{
			Setting:   "count",
			Effective: "1",
			Reason:    "Images API default made explicit in the prepared body",
		})
	}
	body, adjustment, err := encodeImage(effective, prompt, options, c.files)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	if adjustment != nil {
		adjustments = append(adjustments, *adjustment)
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
		ContentType:      "application/json",
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
// transport error does not establish whether Meta received the request.
func (c *Client) SendImage(ctx context.Context,
	request media.ImageRequest) (media.Result, error) {
	if c.noKey != nil {
		return media.Result{}, c.failure(media.ErrorAuthentication, c.noKey)
	}
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

// httpError maps Meta's documented statuses: 503 is a retryable backend
// failure, while 500 is non-retryable or unclassified.
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
	case response.StatusCode == http.StatusUnsupportedMediaType:
		category = media.ErrorUnsupported
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
		Retryable:  retryableStatus(response.StatusCode),
	}
}

func retryableStatus(status int) bool {
	switch status {
	case 429, 502, 503, 504:
		return true
	}
	return false
}
