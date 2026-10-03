package selective

import (
	"fmt"

	"github.com/deepnoodle-ai/dashi/llm"
)

// checkPairing checks that keep is valid history for a model: every tool
// result follows its call, and every call that input answers is answered in
// keep too. Dashi checks messages one at a time, so without this a pairing
// mistake would break every later request.
func checkPairing(input, keep []llm.Message) error {
	answered := map[string]bool{}
	for _, m := range input {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				answered[c.ToolResult.CallID] = true
			}
		}
	}
	calls := map[string]bool{}
	results := map[string]bool{}
	var order []string
	for _, m := range keep {
		for _, c := range m.Content {
			switch {
			case c.ToolCall != nil:
				if !calls[c.ToolCall.ID] {
					order = append(order, c.ToolCall.ID)
				}
				calls[c.ToolCall.ID] = true
			case c.ToolResult != nil:
				if !calls[c.ToolResult.CallID] {
					return fmt.Errorf("selective: the result of tool call %s would come before its call", c.ToolResult.CallID)
				}
				results[c.ToolResult.CallID] = true
			}
		}
	}
	for _, id := range order {
		if answered[id] && !results[id] {
			return fmt.Errorf("selective: tool call %s would lose its result", id)
		}
	}
	return nil
}
