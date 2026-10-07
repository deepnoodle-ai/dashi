package messages

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestStreamToolArgumentsIgnoreEmptyFragments(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		want      string
	}{
		{name: "no fragments", want: `{}`},
		{name: "empty fragments", fragments: []string{"", ""}, want: `{}`},
		{
			name:      "empty between nonempty fragments",
			fragments: []string{"", `{"n":`, "", `9007199254740993}`, ""},
			want:      `{"n":9007199254740993}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := streamToolCodec()
			result, err := wire.readStream(
				io.NopCloser(strings.NewReader(toolArgumentStream(tc.fragments, true))),
				llm.Result{},
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			complete, err := wire.decode([]byte(
				`{"id":"m","type":"message","model":"fixture","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call","name":"act","input":`+tc.want+`,"caller":{"type":"direct"}}]}`,
			), llm.ModelRef{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Stop.Kind != llm.StopToolUse {
				t.Fatalf("completed block lost its tool stop: %+v", result.Stop)
			}
			if len(result.Message.Content) != 1 {
				t.Fatalf("stream produced %d contents", len(result.Message.Content))
			}
			got := result.Message.Content[0]
			want := complete.Message.Content[0]
			if !reflect.DeepEqual(got.ToolCall, want.ToolCall) {
				t.Fatalf("stream changed the call: %+v", got.ToolCall)
			}
			if got.Replay == nil || got.Replay.Partial {
				t.Fatal("completed empty arguments became partial replay")
			}
			var gotRaw, wantRaw any
			if err := json.Unmarshal(got.Replay.Raw, &gotRaw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want.Replay.Raw, &wantRaw); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotRaw, wantRaw) {
				t.Fatal("stream replay differs from the complete response")
			}
		})
	}
}

func TestStreamToolArgumentsKeepMalformedAndInterruptedInputPartial(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		complete  bool
	}{
		{name: "malformed", fragments: []string{`{"n":`}, complete: true},
		{name: "whitespace", fragments: []string{" "}, complete: true},
		{name: "interrupted empty", fragments: []string{""}},
		{name: "interrupted nonempty", fragments: []string{`{"n":`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := streamToolCodec()
			result, err := wire.readStream(
				io.NopCloser(strings.NewReader(toolArgumentStream(tc.fragments, tc.complete))),
				llm.Result{},
				nil,
			)
			if err == nil {
				t.Fatal("unfinished input became a successful tool call")
			}
			if tc.complete {
				failure, ok := errors.AsType[*llm.Error](err)
				if !ok || failure.Category != llm.ErrorProtocol {
					t.Fatalf("malformed arguments lost protocol failure: %v", err)
				}
			} else if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("interrupted arguments lost EOF: %v", err)
			}
			if result.Stop.Kind != llm.StopIncomplete {
				t.Fatalf("failed stream lost incomplete stop: %+v", result.Stop)
			}
			if len(result.Message.Content) != 1 {
				t.Fatal("failed stream lost its partial block")
			}
			part := result.Message.Content[0]
			replay := part.Replay
			if part.Opaque != nil {
				replay = part.Opaque
			}
			if replay == nil || !replay.Partial {
				t.Fatal("failed stream allowed complete replay")
			}
		})
	}
}

func TestStreamToolArgumentsDoNotInventInitialInput(t *testing.T) {
	for _, input := range []string{``, `,"input":`} {
		stream := strings.Replace(
			toolArgumentStream([]string{""}, true),
			`,"input":{}`,
			input,
			1,
		)
		_, err := streamToolCodec().readStream(
			io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil,
		)
		if err == nil {
			t.Fatal("invalid initial input became an empty tool call")
		}
	}
}

func streamToolCodec() codec {
	return codec{origin: content.Origin{
		Provider: "anthropic",
		API:      API,
		Model:    "fixture",
		Scope:    "fixture",
	}}
}

func toolArgumentStream(fragments []string, complete bool) string {
	events := []string{
		`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"act","input":{},"caller":{"type":"direct"}}}`,
	}
	for _, fragment := range fragments {
		encoded, _ := json.Marshal(fragment)
		events = append(events,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(encoded)+`}}`,
		)
	}
	if complete {
		events = append(events,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`,
		)
	}
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}
