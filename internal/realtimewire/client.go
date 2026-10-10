package realtimewire

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/realtime"
)

type Config struct {
	APIKey, Endpoint string
	Client           *http.Client
	Dial             realtime.DialFunc
}
type Client struct {
	config Config
	google bool
}

func (c *Client) String() string { return "realtime.Client{credentials redacted}" }
func New(config Config, google bool) (*Client, error) {
	if config.Endpoint == "" {
		if google {
			config.Endpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
		} else {
			config.Endpoint = "wss://api.x.ai/v1/realtime"
		}
	}
	u, e := url.Parse(config.Endpoint)
	if e != nil || u.Host == "" || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, realtime.Fail(realtime.ErrorInvalidRequest)
	}
	if config.APIKey == "" && config.Endpoint == defaultEndpoint(google) {
		if google {
			config.APIKey = os.Getenv("GEMINI_API_KEY")
			if config.APIKey == "" {
				config.APIKey = os.Getenv("GOOGLE_API_KEY")
			}
		} else {
			config.APIKey = os.Getenv("XAI_API_KEY")
		}
	}
	if config.APIKey != strings.TrimSpace(config.APIKey) {
		return nil, realtime.Fail(realtime.ErrorInvalidRequest)
	}
	if config.Dial == nil {
		config.Dial = Dial
	}
	return &Client{config: config, google: google}, nil
}
func (c *Client) Connect(ctx context.Context, in realtime.Config) (realtime.Session, error) {
	if ctx.Err() != nil {
		return nil, realtime.Fail(realtime.ErrorCanceled)
	}
	if e := in.Validate(); e != nil {
		return nil, e
	}
	if c.config.APIKey == "" && c.config.Endpoint == defaultEndpoint(c.google) {
		return nil, realtime.Fail(realtime.ErrorAuthentication)
	}
	model, provider := "grok-voice-think-fast-2.0", "grok"
	rate := 24000
	if c.google {
		model, provider = "gemini-3.8-live", "google"
		rate = 16000
	}
	if in.Model == "" {
		in.Model = model
	}
	if in.Model != model {
		return nil, realtime.Fail(realtime.ErrorUnsupported)
	}
	input := content.AudioFormat{Encoding: "pcm_s16le", SampleRateHz: rate, Channels: 1}
	output := content.AudioFormat{Encoding: "pcm_s16le", SampleRateHz: 24000, Channels: 1}
	if in.InputFormat != (content.AudioFormat{}) && in.InputFormat != input || in.OutputFormat != (content.AudioFormat{}) && in.OutputFormat != output {
		return nil, realtime.Fail(realtime.ErrorUnsupported)
	}
	headers := http.Header{}
	endpoint := c.config.Endpoint
	if c.google {
		headers.Set("x-goog-api-key", c.config.APIKey)
	} else {
		if c.config.APIKey != "" {
			headers.Set("Authorization", "Bearer "+c.config.APIKey)
		}
		endpoint += "?model=" + url.QueryEscape(model)
	}
	conn, e := c.config.Dial(ctx, endpoint, headers, c.config.Client)
	if e != nil {
		if _, ok := e.(*realtime.Error); ok {
			return nil, e
		}
		return nil, realtime.Fail(realtime.ErrorTransport)
	}
	s := NewSession(ctx, conn, realtime.SessionInfo{Provider: provider, Model: model, InputFormat: input, OutputFormat: output, NeedsResponseAfterTools: !c.google})
	for _, t := range in.Tools {
		s.allowed[t.Name] = true
	}
	tools := []any{}
	var setup any
	if c.google {
		if in.Voice == "" {
			in.Voice = "Kore"
		}
		for _, t := range in.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema, "behavior": "NON_BLOCKING"})
		}
		body := map[string]any{"model": "models/" + model, "generationConfig": map[string]any{"responseModalities": []string{"AUDIO"}, "speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": in.Voice}}}}, "inputAudioTranscription": map[string]any{}, "outputAudioTranscription": map[string]any{}}
		if in.Instructions != "" {
			body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": in.Instructions}}}
		}
		if len(tools) > 0 {
			body["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		}
		setup = map[string]any{"setup": body}
	} else {
		if in.Voice == "" {
			in.Voice = "eve"
		}
		for _, t := range in.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema})
		}
		format := map[string]any{"type": "audio/pcm", "rate": 24000}
		setup = map[string]any{"type": "session.update", "session": map[string]any{"instructions": in.Instructions, "voice": in.Voice, "tools": tools, "turn_detection": map[string]any{"type": "server_vad"}, "audio": map[string]any{"input": map[string]any{"format": format, "transcription": map[string]any{"model": "grok-transcribe"}}, "output": map[string]any{"format": format}}}}
	}
	setupCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if e := s.Setup(setupCtx, setup, "session.updated"); e != nil {
		return nil, e
	}
	return s, nil
}

func defaultEndpoint(google bool) string {
	if google {
		return "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	}
	return "wss://api.x.ai/v1/realtime"
}
func (c *Client) GoString() string { return c.String() }
