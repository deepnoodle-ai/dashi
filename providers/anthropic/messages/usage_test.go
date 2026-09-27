package messages

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestServiceTierMovesFromNativeUsageToResult(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "anthropic", API: API, Model: "model", Scope: "scope"}}
	requested := llm.ModelRef{Provider: "anthropic", Model: "model"}
	complete := `{"id":"msg_1","type":"message","model":"model","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2,"service_tier":"standard"}}`
	stream := `data: {"type":"message_start","message":{"id":"msg_1","type":"message","model":"model","content":[],"usage":{"input_tokens":9,"output_tokens":0,"service_tier":"standard"}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"service_tier":"standard"}}

data: {"type":"message_stop"}

`
	blocking, err := wire.decode([]byte(complete), requested)
	if err != nil {
		t.Fatal(err)
	}
	streaming, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{Requested: requested}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]llm.Result{"blocking": blocking, "streaming": streaming} {
		t.Run(name, func(t *testing.T) {
			if result.ServiceTier != "standard" || result.Usage == nil ||
				result.Usage.InputTokens != 9 || result.Usage.OutputTokens != 2 ||
				result.Stop.Kind != llm.StopFinished || result.Message.Text() != "Hi" {
				t.Fatalf("native usage or final facts lost: %+v", result)
			}
			var raw struct {
				Usage struct {
					ServiceTier string `json:"service_tier"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(result.Raw, &raw); err != nil || raw.Usage.ServiceTier != "standard" {
				t.Fatalf("raw result lost native service tier: %v, %+v", err, raw)
			}
		})
	}
}
