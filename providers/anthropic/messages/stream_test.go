package messages

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Anthropic can stream empty argument fragments for a tool with no arguments.
func TestStreamToolCallWithNoArguments(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		want      string
		cut       bool // the stream ends before the block closes
		bad       bool // the block closes with invalid arguments
	}{
		{name: "empty fragments", fragments: []string{"", ""}, want: `{}`},
		{name: "mixed fragments", fragments: []string{"", `{"n":`, "", `1}`}, want: `{"n":1}`},
		{name: "interrupted", fragments: []string{""}, cut: true},
		{name: "malformed", fragments: []string{"", `{"n":`}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{
				`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[]}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"act","input":{}}}`,
			}
			for _, fragment := range tc.fragments {
				encoded, _ := json.Marshal(fragment)
				events = append(events, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(encoded)+`}}`)
			}
			if !tc.cut {
				events = append(events,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
					`{"type":"message_stop"}`,
				)
			}
			stream := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
			wire := codec{origin: content.Origin{Provider: "anthropic", API: API, Model: "fixture", Scope: "fixture"}}
			result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil)
			if tc.cut {
				if !errors.Is(err, io.ErrUnexpectedEOF) || len(result.Message.Content) != 1 ||
					result.Message.Content[0].Replay == nil || !result.Message.Content[0].Replay.Partial {
					t.Fatalf("interrupted call: err = %v, result = %+v", err, result)
				}
				return
			}
			if tc.bad {
				if err == nil {
					t.Fatalf("malformed arguments were accepted: %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Stop.Kind != llm.StopToolUse || len(result.Message.Content) != 1 {
				t.Fatalf("unexpected result: %+v", result)
			}
			call := result.Message.Content[0].ToolCall
			if call == nil || string(call.Args) != tc.want {
				t.Fatalf("tool call = %+v, want args %s", call, tc.want)
			}
		})
	}
}

func readEvents(t *testing.T, events ...string) (llm.Result, error) {
	t.Helper()
	stream := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
	wire := codec{origin: content.Origin{Provider: "anthropic", API: API, Model: "fixture", Scope: "fixture"}}
	return wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil)
}

// TestStreamPartialProviderToolKeepsItsType covers an interrupted server or
// MCP tool block. Its partial replay data must keep the block's own type.
func TestStreamPartialProviderToolKeepsItsType(t *testing.T) {
	for _, kind := range []string{"tool_use", "server_tool_use", "mcp_tool_use"} {
		t.Run(kind, func(t *testing.T) {
			result, err := readEvents(t,
				`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[]}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"`+kind+`","id":"call","name":"act","server_name":"s","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
			)
			if err == nil || len(result.Message.Content) != 1 {
				t.Fatalf("err = %v, content = %+v", err, result.Message.Content)
			}
			o := result.Message.Content[0].Opaque
			if o == nil || !o.Partial || o.Type != kind {
				t.Fatalf("opaque = %+v, want partial %q", o, kind)
			}
		})
	}
}

// TestStreamRejectsArgumentsForNonToolBlock covers an input_json_delta aimed
// at a text block. The stream broke the block lifecycle, so the call fails
// instead of turning the text into a tool block.
func TestStreamRejectsArgumentsForNonToolBlock(t *testing.T) {
	result, err := readEvents(t,
		`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hi"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
	)
	e, ok := errors.AsType[*llm.Error](err)
	if !ok || e.Category != llm.ErrorProtocol {
		t.Fatalf("err = %v, want a protocol error", err)
	}
	if result.Message.Text() != "Hi" {
		t.Fatalf("content = %+v, want the text block", result.Message.Content)
	}
}

// TestStreamUsageDeltaKeepsStartFields covers a message_delta usage object
// with null fields. A null must not erase what message_start reported.
func TestStreamUsageDeltaKeepsStartFields(t *testing.T) {
	result, err := readEvents(t,
		`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[],"usage":{"input_tokens":5,"output_tokens":1,"service_tier":"priority"}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"service_tier":null}}`,
		`{"type":"message_stop"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceTier != "priority" {
		t.Fatalf("service tier = %q, want priority", result.ServiceTier)
	}
}
