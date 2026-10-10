// Package voice implements xAI's native bidirectional voice conversation API.
package voice

import "github.com/deepnoodle-ai/dashi/internal/realtimewire"

const DefaultModel = "grok-voice-think-fast-2.0"
const DefaultEndpoint = "wss://api.x.ai/v1/realtime"

type Config = realtimewire.Config
type Client struct{ *realtimewire.Client }

func New(config Config) (*Client, error) {
	c, e := realtimewire.New(config, false)
	if e != nil {
		return nil, e
	}
	return &Client{c}, nil
}
