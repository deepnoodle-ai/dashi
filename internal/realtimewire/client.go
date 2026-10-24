package realtimewire

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/realtime"
	"golang.org/x/oauth2"
)

type Config struct {
	APIKey, Endpoint string
	Client           *http.Client
	Dial             realtime.DialFunc
}
type Client struct {
	config Config
	google bool
	vertex *Vertex
}

// Vertex selects Gemini Live on Vertex AI: a regional endpoint, an OAuth
// bearer token in place of an API key, and a project-scoped model name.
type Vertex struct {
	Project, Location string
	TokenSource       oauth2.TokenSource
}

// vertexName limits project and location to the characters Google allows,
// since the location becomes part of a host name.
var vertexName = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]{0,127}$`)

// NewVertex is New for Gemini on Vertex AI. config.Endpoint overrides the
// regional endpoint, for tests; config.APIKey must be empty.
func NewVertex(config Config, vertex Vertex) (*Client, error) {
	if vertex.TokenSource == nil || config.APIKey != "" || !vertexName.MatchString(vertex.Project) || !vertexName.MatchString(vertex.Location) || strings.ContainsAny(vertex.Location, ".:") {
		return nil, realtime.Failf(realtime.ErrorInvalidRequest, "vertex needs a project, a location and a token source, and no API key")
	}
	if config.Endpoint == "" {
		host := vertex.Location + "-aiplatform.googleapis.com"
		if vertex.Location == "global" {
			host = "aiplatform.googleapis.com"
		}
		config.Endpoint = "wss://" + host + "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"
	}
	if !validEndpoint(config.Endpoint) {
		return nil, realtime.Fail(realtime.ErrorInvalidRequest)
	}
	if config.Dial == nil {
		config.Dial = Dial
	}
	return &Client{config: config, google: true, vertex: &vertex}, nil
}

func validEndpoint(endpoint string) bool {
	u, e := url.Parse(endpoint)
	return e == nil && u.Host != "" && u.Scheme == "wss" && u.User == nil && u.Fragment == "" && u.RawQuery == ""
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
	if !validEndpoint(config.Endpoint) {
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
	if c.vertex == nil && c.config.APIKey == "" && c.config.Endpoint == defaultEndpoint(c.google) {
		return nil, realtime.Fail(realtime.ErrorAuthentication)
	}
	model, provider := "grok-voice-3.0", "grok"
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
	if c.vertex != nil {
		// A fresh token per Connect; the token source caches and refreshes.
		// Only an OAuth error code passes through, never the error's text.
		token, e := c.vertex.TokenSource.Token()
		if e != nil || !token.Valid() || token.AccessToken != strings.TrimSpace(token.AccessToken) {
			fail := &realtime.Error{Category: realtime.ErrorAuthentication, Cause: errors.New("vertex token source gave no usable token")}
			var retrieve *oauth2.RetrieveError
			if errors.As(e, &retrieve) {
				fail.Code = truncate(retrieve.ErrorCode, 128)
			}
			return nil, fail
		}
		headers.Set("Authorization", "Bearer "+token.AccessToken)
	} else if c.google {
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
		name := "models/" + model
		if c.vertex != nil {
			name = "projects/" + c.vertex.Project + "/locations/" + c.vertex.Location + "/publishers/google/models/" + model
		}
		body := map[string]any{"model": name, "generationConfig": map[string]any{"responseModalities": []string{"AUDIO"}, "speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": in.Voice}}}}, "inputAudioTranscription": map[string]any{}, "outputAudioTranscription": map[string]any{}}
		if in.Instructions != "" {
			body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": in.Instructions}}}
		}
		if len(tools) > 0 {
			body["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		}
		if len(in.History) > 0 {
			// Without this, Gemini treats clientContent as a turn to answer.
			body["historyConfig"] = map[string]any{"initialHistoryInClientContent": true}
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
	if e := s.seedHistory(setupCtx, in.History); e != nil {
		s.Close()
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
