package geminilive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/realtime"
	"golang.org/x/oauth2"
)

type ackConn struct {
	acked  bool
	writes [][]byte
}

func (c *ackConn) Read(context.Context) ([]byte, error) {
	if c.acked {
		return nil, io.EOF
	}
	c.acked = true
	return []byte(`{"setupComplete":{}}`), nil
}
func (c *ackConn) Write(_ context.Context, b []byte) error {
	c.writes = append(c.writes, append([]byte{}, b...))
	return nil
}
func (*ackConn) Close() error { return nil }

type countingTokens struct{ n int }

func (c *countingTokens) Token() (*oauth2.Token, error) {
	c.n++
	return &oauth2.Token{AccessToken: fmt.Sprintf("secret-token-%d", c.n)}, nil
}

func TestVertexDialsRegionalEndpointWithBearerToken(t *testing.T) {
	for location, host := range map[string]string{"us-central1": "us-central1-aiplatform.googleapis.com", "global": "aiplatform.googleapis.com"} {
		tokens := &countingTokens{}
		var url string
		var headers http.Header
		var conn *ackConn
		c, err := New(Config{Vertex: &VertexConfig{Project: "mobius-506918", Location: location, TokenSource: tokens}, Dial: func(_ context.Context, u string, h http.Header, _ *http.Client) (realtime.Connection, error) {
			url, headers, conn = u, h, &ackConn{}
			return conn, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 2; i++ {
			s, err := c.Connect(context.Background(), realtime.Config{})
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			if want := "wss://" + host + "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"; url != want {
				t.Fatalf("url %s", url)
			}
			if got := headers.Get("Authorization"); got != fmt.Sprintf("Bearer secret-token-%d", i) || headers.Get("x-goog-api-key") != "" {
				t.Fatalf("headers %v", headers)
			}
			var m struct{ Setup struct{ Model string } }
			if err := json.Unmarshal(conn.writes[0], &m); err != nil {
				t.Fatal(err)
			}
			if want := "projects/mobius-506918/locations/" + location + "/publishers/google/models/gemini-3.8-live"; m.Setup.Model != want {
				t.Fatalf("model %s", m.Setup.Model)
			}
		}
	}
}
func TestVertexEndpointOverride(t *testing.T) {
	var url string
	c, err := New(Config{Endpoint: "wss://example.test/live", Vertex: &VertexConfig{Project: "p", Location: "us-central1", TokenSource: &countingTokens{}}, Dial: func(_ context.Context, u string, _ http.Header, _ *http.Client) (realtime.Connection, error) {
		url = u
		return &ackConn{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background(), realtime.Config{}); err != nil || url != "wss://example.test/live" {
		t.Fatalf("url %s error %v", url, err)
	}
}
func TestVertexRejectsIncompleteConfig(t *testing.T) {
	tokens := &countingTokens{}
	for name, config := range map[string]Config{
		"no project":      {Vertex: &VertexConfig{Location: "us-central1", TokenSource: tokens}},
		"no location":     {Vertex: &VertexConfig{Project: "p", TokenSource: tokens}},
		"no tokens":       {Vertex: &VertexConfig{Project: "p", Location: "us-central1"}},
		"host injection":  {Vertex: &VertexConfig{Project: "p", Location: "evil.example/x", TokenSource: tokens}},
		"also an API key": {APIKey: "k", Vertex: &VertexConfig{Project: "p", Location: "us-central1", TokenSource: tokens}},
	} {
		if _, err := New(config); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type failingTokens struct{}

func (failingTokens) Token() (*oauth2.Token, error) {
	return nil, errors.New("refresh failed for secret-token-xyz")
}

func TestVertexTokenFailureIsRedacted(t *testing.T) {
	dialed := false
	c, err := New(Config{Vertex: &VertexConfig{Project: "p", Location: "us-central1", TokenSource: failingTokens{}}, Dial: func(context.Context, string, http.Header, *http.Client) (realtime.Connection, error) {
		dialed = true
		return &ackConn{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), realtime.Config{})
	var e *realtime.Error
	if !errors.As(err, &e) || e.Category != realtime.ErrorAuthentication || strings.Contains(err.Error(), "secret") || dialed {
		t.Fatalf("error %v dialed %v", err, dialed)
	}
	if s := fmt.Sprintf("%v %#v", c, c); strings.Contains(s, "secret") || strings.Contains(s, "us-central1") {
		t.Fatalf("client prints %s", s)
	}
}

type refusedTokens struct{}

func (refusedTokens) Token() (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "secret-token-xyz revoked"}
}

func TestVertexTokenFailureKeepsOAuthCode(t *testing.T) {
	c, err := New(Config{Vertex: &VertexConfig{Project: "p", Location: "us-central1", TokenSource: refusedTokens{}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), realtime.Config{})
	var e *realtime.Error
	if !errors.As(err, &e) || e.Category != realtime.ErrorAuthentication || e.Code != "invalid_grant" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v", err)
	}
}
