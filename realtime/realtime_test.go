package realtime_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/realtime"
	"github.com/deepnoodle-ai/dashi/tool"
)

func tools(n int) []tool.Definition {
	out := make([]tool.Definition, n)
	for i := range out {
		out[i] = tool.Definition{Name: fmt.Sprintf("tool_%d", i), Schema: json.RawMessage(`{"type":"object"}`)}
	}
	return out
}

func TestConfigLimits(t *testing.T) {
	for _, c := range []struct {
		name   string
		config realtime.Config
		ok     bool
	}{
		{"default tool count", realtime.Config{Tools: tools(realtime.DefaultMaxTools)}, true},
		{"over default tool count", realtime.Config{Tools: tools(realtime.DefaultMaxTools + 1)}, false},
		{"raised tool count", realtime.Config{Tools: tools(200), MaxTools: 200}, true},
		{"lowered tool count", realtime.Config{Tools: tools(3), MaxTools: 2}, false},
		{"tools over pending bound", realtime.Config{Tools: tools(realtime.MaxPendingTools + 1)}, true},
		{"default instructions", realtime.Config{Instructions: strings.Repeat("x", realtime.DefaultMaxInstructionBytes)}, true},
		{"over default instructions", realtime.Config{Instructions: strings.Repeat("x", realtime.DefaultMaxInstructionBytes+1)}, false},
		{"lowered instructions", realtime.Config{Instructions: "hello", MaxInstructionBytes: 4}, false},
		{"negative limit", realtime.Config{MaxTools: -1}, false},
	} {
		if err := c.config.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: error %v", c.name, err)
		}
	}
}

func TestConfigHistory(t *testing.T) {
	user, assistant := realtime.SpeakerUser, realtime.SpeakerAssistant
	for _, c := range []struct {
		name    string
		history []realtime.HistoryItem
		ok      bool
	}{
		{"both speakers", []realtime.HistoryItem{{user, "hi"}, {assistant, "hello"}}, true},
		{"at the limit", []realtime.HistoryItem{{user, strings.Repeat("x", realtime.MaxHistoryBytes-1)}, {assistant, "y"}}, true},
		{"over the limit in total", []realtime.HistoryItem{{user, strings.Repeat("x", realtime.MaxHistoryBytes)}, {assistant, "y"}}, false},
		{"empty text", []realtime.HistoryItem{{user, ""}}, false},
		{"unknown speaker", []realtime.HistoryItem{{"system", "be nice"}}, false},
	} {
		if err := (realtime.Config{History: c.history}).Validate(); (err == nil) != c.ok {
			t.Errorf("%s: error %v", c.name, err)
		}
	}
}
