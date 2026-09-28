package dashi

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Canonical JSON compares strings by content, so "0.50" and "0.5" differ.
// Dashi digests Go values, and money.Amount always marshals one trimmed
// spelling, so every spelling a store or a caller may supply decodes to a
// value with the same digest.
func TestMoneyDigestIgnoresStoredSpelling(t *testing.T) {
	if canonjson.Equal(json.RawMessage(`"0.50"`), json.RawMessage(`"0.5"`)) {
		t.Fatal("canonical JSON unexpectedly equates decimal strings")
	}
	live := Budget{MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"}}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"max_cost":{"amount":"0.5","currency":"USD"}}` {
		t.Fatalf("live budget encoded %s", raw)
	}
	want := digest(live)
	if got := digest(jsonbRoundTrip(live)); got != want {
		t.Fatal("a jsonb round trip changed the budget digest")
	}
	for _, spelling := range []string{
		`"0.5"`,
		`"0.50"`,
		`"0.500000000"`,
		`"00.5"`,
		`0.5`,
		`0.50`,
		`5e-1`,
		`0.5000000000001`,
	} {
		var stored Budget
		text := `{"max_cost":{"amount":` + spelling + `,"currency":"USD"}}`
		if err := json.Unmarshal([]byte(text), &stored); err != nil {
			t.Fatalf("decode %s: %v", spelling, err)
		}
		if digest(stored) != want {
			t.Fatalf("stored amount %s changed the budget digest", spelling)
		}
	}
}

// TestJSONBRoundTripBudgetTurn runs a capped turn through a jsonb-style
// recorder, then retries its Start from the reloaded turn, once with the live
// budget and once with the budget spelled as bare JSON numbers.
func TestJSONBRoundTripBudgetTurn(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 40}
	rec := jsonbRecorder{NewMemoryRecorder()}
	def := testAgent()
	def.Settings.MaxTokens = 100
	runner := Runner{
		Agent:    def,
		Client:   newPricedClient(answer),
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	start := Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		Budget:    &Budget{MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"}},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		start,
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	// Typed money fields decode to the same values, so the reloaded steps
	// re-encode identically; reload's check for changed bytes does not apply.
	turn, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Cost["USD"] != amount("0.00014") ||
		view.Budget.MaxCost.Amount != amount("0.5") {
		t.Fatalf("reloaded spend=%+v budget=%+v", view.Spend, view.Budget)
	}

	var numeric Budget
	if err := json.Unmarshal(
		[]byte(`{"max_cost":{"amount":0.50,"currency":"USD"}}`),
		&numeric,
	); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []*Budget{start.Budget, &numeric} {
		retry := start
		retry.Budget = budget
		again, err := runner.Run(t.Context(), turn, retry)
		if err != nil || again.Stop.Kind != StopCompleted {
			t.Fatalf("retried start=%+v err=%v", again.Stop, err)
		}
	}
}
