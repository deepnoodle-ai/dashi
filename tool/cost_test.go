package tool

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/money"
)

func TestOutcomeCostIsExact(t *testing.T) {
	outcome := Outcome{
		State:  StateSucceeded,
		Output: &Output{},
		Cost: &Cost{
			Amount:   money.MustParseAmount("0.0035"),
			Currency: "USD",
		},
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(outcome.Cost)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"amount":"0.0035","currency":"USD"}` {
		t.Fatalf("encoded %s", raw)
	}
	var numeric Cost
	if err := json.Unmarshal(
		[]byte(`{"amount":0.0035,"currency":"USD"}`),
		&numeric,
	); err != nil || numeric != *outcome.Cost {
		t.Fatalf("numeric cost = %+v, %v", numeric, err)
	}
	outcome.Cost.Amount = -1
	if outcome.Validate() == nil {
		t.Fatal("a negative tool charge was accepted")
	}
}
