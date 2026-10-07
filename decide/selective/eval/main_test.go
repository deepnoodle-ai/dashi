package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide"
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

func TestCannedNeedlesAreCaseSpecificAndDecoded(t *testing.T) {
	private := `PRIVATE<detail>"with\escapes`
	first := []needle{{Answer: private}}
	judge := cannedNeedles(first)
	first[0].Answer = "changed later"
	req := decide.NewRequest("state")
	req.Questions["s0.needed"] = decide.Noul(map[string]string{"segment": private})
	req.Questions["s0.verbatim"] = decide.Noul(map[string]string{"segment": private, "short_form": "omitted"})
	resp, err := judge(req)
	if err != nil {
		t.Fatal(err)
	}
	for key, answer := range resp.Answers {
		if answer.(*decide.NoulAnswer).Noul < 0.5 {
			t.Fatalf("%s did not recognize decoded needle", key)
		}
	}
	other, err := cannedNeedles([]needle{{Answer: "other case"}})(req)
	if err != nil {
		t.Fatal(err)
	}
	if other.Answers["s0.needed"].(*decide.NoulAnswer).Noul >= 0.5 {
		t.Fatal("judge used another case's needles")
	}
}

func TestPrivateNeedleFailureIsSafeToPrint(t *testing.T) {
	secret := `PRIVATE<detail>"with\escapes`
	needles := []needle{{Kind: "value", Answer: secret}}
	err := survives([]llm.Message{llm.UserMessage(llm.Text("detail omitted"))}, needles)
	if err == nil {
		t.Fatal("lost needle was not detected")
	}
	r := &report{sessions: []string{"case-01"}, sizes: []int{100}, needles: [][]needle{needles}, results: map[string][]outcome{}, problems: []string{"case-01: " + err.Error()}}
	for _, arm := range arms {
		r.results[arm] = []outcome{{completed: true}}
	}
	var out bytes.Buffer
	r.print(&out, config{})
	encoded, _ := json.Marshal(secret)
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), string(encoded)) || !strings.Contains(out.String(), "needle 1 did not survive") {
		t.Fatalf("unsafe failure report: %s", out.String())
	}
}
