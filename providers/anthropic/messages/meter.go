package messages

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// serverToolCounters maps usage.server_tool_use counters to meter keys.
// Anthropic counts only uses it bills, so a search refused with
// max_uses_exceeded is not counted.
var serverToolCounters = map[string]string{
	"web_search_requests":     llm.MeterWebSearch,
	"web_fetch_requests":      llm.MeterWebFetch,
	"code_execution_requests": llm.MeterCodeExecution,
}

// serverToolNames maps server_tool_use block names to the meter each use
// bills. Blocks are counted only for a meter usage reports no counter for. An
// empty meter marks a tool Anthropic documents as not billed separately.
var serverToolNames = map[string]string{
	"web_search":                 llm.MeterWebSearch,
	"web_fetch":                  llm.MeterWebFetch,
	"code_execution":             llm.MeterCodeExecution,
	"bash_code_execution":        llm.MeterCodeExecution,
	"text_editor_code_execution": llm.MeterCodeExecution,
	"tool_search_tool_regex":     "",
	"tool_search_tool_bm25":      "",
}

// serverToolRequests converts a usage.server_tool_use object into meter
// counts. It also returns the meters the object has a counter for, even a
// zero one, because a reported counter wins over counting blocks.
func serverToolRequests(
	raw json.RawMessage,
) (map[string]int, map[string]bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	requests := map[string]int{}
	reported := map[string]bool{}
	for name, value := range fields {
		if !strings.HasSuffix(name, "_requests") ||
			string(value) == "null" {
			continue
		}
		var count int
		if err := json.Unmarshal(value, &count); err != nil {
			return nil, nil, fmt.Errorf("server_tool_use.%s: %w", name, err)
		}
		meter, ok := serverToolCounters[name]
		if !ok {
			meter = llm.UnrecognizedMeter(name)
		}
		reported[meter] = true
		if count > 0 {
			requests[meter] += count
		}
	}
	return requests, reported, nil
}

// countHostedBlocks adds meter counts for hosted blocks whose meter usage
// has no counter for. It skips server tool uses whose result is an error,
// matching usage counters, which exclude uses Anthropic does not bill. MCP
// calls are counted whatever their outcome because no documentation says
// failed calls are free.
func countHostedBlocks(
	requests map[string]int,
	reported map[string]bool,
	blocks []json.RawMessage,
) {
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	decoded := make([]block, 0, len(blocks))
	failed := map[string]bool{}
	for _, raw := range blocks {
		var b block
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		decoded = append(decoded, b)
		if b.ToolUseID != "" && strings.HasSuffix(b.Type, "_tool_result") &&
			hostedResultError(b.Content) {
			failed[b.ToolUseID] = true
		}
	}
	for _, b := range decoded {
		var meter string
		switch b.Type {
		case "mcp_tool_use":
			meter = llm.MeterMCP
		case "server_tool_use":
			known, ok := serverToolNames[b.Name]
			switch {
			case !ok:
				meter = llm.UnrecognizedMeter(b.Name)
			case known == "" || failed[b.ID]:
				continue
			default:
				meter = known
			}
		default:
			continue
		}
		if !reported[meter] {
			requests[meter]++
		}
	}
}
