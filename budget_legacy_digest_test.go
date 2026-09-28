package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

// preMoney types encode commands exactly as Kernel did while Money.Amount was
// a float64. They are written out here, independently of digest.go, so the
// test checks the compatibility path against the old shape itself.
type preMoneyMoney struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type preMoneyBudget struct {
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int            `json:"max_total_tokens,omitempty"`
	MaxCost         *preMoneyMoney `json:"max_cost,omitempty"`
}

type preMoneyStart struct {
	ID           string
	Origin       StartOrigin
	Budget       *preMoneyBudget
	OutputSchema json.RawMessage
	History      []llm.Message
	Input        []llm.Message
}

type preMoneyContinue struct {
	ID      string
	History []llm.Message
	Budget  *preMoneyBudget
}

// preMoneyDigests returns the bare-hex digest written before canonical
// hashing and the v2 digest written after it, both over the old encoding.
func preMoneyDigests(t *testing.T, command any) []string {
	t.Helper()
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return []string{hex.EncodeToString(h[:]), digest(command)}
}

// seededLoader is a recorder whose saved turn carries rewritten digests.
type seededLoader struct {
	*MemoryRecorder
	turn Turn
}

func (r seededLoader) Load(context.Context, string) (Turn, error) {
	return r.turn, nil
}

func withCommandDigest(turn Turn, commandID, stored string) Turn {
	turn.Steps = append([]Step(nil), turn.Steps...)
	for i := range turn.Steps {
		if turn.Steps[i].CommandID == commandID {
			turn.Steps[i].CommandDigest = stored
		}
	}
	return turn
}

func TestFloatMoneyShapesMatchCommands(t *testing.T) {
	names := func(v any) []string {
		var out []string
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			out = append(out, typ.Field(i).Name)
		}
		return out
	}
	for _, pair := range [][2]any{
		{Start{}, floatMoneyStart{}},
		{Continue{}, floatMoneyContinue{}},
		{Start{}, preMoneyStart{}},
		{Continue{}, preMoneyContinue{}},
		{Budget{}, floatMoneyBudget{}},
	} {
		if !reflect.DeepEqual(names(pair[0]), names(pair[1])) {
			t.Fatalf("%T fields %v; legacy shape %v",
				pair[0], names(pair[0]), names(pair[1]))
		}
	}
}

func TestPreMoneyCommandDigestsStillAcknowledgeRetries(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	second := final("done")
	second.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	client := newPricedClient(first, second)
	def := definition()
	def.Settings.MaxTokens = 500
	b := Bindings{
		Client:   client,
		Tools:    fixedTools{budgetLookup(nil)},
		Recorder: NewMemoryRecorder(),
		Policy:   AllowLocalPolicy{},
	}
	input := []llm.Message{llm.UserMessage(llm.Text("lookup"))}
	start := Start{
		ID:     "start",
		Budget: &Budget{MaxCost: &Money{Amount: amount("0.0016"), Currency: "USD"}},
		Input:  input,
	}
	out, err := (Engine{}).Apply(t.Context(), Turn{ID: "t1"}, def, start, b)
	if err != nil {
		t.Fatal(err)
	}
	requireBudgetLimit(t, out, LimitCost)
	raise := Continue{
		ID:     "raise",
		Budget: &Budget{MaxCost: &Money{Amount: amount("0.003"), Currency: "USD"}},
	}
	out, err = (Engine{}).Apply(t.Context(), out.Turn, def, raise, b)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("raise=%+v err=%v", out.Stop, err)
	}
	recorded := out.Turn

	oldStart := preMoneyStart{
		ID: "start",
		Budget: &preMoneyBudget{
			MaxCost: &preMoneyMoney{Amount: 0.0016, Currency: "USD"},
		},
		Input: input,
	}
	oldRaise := preMoneyContinue{
		ID: "raise",
		Budget: &preMoneyBudget{
			MaxCost: &preMoneyMoney{Amount: 0.003, Currency: "USD"},
		},
	}
	changedStart := start
	changedStart.Budget = &Budget{
		MaxCost: &Money{Amount: amount("0.0016") + 1, Currency: "USD"},
	}
	changedRaise := raise
	changedRaise.Budget = &Budget{
		MaxCost: &Money{Amount: amount("0.004"), Currency: "USD"},
	}
	for _, tc := range []struct {
		name    string
		id      string
		old     any
		retry   Command
		changed Command
	}{
		{"start", "start", oldStart, start, changedStart},
		{"raise", "raise", oldRaise, raise, changedRaise},
	} {
		for _, stored := range preMoneyDigests(t, tc.old) {
			seeded := withCommandDigest(recorded, tc.id, stored)
			rec := seededLoader{NewMemoryRecorder(), seeded}
			bindings := b
			bindings.Recorder = rec
			// The caller's snapshot holds the command, and a stale snapshot
			// finds it only in the recorder.
			for _, snapshot := range []Turn{seeded, {ID: "t1"}} {
				again, err := (Engine{}).Apply(
					t.Context(), snapshot, def, tc.retry, bindings,
				)
				if err != nil || again.Stop.Kind != StopCompleted {
					t.Fatalf("%s retry with %s: stop=%+v err=%v",
						tc.name, stored[:12], again.Stop, err)
				}
				_, err = (Engine{}).Apply(
					t.Context(), snapshot, def, tc.changed, bindings,
				)
				if !errors.Is(err, ErrCommandConflict) {
					t.Fatalf("%s changed amount with %s: err=%v",
						tc.name, stored[:12], err)
				}
			}
		}
	}
	if client.sends != 2 {
		t.Fatalf("retries sent model calls: %d", client.sends)
	}
}
