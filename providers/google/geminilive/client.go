// Package geminilive implements native Gemini Live audio conversations without
// coupling the application to the optional batch Gemini SDK module.
package geminilive

import "github.com/deepnoodle-ai/dashi/internal/realtimewire"

const DefaultModel = "gemini-3.8-live"
const DefaultEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

type Config = realtimewire.Config
type Client struct{ *realtimewire.Client }

func New(config Config) (*Client, error) {
	c, e := realtimewire.New(config, true)
	if e != nil {
		return nil, e
	}
	return &Client{c}, nil
}
