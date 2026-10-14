package compaction

import (
	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// LastUsage returns the usage of the last model call in t that reported any.
// Pass it to Policy.Due and to Options.Last. It is not TurnView.Spend.Usage.
// The spend sums every call of a Turn, and each call in a tool loop sends the
// whole history again, so the sum is larger than the context. The usage of
// the last call is the size of the context at the end of the Turn. It is zero
// when no call reported usage.
func LastUsage(t dashi.Turn) llm.Usage {
	for i := len(t.Steps) - 1; i >= 0; i-- {
		m := t.Steps[i].Model
		if m == nil || m.Result == nil || m.Result.Usage == nil {
			continue
		}
		return *m.Result.Usage.Clone()
	}
	return llm.Usage{}
}
