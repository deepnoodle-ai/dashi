package geminilive_test

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/deepnoodle-ai/dashi/providers/google/geminilive"
	"github.com/deepnoodle-ai/dashi/realtime"
	"golang.org/x/oauth2"
)

// The example injects a setup acknowledgment instead of making a billable
// call. A real program passes google.DefaultTokenSource with the
// cloud-platform scope and omits Dial.
type exampleConnection struct{ acked bool }

func (c *exampleConnection) Read(context.Context) ([]byte, error) {
	if c.acked {
		return nil, io.EOF
	}
	c.acked = true
	return []byte(`{"setupComplete":{}}`), nil
}
func (*exampleConnection) Write(context.Context, []byte) error { return nil }
func (*exampleConnection) Close() error                        { return nil }

// Gemini Live on Vertex AI, seeded with the conversation so far.
func ExampleNew_vertex() {
	client, err := geminilive.New(geminilive.Config{
		Vertex: &geminilive.VertexConfig{Project: "my-project", Location: "us-central1", TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "example-fake-token"})},
		Dial: func(context.Context, string, http.Header, *http.Client) (realtime.Connection, error) {
			return &exampleConnection{}, nil
		},
	})
	if err != nil {
		panic(err)
	}
	live, err := client.Connect(context.Background(), realtime.Config{
		Instructions: "You are a helpful assistant. Speak briefly.",
		History: []realtime.HistoryItem{
			{Speaker: realtime.SpeakerUser, Text: "Remind me to call Ana tomorrow."},
			{Speaker: realtime.SpeakerAssistant, Text: "Done. I'll remind you tomorrow at 9."},
		},
	})
	if err != nil {
		panic(err)
	}
	defer live.Close()
	fmt.Println(live.Info().Provider, live.Info().Model)
	// Output: google gemini-3.8-live
}
