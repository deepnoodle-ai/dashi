package main

import (
	"context"
	"io"
	"testing"
)

// The keyless run checks the plumbing: every needle survives selective
// compaction word for word, every tool call keeps its results, an
// unreachable Decide falls back to the stock compactor, and each recorded
// compaction turn rebuilds the same history. The variants run too; with a
// long last turn, every arm keeps the file read in it word for word.
func TestKeyless(t *testing.T) {
	for name, v := range map[string]variant{
		"default":   {},
		"vague":     {vague: true},
		"long turn": {longTurn: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config{sessions: len(themes), window: 16_000, seed: 1, variant: v}
			r, err := run(context.Background(), cfg, io.Discard)
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
			// Stock's budget is 0.25 of the window; both keep the system
			// message, about 40 tokens, outside it. Stock often keeps less
			// than its budget, because it cuts only where a user message
			// starts, so selective may keep more than stock did.
			limit := cfg.window/4 + 100
			for i := range r.sessions {
				if sel := r.results[armSelective][i]; sel.kept > limit {
					t.Errorf("%s: selective kept %d tokens, over stock's budget %d", r.sessions[i], sel.kept, limit)
				}
			}
		})
	}
}
