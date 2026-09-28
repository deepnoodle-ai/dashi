package tool

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/money"
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
	var legacy Cost
	if err := json.Unmarshal(
		[]byte(`{"amount":0.0035,"currency":"USD"}`),
		&legacy,
	); err != nil || legacy != *outcome.Cost {
		t.Fatalf("legacy cost = %+v, %v", legacy, err)
	}
	outcome.Cost.Amount = -1
	if outcome.Validate() == nil {
		t.Fatal("a negative tool charge was accepted")
	}
}
