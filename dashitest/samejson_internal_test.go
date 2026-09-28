package dashitest

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi"
)

// TestSameJSONAgreesWithStepEqual keeps the suite's independent comparison
// in step with dashi.Step.Equal, which recorders use for retries. The suite
// does not call Step.Equal itself, so a bug there cannot hide from it.
func TestSameJSONAgreesWithStepEqual(t *testing.T) {
	schemas := []string{
		`{"type":"object","properties":{"z":{"maxLength":100}}}`,
		`{"properties": {"z": {"maxLength": 1e2}}, "type": "object"}`,
		`{"type":"object","properties":{"z":{"maxLength":100.0}}}`,
		`{"type":"object","properties":{"z":{"maxLength":101}}}`,
		`{"type":"object","properties":{"z":{"maxLength":"100"}}}`,
		`{"type":"object","type":"array","properties":{"z":{}}}`,
		`{"type":"array","properties":{"z":{}}}`,
		`{"type":"object","properties":{"z":{"maxLength":100}}}`,
	}
	steps := make([]json.RawMessage, 0, len(schemas)+1)
	for _, schema := range schemas {
		step := startStep(turnID)
		step.Started.OutputSchema = json.RawMessage(schema)
		raw, err := json.Marshal(step)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, raw)
	}
	raw, err := json.Marshal(contextStep(turnID, 2, "text"))
	if err != nil {
		t.Fatal(err)
	}
	steps = append(steps, raw)
	pairs := 0
	for i, a := range steps {
		for j, b := range steps {
			same, err := sameJSON(a, b)
			if err != nil {
				t.Fatal(err)
			}
			var x, y dashi.Step
			if err := json.Unmarshal(a, &x); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &y); err != nil {
				t.Fatal(err)
			}
			if same && i != j {
				pairs++
			}
			if equal := x.Equal(y); equal != same {
				t.Errorf(
					"steps %d and %d: sameJSON = %v, Step.Equal = %v",
					i,
					j,
					same,
					equal,
				)
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no two distinct spellings compared equal; the test is vacuous")
	}
}
