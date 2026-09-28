package gemini

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

// ImageAPI identifies the Gemini GenerateContent image route.
const ImageAPI = API

// VideoAPI identifies the Veo long-running prediction route.
const VideoAPI = "google.predict_long_running"

// DefaultImageEndpoint is the live-qualified v1 GenerateContent image route.
const DefaultImageEndpoint = "https://generativelanguage.googleapis.com/v1/models/{model}:generateContent"

// DefaultVideoEndpoint is the model-specific Veo submission URL template.
const DefaultVideoEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/{model}:predictLongRunning"

const (
	maxImageResponseBytes = 64 << 20
	maxOperationBytes     = 4 << 20
	maxVideoBytes         = 512 << 20
)

// MediaConfig describes a Gemini API-key destination for image calls and Veo
// jobs. ImageModels and VideoModels contain application-owned evidence; an
// absent model follows CapabilityPolicy. Custom endpoints need an explicit
// Provider identity so their outputs cannot be confused with Google's service.
type MediaConfig struct {
	Provider         string
	APIKey           string
	ImageEndpoint    string
	VideoEndpoint    string
	Client           *http.Client
	ImageModels      map[string]media.ModelInfo
	VideoModels      map[string]media.ModelInfo
	CapabilityPolicy model.CapabilityPolicy
}

// MediaClient owns destination configuration, not job state or downloaded
// artifacts. It never retries a remote submission.
type MediaClient struct {
	config      MediaConfig
	imageModels map[string]media.ModelInfo
	videoModels map[string]media.ModelInfo
}

var (
	_ media.ImageClient    = (*MediaClient)(nil)
	_ media.VideoJobClient = (*MediaClient)(nil)
)

// NewMedia validates endpoint scope and snapshots model evidence. The Gemini
// API-key route is supported here; Vertex needs a separately qualified media
// destination and credential path.
func NewMedia(config MediaConfig) (*MediaClient, error) {
	if config.ImageEndpoint == "" {
		config.ImageEndpoint = DefaultImageEndpoint
	}
	if config.VideoEndpoint == "" {
		config.VideoEndpoint = DefaultVideoEndpoint
	}
	if config.Provider == "" {
		if config.ImageEndpoint != DefaultImageEndpoint ||
			config.VideoEndpoint != DefaultVideoEndpoint {
			return nil, errors.New("gemini: custom media endpoint requires provider identity")
		}
		config.Provider = "google"
	}
	// The Gemini key is read only for the "google" destination, so a client
	// for another provider never sends it elsewhere.
	if config.APIKey == "" && config.Provider == "google" {
		config.APIKey = cmp.Or(
			os.Getenv("GEMINI_API_KEY"),
			os.Getenv("GOOGLE_API_KEY"),
		)
	}
	if err := validateMediaEndpoint(
		config.ImageEndpoint,
		":generateContent",
	); err != nil {
		return nil, err
	}
	if err := validateMediaEndpoint(
		config.VideoEndpoint,
		":predictLongRunning",
	); err != nil {
		return nil, err
	}
	if !config.CapabilityPolicy.Valid() {
		return nil, errors.New("gemini: invalid unknown media capability policy")
	}
	if config.Client != nil {
		client := *config.Client
		config.Client = &client
	}
	p := &MediaClient{
		config:      config,
		imageModels: make(map[string]media.ModelInfo, len(config.ImageModels)),
		videoModels: make(map[string]media.ModelInfo, len(config.VideoModels)),
	}
	for name, info := range config.ImageModels {
		if err := p.validateModelInfo(
			name,
			info,
			ImageAPI,
			p.imageURL(name),
			true,
		); err != nil {
			return nil, err
		}
		p.imageModels[name] = info.Clone()
	}
	for name, info := range config.VideoModels {
		if err := p.validateModelInfo(
			name,
			info,
			VideoAPI,
			p.videoURL(name),
			false,
		); err != nil {
			return nil, err
		}
		p.videoModels[name] = info.Clone()
	}
	// The independent snapshots above are authoritative after construction.
	p.config.ImageModels = nil
	p.config.VideoModels = nil
	return p, nil
}

func validateMediaEndpoint(endpoint, suffix string) error {
	u, err := url.Parse(endpoint)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "https" &&
			u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!strings.HasSuffix(u.Path, "/models/{model}"+suffix) ||
		strings.Count(endpoint, "{model}") != 1 {
		return fmt.Errorf("gemini: invalid media endpoint for %s", suffix)
	}
	return nil
}

func (p *MediaClient) validateModelInfo(
	name string,
	info media.ModelInfo,
	api, endpoint string,
	image bool,
) error {
	if name == "" {
		return errors.New("gemini: media model name is required")
	}
	if err := info.Validate(); err != nil {
		return fmt.Errorf("gemini: media model %q: %w", name, err)
	}
	if info.Evidence.Model != name ||
		info.Evidence.API != api ||
		info.Evidence.Endpoint != endpoint {
		return fmt.Errorf(
			"gemini: media model %q evidence does not match its API endpoint",
			name,
		)
	}
	if image && info.Image == nil || !image && info.Video == nil {
		return fmt.Errorf(
			"gemini: media model %q lacks operation capabilities",
			name,
		)
	}
	return nil
}

// Provider returns the configured destination identity.
func (p *MediaClient) Provider() string {
	return p.config.Provider
}

// ImageModelInfo returns an independent snapshot of image evidence.
func (p *MediaClient) ImageModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := p.imageModels[name]
	return info.Clone(), ok
}

// VideoModelInfo returns an independent snapshot of video evidence.
func (p *MediaClient) VideoModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := p.videoModels[name]
	return info.Clone(), ok
}

func (p *MediaClient) imageURL(name string) string {
	return strings.ReplaceAll(
		p.config.ImageEndpoint,
		"{model}",
		url.PathEscape(name),
	)
}

func (p *MediaClient) videoURL(name string) string {
	return strings.ReplaceAll(
		p.config.VideoEndpoint,
		"{model}",
		url.PathEscape(name),
	)
}

func (p *MediaClient) origin(api, endpoint, name string) content.Origin {
	scopeData, _ := json.Marshal([]string{api, endpoint, p.config.APIKey})
	sum := sha256.Sum256(scopeData)
	return content.Origin{
		Provider: p.Provider(),
		API:      api,
		Model:    name,
		Scope:    fmt.Sprintf("%x", sum),
	}
}

func (p *MediaClient) imageOrigin(name string) content.Origin {
	return p.origin(ImageAPI, p.imageURL(name), name)
}

func (p *MediaClient) videoOrigin(name string) content.Origin {
	return p.origin(VideoAPI, p.videoURL(name), name)
}

func (p *MediaClient) failure(
	category media.ErrorCategory,
	format string,
	args ...any,
) error {
	return mediaFailure(p.Provider(), category, format, args...)
}

func (p *MediaClient) do(
	ctx context.Context,
	method, endpoint string,
	body []byte,
	contentType string,
	maxBytes int64,
	expectVideo bool,
) ([]byte, error) {
	// Image and video endpoints are configured separately, so the key check
	// follows the endpoint this operation actually calls.
	if err := mediaNoKey(
		p.config.APIKey,
		p.config.Client == nil && publicGeminiHost(endpoint),
	); err != nil {
		return nil, err
	}
	return doGeminiMedia(
		ctx,
		p.Provider(),
		p.config.APIKey,
		p.config.Client,
		method,
		endpoint,
		body,
		contentType,
		maxBytes,
		expectVideo,
	)
}

func doGeminiMedia(
	ctx context.Context,
	provider, apiKey string,
	configuredClient *http.Client,
	method, endpoint string,
	body []byte,
	contentType string,
	maxBytes int64,
	expectVideo bool,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 5 * time.Minute}
	if configuredClient != nil {
		client = *configuredClient
	}
	client.CheckRedirect = func(next *http.Request, prior []*http.Request) error {
		if method != http.MethodGet || !strings.Contains(
			endpoint,
			":download?",
		) || len(prior) > 3 ||
			next.URL.Scheme != prior[0].URL.Scheme {
			return http.ErrUseLastResponse
		}
		if next.URL.Host != prior[0].URL.Host {
			if prior[0].URL.Host != "generativelanguage.googleapis.com" ||
				next.URL.Scheme != "https" || !googleDownloadHost(next.URL.Hostname()) {
				return http.ErrUseLastResponse
			}
			// Files API may redirect to a Google storage URL. Its signed URL
			// carries its own authorization; never forward the Gemini API key.
			next.Header.Del("X-Goog-Api-Key")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, mediaFailure(provider, media.ErrorInvalidRequest, "%w", err)
	}
	req.Header.Set("X-Goog-Api-Key", apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, mediaFailure(provider, media.ErrorTransport, "%w", err)
	}
	defer resp.Body.Close()
	readLimit := maxBytes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		readLimit = min(readLimit, 1<<20)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, readLimit+1))
	if err != nil {
		return nil, mediaFailure(
			provider,
			media.ErrorTransport,
			"read response: %w",
			err,
		)
	}
	if int64(len(raw)) > readLimit {
		return nil, mediaFailure(
			provider,
			media.ErrorProtocol,
			"media response exceeds %d bytes",
			readLimit,
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		category := media.ErrorInvalidRequest
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			category = media.ErrorAuthentication
		case resp.StatusCode == 429:
			category = media.ErrorRateLimited
		case resp.StatusCode >= 500:
			category = media.ErrorOverloaded
		}
		return nil, &media.Error{
			Provider: provider,
			Category: category,
			Status:   resp.StatusCode,
			Body:     slices.Clone(raw),
			Retryable: resp.StatusCode == 408 ||
				resp.StatusCode == 429 ||
				resp.StatusCode >= 500,
			Cause: fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}
	if expectVideo {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType != "" && !strings.HasPrefix(
			mediaType,
			"video/",
		) && mediaType != "application/octet-stream" {
			return nil, mediaFailure(
				provider,
				media.ErrorProtocol,
				"video file download returned %q",
				mediaType,
			)
		}
	}
	return raw, nil
}

// publicGeminiHost reports whether endpoint is on Google's public Gemini API
// host, which needs an API key.
func publicGeminiHost(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Host == "generativelanguage.googleapis.com"
}

// mediaNoKey returns the error a media send reports when the public
// endpoint has no key, or nil. Construction and preparation still succeed.
func mediaNoKey(key string, public bool) error {
	if key != "" || !public {
		return nil
	}
	return &media.Error{
		Provider: "google",
		Category: media.ErrorAuthentication,
		Cause: fmt.Errorf(
			"%w: set GEMINI_API_KEY or GOOGLE_API_KEY or Config.APIKey",
			llm.ErrNoAPIKey,
		),
	}
}

func mediaFailure(
	provider string,
	category media.ErrorCategory,
	format string,
	args ...any,
) error {
	return &media.Error{
		Provider: provider,
		Category: category,
		Cause:    fmt.Errorf(format, args...),
	}
}

func googleDownloadHost(host string) bool {
	return host == "storage.googleapis.com" ||
		strings.HasSuffix(host, ".storage.googleapis.com") ||
		strings.HasSuffix(host, ".googleusercontent.com")
}
