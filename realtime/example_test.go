package realtime_test

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/deepnoodle-ai/dashi/providers/grok/voice"
	"github.com/deepnoodle-ai/dashi/realtime"
)

// The example injects protocol fixtures instead of making a billable call.
type exampleConnection struct{ messages []string }

func (c *exampleConnection) Read(context.Context) ([]byte, error) {
	if len(c.messages) == 0 {
		return nil, io.EOF
	}
	m := c.messages[0]
	c.messages = c.messages[1:]
	return []byte(m), nil
}
func (*exampleConnection) Write(context.Context, []byte) error { return nil }
func (*exampleConnection) Close() error                        { return nil }
func Example() {
	client, err := voice.New(voice.Config{APIKey: "example-fake-key", Dial: func(context.Context, string, http.Header, *http.Client) (realtime.Connection, error) {
		return &exampleConnection{messages: []string{`{"type":"session.updated"}`, `{"type":"response.output_audio_transcript.done","response_id":"r","item_id":"i","transcript":"Hello."}`}}, nil
	}})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, err := client.Connect(ctx, realtime.Config{Instructions: "Speak briefly."})
	if err != nil {
		panic(err)
	}
	defer live.Close()
	fmt.Println(live.Info().OutputFormat.SampleRateHz)
	event, err := live.Receive(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(event.Transcript.Text)
	// Output:
	// 24000
	// Hello.
}
