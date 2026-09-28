package dashi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func equalFixture() Step {
	return Step{
		Version:       1,
		TurnID:        "turn",
		Seq:           1,
		At:            time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC),
		Kind:          StepStarted,
		CommandID:     "start",
		CommandDigest: "digest",
		Started: &StartFact{
			OutputSchema: messySchema,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("café <now> & go")),
			},
		},
	}
}

func TestStepEqualAcrossJSONBRoundTrip(t *testing.T) {
	step := equalFixture()
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Step
	if err := json.Unmarshal(jsonbNormalize(raw), &reloaded); err != nil {
		t.Fatal(err)
	}
	if !step.Equal(reloaded) || !reloaded.Equal(step) {
		t.Fatal("a step reloaded from jsonb does not equal the committed step")
	}
}

func TestStepEqualDetectsDifferences(t *testing.T) {
	step := equalFixture()
	retimed := equalFixture()
	retimed.At = retimed.At.Add(time.Nanosecond)
	if step.Equal(retimed) {
		t.Fatal("steps a nanosecond apart are equal")
	}
	reworded := equalFixture()
	reworded.Started.Input = []llm.Message{llm.UserMessage(llm.Text("other"))}
	if step.Equal(reworded) {
		t.Fatal("steps with different input are equal")
	}
}

func TestStepEqualRejectsUnencodableSteps(t *testing.T) {
	broken := equalFixture()
	broken.Started.OutputSchema = json.RawMessage(`{`)
	if _, err := json.Marshal(broken); err == nil {
		t.Fatal("fixture encodes; want an encoding error")
	}
	if broken.Equal(broken) {
		t.Fatal("an unencodable step equals itself")
	}
	if broken.Equal(equalFixture()) || equalFixture().Equal(broken) {
		t.Fatal("an unencodable step equals a valid one")
	}
}
