// Package imagine implements xAI's dedicated image and video generation APIs.
// Video requests use a resumable provider job; this package does not poll in a
// loop or store generated output for the caller.
package imagine

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

// DefaultEndpoint is the base URL for xAI's dedicated media APIs.
const DefaultEndpoint = "https://api.x.ai/v1"

const (
	// ImageAPI identifies the xAI image generation and editing wire format.
	ImageAPI = "xai.images"
	// VideoAPI identifies the xAI video job wire format.
	VideoAPI = "xai.videos"
)

// Config defines one xAI destination. Models holds application-owned evidence
// for an exact model, API, and base endpoint. A model without evidence requires
// explicit UnknownAllow before its operation can be prepared.
type Config struct {
	// Provider is the application's stable destination name. It defaults to
	// "grok" only for DefaultEndpoint; custom endpoints require a name.
	Provider string

	// APIKey defaults to XAI_API_KEY, then GROK_API_KEY.
	APIKey string

	// Endpoint is the base URL shared by image and video routes.
	Endpoint string

	// Client supplies transport and timeout behavior without hidden retries.
	Client *http.Client

	// Models contains evidence keyed by exact model ID.
	Models map[string]media.ModelInfo

	// UnknownCapabilities controls how absent model claims are handled.
	UnknownCapabilities model.UnknownPolicy
}

// Client prepares and executes dedicated xAI image calls and video jobs.
// It snapshots configuration; the configured HTTP client remains shared.
type Client struct {
	provider string
	endpoint string
	apiKey   string
	http     *http.Client
	models   map[string]media.ModelInfo
	unknown  model.UnknownPolicy
	image    content.Origin
	video    content.Origin
}

// New validates and snapshots one destination. Custom endpoints are useful for
// testing and private gateways; they must be absolute HTTP(S) base URLs.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("XAI_API_KEY")
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("GROK_API_KEY")
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("imagine: endpoint must be an absolute HTTP(S) base URL")
	}
	endpoint := strings.TrimRight(u.String(), "/")
	if config.Provider == "" {
		if endpoint != DefaultEndpoint {
			return nil, errors.New("imagine: custom endpoint requires provider identity")
		}
		config.Provider = "grok"
	}
	if strings.TrimSpace(config.Provider) == "" {
		return nil, errors.New("imagine: provider identity is required")
	}
	if config.UnknownCapabilities != model.UnknownReject &&
		config.UnknownCapabilities != model.UnknownAllow {
		return nil, errors.New("imagine: invalid unknown capability policy")
	}
	models := make(map[string]media.ModelInfo, len(config.Models))
	for name, info := range config.Models {
		if name == "" {
			return nil, errors.New("imagine: model name is required")
		}
		if err := info.Validate(); err != nil {
			return nil, fmt.Errorf("imagine: model %q: %w", name, err)
		}
		if e := info.Evidence; e.Model != name || e.Endpoint != endpoint ||
			(e.API != ImageAPI && e.API != VideoAPI) {
			return nil, fmt.Errorf("imagine: model %q evidence does not match destination", name)
		}
		imageShape := info.Evidence.API == ImageAPI &&
			(info.Image == nil || info.Video != nil || info.Audio != nil)
		videoShape := info.Evidence.API == VideoAPI &&
			(info.Video == nil || info.Image != nil || info.Audio != nil)
		if imageShape || videoShape {
			return nil, fmt.Errorf("imagine: model %q capability does not match its API", name)
		}
		models[name] = info.Clone()
	}
	return &Client{
		provider: config.Provider,
		endpoint: endpoint,
		apiKey:   config.APIKey,
		http:     config.Client,
		models:   models,
		unknown:  config.UnknownCapabilities,
		image: content.Origin{
			Provider: config.Provider,
			API:      ImageAPI,
			Scope:    credentialScope(ImageAPI, endpoint, config.APIKey),
		},
		video: content.Origin{
			Provider: config.Provider,
			API:      VideoAPI,
			Scope:    credentialScope(VideoAPI, endpoint, config.APIKey),
		},
	}, nil
}

// ModelInfo returns an independent copy of the configured model evidence.
// False means the caller did not supply evidence for this model.
func (c *Client) ModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := c.models[name]
	return info.Clone(), ok
}

func credentialScope(api, endpoint, credential string) string {
	data, _ := json.Marshal([]string{api, endpoint, credential})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (c *Client) origin(api, name string) content.Origin {
	if api == ImageAPI {
		origin := c.image
		origin.Model = name
		return origin
	}
	origin := c.video
	origin.Model = name
	return origin
}
