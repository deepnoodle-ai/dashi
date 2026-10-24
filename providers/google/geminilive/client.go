// Package geminilive implements native Gemini Live audio conversations without
// coupling the application to the optional batch Gemini SDK module.
package geminilive

import (
	"net/http"

	"github.com/deepnoodle-ai/dashi/internal/realtimewire"
	"github.com/deepnoodle-ai/dashi/realtime"
	"golang.org/x/oauth2"
)

const DefaultModel = "gemini-3.8-live"
const DefaultEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

// Config chooses the destination. Without Vertex, the client uses the Gemini
// Developer API with APIKey, or GEMINI_API_KEY or GOOGLE_API_KEY at the
// default endpoint.
type Config struct {
	APIKey, Endpoint string
	Client           *http.Client
	Dial             realtime.DialFunc
	// Vertex selects Gemini Live on Vertex AI. APIKey must then be empty.
	Vertex *VertexConfig
}

// VertexConfig names a Google Cloud project and location and the source of
// its OAuth access tokens, such as google.DefaultTokenSource with the
// cloud-platform scope. Connect takes one token per connection and never
// logs it; a failure reports only the OAuth error code. Token takes no
// context, so the token source must bound its own fetch time. The model must
// be available to the project in that location.
type VertexConfig struct {
	Project, Location string
	TokenSource       oauth2.TokenSource
}

type Client struct{ *realtimewire.Client }

func New(config Config) (*Client, error) {
	wire := realtimewire.Config{APIKey: config.APIKey, Endpoint: config.Endpoint, Client: config.Client, Dial: config.Dial}
	var c *realtimewire.Client
	var e error
	if config.Vertex != nil {
		c, e = realtimewire.NewVertex(wire, realtimewire.Vertex{Project: config.Vertex.Project, Location: config.Vertex.Location, TokenSource: config.Vertex.TokenSource})
	} else {
		c, e = realtimewire.New(wire, true)
	}
	if e != nil {
		return nil, e
	}
	return &Client{c}, nil
}
