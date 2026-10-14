package turnwatch

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

// trigger runs the free checks on the calls the latest model reply made,
// in this order: failed again, same call, every N model calls. It returns
// the first that triggers, with a short description for the record, or
// "" when none does.
//
// Both call checks match tool name, arguments, and result. A loop that
// makes progress changes its results, as an edit-and-test loop does when
// each test run fails in a new way, so it triggers nothing.
func (w *Watcher) trigger(
	turn dashi.TurnView,
	latest []string,
	heuristic bool,
) (Trigger, string) {
	for _, id := range latest {
		v, ok := turn.Tools[id]
		if !ok || !failed(v) {
			continue
		}
		if n := matching(turn, v); n >= 2 {
			return TriggerFailedAgain,
				fmt.Sprintf("%s: %s, %d times", TriggerFailedAgain, v.Call.Name, n)
		}
	}
	if same := cmp.Or(w.SameCalls, defaultSameCalls); same > 0 {
		for _, id := range latest {
			v, ok := turn.Tools[id]
			if !ok || v.Status != dashi.ToolFinished {
				continue
			}
			if n := matching(turn, v); n-1 >= same {
				return TriggerSameCall,
					fmt.Sprintf("%s: %s, %d times", TriggerSameCall, v.Call.Name, n)
			}
		}
	}
	if !heuristic && w.CheckEvery > 0 && turn.ModelCalls > 0 &&
		turn.ModelCalls%w.CheckEvery == 0 {
		return TriggerCheckEvery,
			fmt.Sprintf("%s: %d model calls", TriggerCheckEvery, turn.ModelCalls)
	}
	return "", ""
}

// matching counts the turn's finished calls, v included, with v's tool,
// arguments by JSON meaning, and result. A failed call matches only failed
// calls, since the result includes whether the call failed.
func matching(turn dashi.TurnView, v dashi.ToolView) int {
	key := callKey(v)
	n := 0
	for _, other := range turn.Tools {
		if other.Status == dashi.ToolFinished && other.Call.Name == v.Call.Name &&
			callKey(other) == key {
			n++
		}
	}
	return n
}

// callKey identifies a finished call by its arguments and the result the
// model saw.
func callKey(v dashi.ToolView) string {
	return argsKey(v.Call.Args) + " " + resultHash(v)
}

// argsKey identifies arguments by JSON meaning, or by their bytes when they
// are not valid JSON.
func argsKey(args json.RawMessage) string {
	if !json.Valid(args) {
		return "raw:" + string(args)
	}
	return dashi.ArgsHash(args)
}

// resultHash identifies the result the model saw: its content and whether
// it is an error. The call ID is left out, since every call has its own,
// and so is Output.Display, which the model never sees.
func resultHash(v dashi.ToolView) string {
	r := v.Result()
	b, err := json.Marshal(struct {
		Content []content.Block `json:"content"`
		IsError bool            `json:"is_error"`
	}{r.Output.Content, r.IsError || failed(v)})
	if err != nil {
		// Unreadable: match no other call.
		return "call:" + v.Call.ID
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// failed reports whether the model saw v as an error: a call that failed,
// or one that never ran, such as a call with invalid arguments or one a
// hook denied. Repeating either unchanged gets the same error again.
func failed(v dashi.ToolView) bool {
	return v.Outcome != nil && (v.Outcome.State == tool.StateFailed ||
		v.Outcome.State == tool.StateNotExecuted)
}
