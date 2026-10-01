package responses

import (
	"encoding/json"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// hostedItemMeters maps hosted output item types to the meter each item
// bills. web_search_call is absent because its meter depends on the declared
// tool and its action. An empty meter marks an item type the provider
// documents as not billed: OpenAI does not charge for listing a remote MCP
// server's tools or for asking the caller to approve a call.
var providerToolItemMeters = map[string]string{
	"file_search_call":      llm.MeterFileSearch,
	"code_interpreter_call": llm.MeterCodeExecution,
	"image_generation_call": llm.MeterImageGeneration,
	"mcp_call":              llm.MeterMCP,
	"mcp_list_tools":        "",
	"mcp_approval_request":  "",
}

// xaiToolCounters maps xAI's usage.server_side_tool_usage_details counters to
// meter keys. xAI bills X search per post and per profile fetched since
// 2026-09-21, so x_search_calls is not a billing unit: its empty meter means
// it counts nothing, but it still covers x_search_call items, which carry no
// post or profile counts of their own. Counters not listed here, such as
// document_search_calls, count under an unrecognized key.
var xaiToolCounters = map[string]string{
	"web_search_calls":       llm.MeterWebSearch,
	"x_posts_fetched":        llm.MeterXSearchPost,
	"x_users_fetched":        llm.MeterXSearchProfile,
	"code_interpreter_calls": llm.MeterCodeExecution,
	"file_search_calls":      llm.MeterFileSearch,
	"image_generation_calls": llm.MeterImageGeneration,
	"mcp_calls":              llm.MeterMCP,
	"x_search_calls":         "",
}

// usdTicksPerNano converts xAI's cost_in_usd_ticks: 10^10 ticks make one US
// dollar, so one tick is a tenth of a nano-dollar.
const usdTicksPerNano = 10

// meterFields holds the response fields that report hosted work. Both the
// blocking and streaming paths decode the same terminal response object, so
// their counts agree.
type meterFields struct {
	// Usage is the raw usage object, read for xAI's counters and cost.
	Usage json.RawMessage `json:"usage"`
	// ToolUsage is OpenAI's top-level tool_usage object. Its
	// web_search.num_requests counts web searches, preview or not.
	ToolUsage json.RawMessage `json:"tool_usage"`
	// Tools echoes the declared tools. It stands in for the request's
	// declarations when the codec was not given them.
	Tools []struct {
		Type string `json:"type"`
	} `json:"tools"`
}

// hostedRequests returns meter counts for the hosted work in a terminal
// response. A provider-reported counter wins for its meter; output items are
// counted only for meters no counter reports.
func (wire codec) providerToolRequests(
	fields meterFields,
	output []json.RawMessage,
) map[string]int {
	requests := map[string]int{}
	reported := map[string]bool{}
	declared := wire.declaredTypes(fields)
	webSearch := webSearchMeter(declared)
	wire.countToolUsage(requests, reported, fields.ToolUsage, webSearch)
	countXAIDetails(requests, reported, fields.Usage)
	// A billed total that includes tool charges accounts for X search items
	// too; it becomes Result.Cost, which wins over any estimate.
	if reportedCost(fields.Usage) != nil {
		reported[xSearchItemMeter] = true
	}
	xSearch := slices.Contains(declared, "x_search")
	for _, raw := range output {
		meter, count := wire.itemRequests(raw, webSearch, xSearch)
		if meter == "" || count <= 0 || reported[meter] {
			continue
		}
		requests[meter] += count
	}
	if len(requests) == 0 {
		return nil
	}
	return requests
}

// declaredTypes returns the request's hosted tool types, or the tools the
// response echoes when the codec was not given them.
func (wire codec) declaredTypes(fields meterFields) []string {
	declared := wire.providerToolTypes
	if len(declared) == 0 {
		for _, t := range fields.Tools {
			declared = append(declared, t.Type)
		}
	}
	return declared
}

// webSearchMeter chooses the key for billed web searches from the declared
// tool types. OpenAI prices web_search_preview separately; when a request
// declares both tools, the response echoes only the preview tool, and its
// price is never lower, so searches count as preview then too.
func webSearchMeter(declared []string) string {
	if slices.Contains(declared, "web_search_preview") {
		return llm.MeterWebSearchPreview
	}
	return llm.MeterWebSearch
}

// countToolUsage reads OpenAI's tool_usage.web_search.num_requests. It
// counts searches only, not page opens or in-page finds, and so confirms the
// item reading in itemRequests.
func (wire codec) countToolUsage(
	requests map[string]int,
	reported map[string]bool,
	raw json.RawMessage,
	webSearch string,
) {
	var value struct {
		WebSearch *struct {
			NumRequests *int `json:"num_requests"`
		} `json:"web_search"`
	}
	if !nonNull(raw) || json.Unmarshal(raw, &value) != nil ||
		value.WebSearch == nil || value.WebSearch.NumRequests == nil {
		return
	}
	reported[webSearch] = true
	if n := *value.WebSearch.NumRequests; n > 0 {
		requests[webSearch] = n
	}
}

// countXAIDetails reads xAI's usage.server_side_tool_usage_details. Every
// counter present, even a zero one, reports its meter, so items of that kind
// are not counted a second time. Values that are not counts are ignored.
func countXAIDetails(
	requests map[string]int,
	reported map[string]bool,
	usage json.RawMessage,
) {
	var value struct {
		Details map[string]json.RawMessage `json:"server_side_tool_usage_details"`
	}
	if !nonNull(usage) || json.Unmarshal(usage, &value) != nil {
		return
	}
	for name, raw := range value.Details {
		var count int
		if json.Unmarshal(raw, &count) != nil {
			continue
		}
		meter, ok := xaiToolCounters[name]
		switch {
		case !ok:
			meter = llm.UnrecognizedMeter(name)
		case meter == "":
			reported[llm.UnrecognizedMeter("x_search_call")] = true
			continue
		}
		reported[meter] = true
		if count > 0 {
			requests[meter] += count
		}
	}
	// X search items bill posts and profiles separately, so they are
	// accounted for only when both counters are present. With one missing,
	// the known counter's count stays and the items still count as unpriced.
	// Captured responses (2026-09-28) list both counters even when zero.
	if reported[llm.MeterXSearchPost] && reported[llm.MeterXSearchProfile] {
		reported[xSearchItemMeter] = true
	}
}

// xSearchItemMeter marks X search work seen only as output items. xAI sends X
// search as custom_tool_call items (x_keyword_search, for example) and bills
// the posts and profiles they fetch, which only its usage counters report.
// Without those counters the items are unpriced, never free.
var xSearchItemMeter = llm.UnrecognizedMeter("custom_tool_call")

// itemRequests returns the meter and count one output item bills. Items the
// codec does not treat as hosted work return an empty meter. A
// custom_tool_call is a client tool call on OpenAI, but when the request
// declared xAI's x_search it may be X search, so it counts as unpriced work.
func (wire codec) itemRequests(
	raw json.RawMessage,
	webSearch string,
	xSearch bool,
) (string, int) {
	var item struct {
		Type   string `json:"type"`
		Action *struct {
			Type    string   `json:"type"`
			Query   string   `json:"query"`
			Queries []string `json:"queries"`
		} `json:"action"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return "", 0
	}
	if item.Type == "custom_tool_call" {
		if xSearch {
			return xSearchItemMeter, 1
		}
		return "", 0
	}
	if item.Type != "web_search_call" {
		meter, ok := providerToolItemMeters[item.Type]
		switch {
		case ok:
			return meter, 1
		case isProviderToolItem(item.Type):
			return llm.UnrecognizedMeter(item.Type), 1
		}
		return "", 0
	}
	// A web_search_call is one search unless its action opens a page or
	// finds text in one. OpenAI documents open_page and find_in_page as
	// actions within a search but does not say whether they are billed; its
	// tool_usage counter excludes them (captured 2026-09-28), so they are not
	// counted. An item without an action is a search.
	if item.Action != nil && item.Action.Type != "" &&
		item.Action.Type != "search" {
		return "", 0
	}
	if wire.origin.Provider != "meta" {
		return webSearch, 1
	}
	// Meta bills web search per query, not per call. The queries an item
	// shows are taken as the queries billed, with at least one per call.
	// Unverified: Meta's pricing page says only "per 1,000 search queries",
	// and no captured item has shown more than one query.
	queries := 1
	if item.Action != nil {
		queries = max(1, len(item.Action.Queries))
	}
	return llm.MeterWebSearch, queries
}

// isHostedItem reports whether decodeItem treats an item type as hosted
// work. It must list every type decodeItem routes to its hosted decoders.
func isProviderToolItem(itemType string) bool {
	switch itemType {
	case "web_search_call", "x_search_call", "file_search_call",
		"code_interpreter_call", "image_generation_call", "mcp_call",
		"mcp_approval_request":
		return true
	}
	return false
}

// reportedCost reads xAI's usage.cost_in_usd_ticks, which xAI documents as the
// amount billed, tool invocations included. Ticks are rounded half to even to
// whole nano-dollars. cost_in_nano_usd is not read: no captured response has
// carried it, so its semantics are unconfirmed.
func reportedCost(usage json.RawMessage) *llm.Cost {
	var value struct {
		Ticks *int64 `json:"cost_in_usd_ticks"`
	}
	if !nonNull(usage) || json.Unmarshal(usage, &value) != nil ||
		value.Ticks == nil || *value.Ticks < 0 {
		return nil
	}
	nanos := ticksToNanos(*value.Ticks)
	return &llm.Cost{
		Total:                money.AmountFromNanos(nanos),
		Currency:             "USD",
		Source:               llm.CostSourceProviderReported,
		BreakdownUnavailable: true,
	}
}

// ticksToNanos divides nonnegative ticks by usdTicksPerNano, rounding half to
// even.
func ticksToNanos(ticks int64) int64 {
	nanos, rest := ticks/usdTicksPerNano, ticks%usdTicksPerNano
	if rest*2 > usdTicksPerNano ||
		(rest*2 == usdTicksPerNano && nanos%2 == 1) {
		nanos++
	}
	return nanos
}
