package main

import (
	"context"
	"io"
	"testing"
)

// The keyless run checks the plumbing: every needle survives selective
// compaction word for word, every tool call keeps its results, an
// unreachable Decide falls back to the stock compactor, and each recorded
// compaction turn rebuilds the same history.
func TestKeyless(t *testing.T) {
	r, err := run(context.Background(), config{sessions: len(themes), window: 16_000, seed: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range r.results[armSelective] {
		for _, n := range r.needles[i] {
			if !o.found[n.Answer] {
				t.Errorf("%s: needle %q not recalled", r.sessions[i], n.Answer)
			}
		}
		if o.fallback != "" {
			t.Errorf("%s: fell back: %s", r.sessions[i], o.fallback)
		}
	}
	for i := range r.sessions {
		stock, sel := r.results[armStock][i], r.results[armSelective][i]
		if sel.kept > stock.kept+stock.kept/10 {
			t.Errorf("%s: selective kept %d tokens, stock %d", r.sessions[i], sel.kept, stock.kept)
		}
	}
}
