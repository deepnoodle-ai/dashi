package gemini

import (
	"encoding/json"
	"maps"

	"github.com/deepnoodle-ai/dashi/llm"
)

// groundingWire holds the fields of a candidate's groundingMetadata that
// show which provider tools ran. Each groundingChunks entry is a union keyed by
// its source kind (web, retrievedContext, maps, image), so chunks stay maps
// and an unknown kind can still be named.
type groundingWire struct {
	WebSearchQueries   []string                     `json:"webSearchQueries"`
	ImageSearchQueries []string                     `json:"imageSearchQueries"`
	GroundingChunks    []map[string]json.RawMessage `json:"groundingChunks"`
}

// urlContextWire holds the per-URL entries of a candidate's
// urlContextMetadata.
type urlContextWire struct {
	URLMetadata []json.RawMessage `json:"urlMetadata"`
}

// metadataMeters counts the provider tool work that a candidate's grounding and
// URL context metadata record. Gemini reports no provider-tool counters in
// usageMetadata, so these counts come from the metadata itself:
//
//   - Each non-empty webSearchQueries entry is one MeterWebSearch. Gemini 3
//     bills per query and ignores empty queries. Repeated queries are not
//     merged, because the documentation does not say they are.
//   - Web chunks with no reported query count as one search, so per-query
//     billing is never understated to zero.
//   - A response grounded by web search (a web chunk or a search query) is
//     one MeterGroundedPrompt, which Gemini 2.5 and older bill. Both keys are
//     emitted; the price sheet for each model family prices the one it bills.
//   - A response with retrievedContext chunks is one MeterFileSearch. File
//     search bills only tokens, so the key records that it ran.
//   - Each urlMetadata entry is one MeterURLContext.
//
// Image search queries and chunk kinds this adapter does not recognize are
// counted under llm.UnrecognizedMeter so they show as unpriced, not free.
// Metadata that does not decode counts once as unrecognized rather than
// failing a response whose content is intact.
func metadataMeters(grounding, urlContext json.RawMessage) map[string]int {
	counts := map[string]int{}
	if present(grounding) {
		var wire groundingWire
		if err := json.Unmarshal(grounding, &wire); err != nil {
			counts[llm.UnrecognizedMeter("groundingMetadata")] = 1
		} else {
			wire.count(counts)
		}
	}
	if present(urlContext) {
		var wire urlContextWire
		if err := json.Unmarshal(urlContext, &wire); err != nil {
			counts[llm.UnrecognizedMeter("urlContextMetadata")] = 1
		} else if len(wire.URLMetadata) > 0 {
			counts[llm.MeterURLContext] = len(wire.URLMetadata)
		}
	}
	return counts
}

func (wire groundingWire) count(counts map[string]int) {
	queries := 0
	for _, query := range wire.WebSearchQueries {
		if query != "" {
			queries++
		}
	}
	images := 0
	for _, query := range wire.ImageSearchQueries {
		if query != "" {
			images++
		}
	}
	web, files := false, false
	for _, chunk := range wire.GroundingChunks {
		for kind := range chunk {
			switch kind {
			case "web":
				web = true
			case "retrievedContext":
				files = true
			default:
				counts[llm.UnrecognizedMeter("groundingChunk."+kind)] = 1
			}
		}
	}
	// Web chunks without any reported query still came from at least one
	// search, so count one rather than none for per-query billing.
	if web && queries == 0 {
		queries = 1
	}
	if queries > 0 {
		counts[llm.MeterWebSearch] = queries
	}
	if web || queries > 0 {
		counts[llm.MeterGroundedPrompt] = 1
	}
	if files {
		counts[llm.MeterFileSearch] = 1
	}
	if images > 0 {
		counts[llm.UnrecognizedMeter("imageSearchQueries")] = images
	}
}

// streamMeters accumulates meter counts across the chunks of one stream.
// Grounding and URL context metadata may repeat or grow from chunk to chunk,
// so their counts keep the largest value seen and are never summed. Each
// chunk carries new parts, so code runs add up.
type streamMeters struct {
	metadata map[string]int
	codeRuns int
}

// observe records one chunk's first candidate and reports whether the counts
// changed.
func (m *streamMeters) observe(
	grounding json.RawMessage,
	urlContext json.RawMessage,
	codeRuns int,
) bool {
	before := m.counts()
	if m.metadata == nil {
		m.metadata = map[string]int{}
	}
	for key, count := range metadataMeters(grounding, urlContext) {
		m.metadata[key] = max(m.metadata[key], count)
	}
	m.codeRuns += codeRuns
	return !maps.Equal(before, m.counts())
}

// counts returns the cumulative counts, or nil when no provider tool work was
// seen.
func (m *streamMeters) counts() map[string]int {
	counts := maps.Clone(m.metadata)
	if m.codeRuns > 0 {
		if counts == nil {
			counts = map[string]int{}
		}
		counts[llm.MeterCodeExecution] = m.codeRuns
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

// recordMeters adds counts to the usage Gemini reported. Counts ride on that
// usage; a response without usageMetadata gets no invented Usage.
func recordMeters(usage *llm.Usage, counts map[string]int) {
	if usage != nil && len(counts) > 0 {
		usage.Absorb(&llm.Usage{Requests: counts})
	}
}

// usageWithMeters returns a copy of usage carrying counts, for a provisional
// usage delta. Deltas carry cumulative counts; llm.Usage.Absorb keeps the
// largest count per key.
func usageWithMeters(usage *llm.Usage, counts map[string]int) *llm.Usage {
	out := usage.Copy()
	recordMeters(out, counts)
	return out
}

// present reports whether a raw JSON field was sent with a value.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
