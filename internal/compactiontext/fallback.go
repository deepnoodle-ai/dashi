package compactiontext

import "fmt"

// FallbackNote begins every summary fallback: the stand-in a compaction
// turn's replacement history uses when the turn ended without a summary.
// Package compaction's IsSummary recognizes it.
const FallbackNote = "Your conversation history was shortened without a summary."

// EmptyFallback is the stand-in replayed for a compaction turn that ended
// without a summary and recorded no fallback of its own.
const EmptyFallback = FallbackNote + " The earlier messages were removed." +
	" Ask the user about earlier work you need."

// Summarized reports whether a compaction turn that stopped with a
// completed stop (completed) and reply replaces history with a summary.
// Any other ended compaction turn uses its summary fallback.
func Summarized(completed bool, reply string) bool {
	return completed && Summary(reply) != ""
}

// FallbackWarning is the message of a summary_fallback warning about
// compaction turn id.
func FallbackWarning(id string) string {
	return fmt.Sprintf(
		"compaction turn %s ended without a summary; the history it "+
			"replaces was shortened without one",
		id,
	)
}
