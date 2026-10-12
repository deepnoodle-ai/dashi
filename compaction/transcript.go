package compaction

import (
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

func renderTranscript(messages []llm.Message) []string { return compactiontext.Transcript(messages) }

func shortenTranscript(items []string, budget int) []string {
	return compactiontext.ShortenTranscript(items, budget)
}
