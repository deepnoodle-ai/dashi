package llm

import "strings"

// Meter keys name one billable unit of provider tool work. Provider adapters
// count them into Usage.Requests when they decode a response, and a price
// sheet prices them in Pricing.Requests at currency units per unit. A key
// names the unit, not the tool the model called: Anthropic's
// bash_code_execution and Gemini's executableCode both count as
// MeterCodeExecution.
const (
	// MeterWebSearch counts one web search the provider bills: a search call,
	// or one search query where the provider bills per query.
	MeterWebSearch = "web_search"
	// MeterWebSearchPreview counts one call of OpenAI's web_search_preview
	// tool, which some models bill at a different rate than web_search.
	MeterWebSearchPreview = "web_search_preview"
	// MeterGroundedPrompt counts one response grounded by search, for
	// providers that bill per grounded prompt rather than per query.
	MeterGroundedPrompt = "grounded_prompt"
	// MeterWebFetch counts one page fetch.
	MeterWebFetch = "web_fetch"
	// MeterURLContext counts one URL retrieved as context.
	MeterURLContext = "url_context"
	// MeterCodeExecution counts one code run in a provider sandbox.
	MeterCodeExecution = "code_execution"
	// MeterFileSearch counts one search over files stored with the provider.
	MeterFileSearch = "file_search"
	// MeterImageGeneration counts one image generation call.
	MeterImageGeneration = "image_generation"
	// MeterMCP counts one call to a provider-connected remote MCP server.
	MeterMCP = "mcp"
	// MeterXSearchPost counts one X post fetched by xAI's X search.
	MeterXSearchPost = "x_search_post"
	// MeterXSearchProfile counts one X profile fetched by xAI's X search.
	MeterXSearchProfile = "x_search_profile"

	// MeterUnreported counts provider tool uses in a response whose usage never
	// arrived, such as a stream cut off before its final usage frame. No sheet
	// may price it: Dashi cannot tell which meter those uses billed.
	MeterUnreported = "unreported"
)

// unrecognizedMeterPrefix marks a key for provider tool work an adapter did not
// recognize. No sheet may price such a key.
const unrecognizedMeterPrefix = "unrecognized:"

// UnrecognizedMeter returns the key an adapter records for provider tool work
// of a provider wire type it does not recognize, so the work shows as unpriced
// rather than free.
func UnrecognizedMeter(wireType string) string {
	return unrecognizedMeterPrefix + wireType
}

// IsReservedMeter reports whether a key marks usage Dashi could not
// attribute to a billable unit: MeterUnreported or an UnrecognizedMeter key.
// Such keys are never priced.
func IsReservedMeter(key string) bool {
	return key == MeterUnreported ||
		strings.HasPrefix(key, unrecognizedMeterPrefix)
}
