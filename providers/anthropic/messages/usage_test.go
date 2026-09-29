package messages

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestServiceTierMovesFromNativeUsageToResult(t *testing.T) {
	wire := codec{
		origin: content.Origin{
			Provider: "anthropic",
			API:      API,
			Model:    "model",
			Scope:    "scope",
		},
	}
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
	streaming, err := wire.readStream(
		io.NopCloser(strings.NewReader(stream)),
		llm.Result{
			Requested: requested,
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]llm.Result{
		"blocking":  blocking,
		"streaming": streaming,
	} {
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
			if err := json.Unmarshal(
				result.Raw,
				&raw,
			); err != nil || raw.Usage.ServiceTier != "standard" {
				t.Fatalf("raw result lost native service tier: %v, %+v", err, raw)
			}
		})
	}
}

func TestOneHourCacheWritesDecodeFromNativeUsage(t *testing.T) {
	wire := codec{
		origin: content.Origin{
			Provider: "anthropic",
			API:      API,
			Model:    "model",
			Scope:    "scope",
		},
	}
	requested := llm.ModelRef{Provider: "anthropic", Model: "model"}
	usage := `{"input_tokens":9,"output_tokens":2,"cache_creation_input_tokens":300,"cache_read_input_tokens":40,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}`
	complete := `{"id":"msg_1","type":"message","model":"model","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":` + usage + `}`
	stream := `data: {"type":"message_start","message":{"id":"msg_1","type":"message","model":"model","content":[],"usage":` + usage + `}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

data: {"type":"message_stop"}

`
	blocking, err := wire.decode([]byte(complete), requested)
	if err != nil {
		t.Fatal(err)
	}
	streaming, err := wire.readStream(
		io.NopCloser(strings.NewReader(stream)),
		llm.Result{Requested: requested},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]llm.Result{
		"blocking":  blocking,
		"streaming": streaming,
	} {
		u := result.Usage
		if u == nil || u.CacheCreationInputTokens != 300 ||
			u.CacheCreation1hInputTokens != 200 ||
			u.CacheReadInputTokens != 40 || u.Validate() != nil {
			t.Fatalf("%s: one-hour writes not decoded: %+v", name, u)
		}
	}
}
