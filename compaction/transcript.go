package compaction

import (
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

func renderTranscript(messages []llm.Message) []string { return compactiontext.Transcript(messages) }
func fitTranscript(items []string, budget int) ([]string, error) {
	return compactiontext.FitTranscript(items, budget)
}
