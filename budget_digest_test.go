package kernel

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/internal/canonjson"
	"github.com/deepnoodle-ai/kernel/llm"
)

// Canonical JSON compares strings by content, so "0.50" and "0.5" differ.
// Kernel digests Go values, and money.Amount always marshals one trimmed
// spelling, so every spelling a store or an older record may hold decodes to
// a value with the same digest.
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
		if digest(stored) != want || !digestMatches(want, stored) {
			t.Fatalf("stored amount %s changed the budget digest", spelling)
		}
	}
}

// TestJSONBRoundTripBudgetTurn runs a capped turn through a jsonb-style
// recorder, then retries its Start from the reloaded turn, once with the live
// budget and once with the budget as an older record spelled it.
func TestJSONBRoundTripBudgetTurn(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 40}
	rec := jsonbRecorder{NewMemoryRecorder()}
	bindings := Bindings{
		Client:   newPricedClient(answer),
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	def := definition()
	def.Settings.MaxTokens = 100
	start := Start{
		ID:    "start",
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		Budget: &Budget{
			MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"},
		},
	}
	out, err := (Engine{}).Apply(t.Context(), Turn{ID: "t1"}, def, start, bindings)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	// Typed money fields decode to the same values, so the reloaded steps
	// re-encode identically; reload's check for changed bytes does not apply.
	turn, err := rec.Load("t1")
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

	var legacy Budget
	if err := json.Unmarshal(
		[]byte(`{"max_cost":{"amount":0.50,"currency":"USD"}}`),
		&legacy,
	); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []*Budget{start.Budget, &legacy} {
		retry := start
		retry.Budget = budget
		again, err := (Engine{}).Apply(t.Context(), turn, def, retry, bindings)
		if err != nil || again.Stop.Kind != StopCompleted {
			t.Fatalf("retried start=%+v err=%v", again.Stop, err)
		}
	}
}
