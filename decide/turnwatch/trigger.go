package turnwatch

import (
	"cmp"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// trigger runs the free checks on the calls the latest model reply made,
// in this order: failed again, same call, every N model calls. It returns
// the first that triggers, with a short description for the record, or
// "" when none does.
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
		if n := matching(turn, v.Call, true); n >= 2 {
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
			if n := matching(turn, v.Call, false); n-1 >= same {
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

// matching counts the turn's calls, call included, with call's tool and
// arguments by JSON meaning; with onlyFailed, only failed ones.
func matching(turn dashi.TurnView, call tool.Call, onlyFailed bool) int {
	key := argsKey(call.Args)
	n := 0
	for _, v := range turn.Tools {
		if v.Call.Name != call.Name || argsKey(v.Call.Args) != key {
			continue
		}
		if onlyFailed && !failed(v) {
			continue
		}
		n++
	}
	return n
}

// argsKey identifies arguments by JSON meaning, or by their bytes when they
// are not valid JSON.
func argsKey(args json.RawMessage) string {
	if !json.Valid(args) {
		return "raw:" + string(args)
	}
	return dashi.ArgsHash(args)
}

func failed(v dashi.ToolView) bool {
	return v.Outcome != nil && v.Outcome.State == tool.StateFailed
}
