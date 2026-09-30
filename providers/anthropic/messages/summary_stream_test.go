package messages

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestSummaryStreamDeltasRequirePublicBlock(t *testing.T) {
	for _, block := range []string{
		`{"type":"future_thinking","data":"PRIVATE_ARTIFACT"}`,
		`{"type":"redacted_thinking","data":"PRIVATE_ARTIFACT"}`,
		`{"type":"thinking","thinking":"","signature":"signature"}`,
	} {
		for _, complete := range []bool{false, true} {
			stream := "data: " + `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}` + "\n\n" + "data: " + `{"type":"content_block_start","index":0,"content_block":` + block + "}\n\n" + "data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"PREVIEW"}}` + "\n\n" + "data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"PRIVATE_ANSWER_LABEL"}}` + "\n\n"
			if complete {
				stream += "data: " + `{"type":"content_block_stop","index":0}` + "\n\n" + "data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" + "data: " + `{"type":"message_stop"}` + "\n\n"
			}
			client, err := New(Config{Provider: "fixture", Endpoint: "https://fixture.test/messages"})
			if err != nil {
				t.Fatal(err)
			}
			wire := codec{origin: client.client.Origin("fixture")}
			var previews []llm.Delta
			result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) {
				if d.Text != "" {
					previews = append(previews, d)
				}
			})
			if complete && err != nil || !complete && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("complete=%v error=%v", complete, err)
			}
			public := strings.Contains(block, `"type":"thinking"`)
			if public && (len(previews) != 1 || previews[0].Kind != llm.DeltaReasoning || previews[0].Text != "PREVIEW") || !public && len(previews) != 0 {
				t.Fatalf("block=%s preview=%+v", block, previews)
			}
			if result.Message.Text() != "" || len(result.Message.Content) != 1 {
				t.Fatalf("private answer label leaked: %+v", result.Message)
			}
			part := result.Message.Content[0]
			replay := part.Replay
			if replay == nil {
				replay = part.Opaque
			}
			if replay == nil || replay.Partial == complete || !strings.Contains(string(replay.Raw), "PREVIEW") {
				t.Fatalf("lost partial replay: %+v", replay)
			}
			if part.Reasoning != nil && !public && part.Reasoning.Text != "" {
				t.Fatal("redacted thinking became public")
			}
			input := llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("explain")), result.Message, llm.UserMessage(llm.Text("continue"))}}
			_, err = client.Prepare(t.Context(), input)
			if (err == nil) != complete {
				t.Fatalf("complete=%v Prepare error=%v", complete, err)
			}
		}
	}
}
