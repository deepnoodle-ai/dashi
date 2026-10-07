package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
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

func TestSemanticFailureContainsOnlyCanonicalMetadata(t *testing.T) {
	secret := "PRIVATE-session-path-message-or-error"
	out := dashi.RunResult{
		Turn: dashi.Turn{ID: secret, Steps: []dashi.Step{{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{
			ModelCallID: secret, Error: secret, Result: &llm.Result{Message: llm.AssistantMessage(llm.Text(secret)), Raw: []byte(secret), Stop: llm.Stop{Kind: llm.StopOutputLimit, Raw: secret}},
		}}}},
		TurnView: dashi.TurnView{Stop: dashi.Stop{Kind: dashi.StopLimited, Reason: secret, Limit: &dashi.LimitDetail{Kind: dashi.LimitOutputTokens, Code: secret}}},
	}
	failure := stoppedTurn("summary", out)
	if failure.stage != "summary" || failure.stop != dashi.StopLimited || failure.limit != dashi.LimitOutputTokens || failure.modelStop != llm.StopOutputLimit {
		t.Fatal("canonical failure metadata lost")
	}
	var printed bytes.Buffer
	(&report{problems: []string{"case-01: " + failure.Error()}, incomplete: true}).print(&printed, config{})
	if strings.Contains(printed.String(), secret) || !strings.Contains(printed.String(), `model stop "output_limit"`) {
		t.Fatal("semantic failure report lost metadata or exposed private text")
	}
	out.Stop.Kind = dashi.StopKind(secret)
	out.Stop.Limit.Kind = dashi.LimitKind(secret)
	out.Turn.Steps[0].Model.Result.Stop.Kind = llm.StopKind(secret)
	if strings.Contains(stoppedTurn(secret, out).Error(), secret) {
		t.Fatal("unrecognized enum or stage was published")
	}
}

func TestSummaryAndProbeSemanticStopsRetainDiagnostics(t *testing.T) {
	for _, kind := range []llm.StopKind{llm.StopOutputLimit, llm.StopRefusal} {
		result := dashitest.Reply("PRIVATE-model-message")
		result.Stop = llm.Stop{Kind: kind, Raw: "PRIVATE-stop-reason"}
		e := &env{model: dashitest.Model, client: func(_, _ string) llm.Client { return dashitest.NewClient(result) }}
		s := newSession(0, 1, variant{})
		c := compaction.Compactor{Model: dashitest.Model, Policy: compaction.Policy{Model: llm.ModelInfo{Capabilities: llm.Capabilities{ContextWindow: 16_000}}}}
		start, err := c.Start(t.Context(), s.messages)
		if err != nil {
			t.Fatal(err)
		}
		_, _, summaryErr := e.compact(t.Context(), s, "PRIVATE-turn-id", start, s.messages)
		_, _, probeErr := e.probe(t.Context(), s, "PRIVATE-turn-id", nil, s.needles)
		for stage, err := range map[string]error{"summary": summaryErr, "probe": probeErr} {
			failure, ok := errors.AsType[*semanticFailure](err)
			if !ok || failure.stage != stage || failure.modelStop != kind || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("%s did not retain safe stop metadata", stage)
			}
		}
	}
}
