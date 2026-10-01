// Package llmclone copies llm values so a caller can change the copy without
// changing the original.
package llmclone

import (
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Messages returns a deep copy of ms.
func Messages(ms []llm.Message) []llm.Message {
	out := make([]llm.Message, len(ms))
	for i, m := range ms {
		out[i] = m.Clone()
	}
	return out
}

// Input returns a deep copy of in, made by a JSON round trip.
func Input(in llm.Input) llm.Input {
	b, _ := json.Marshal(in)
	var out llm.Input
	_ = json.Unmarshal(b, &out)
	return out
}

// ToolResult returns r with its output copied.
func ToolResult(r llm.ToolResult) llm.ToolResult {
	r.Output = r.Output.Clone()
	return r
}
