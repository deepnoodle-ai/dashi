package compactiontext

import (
	"fmt"
	"strings"
	"testing"
)

// ShortenTranscript never fails: it drops middle items, keeping the oldest
// and the newest, and cuts even those when the budget is tiny.
func TestShortenTranscript(t *testing.T) {
	var items []string
	for i := range 100 {
		items = append(items, fmt.Sprintf("item %d %s", i, strings.Repeat("x", 3_000)))
	}
	got := ShortenTranscript(items, 10_000)
	if !strings.HasPrefix(got[0], "item 0 ") || !strings.HasPrefix(got[len(got)-1], "item 99 ") ||
		got[1] == "" || !strings.HasSuffix(got[1], "items omitted]") {
		t.Fatalf("kept %d items, first %.10q, marker %q", len(got), got[0], got[1])
	}
	total := 0
	for _, s := range got {
		total += len(s)
	}
	if total > 10_000+len(got)*40 {
		t.Fatalf("total %d bytes", total)
	}
	tiny := ShortenTranscript(items, 100)
	if len(tiny) != 3 {
		t.Fatalf("tiny budget kept %d items", len(tiny))
	}
	if got := ShortenTranscript(nil, -1); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
	if got := ShortenTranscript([]string{strings.Repeat("y", 5_000)}, 100); len(got) != 1 || len(got[0]) > 200 {
		t.Fatalf("one item: %d bytes", len(got[0]))
	}
}
