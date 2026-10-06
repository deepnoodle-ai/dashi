package dashi

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxHookError bounds the hook error text, including a panic's stack, kept
// on the record and in warnings, which are persisted with every turn.
const maxHookError = 4 << 10

// maxHookReason bounds a BeforeFinish reason, which the model sees and the
// record keeps.
const maxHookReason = 1 << 10

// maxRewrites is how many BeforeTool rewrites one call may take before it is
// denied; rewrites must converge.
const maxRewrites = 3

// hookMark and dropHooks bracket one hook chain. When the run's context ends
// during a chain, no decision was made, so the answers already given are
// dropped: the chain runs again on resume and records them then.
type chainMark struct{ hooks, warnings int }

func (r *runtime) hookMark() chainMark {
	return chainMark{hooks: len(r.pendingHooks), warnings: len(r.pendingWarnings)}
}

func (r *runtime) dropHooks(m chainMark) {
	r.pendingHooks = r.pendingHooks[:m.hooks]
	r.pendingWarnings = r.pendingWarnings[:m.warnings]
}

func (r *runtime) addHookResult(result HookResult) {
	r.pendingHooks = append(r.pendingHooks, result)
}

// hookFailed records a failed hook, and a warning when the turn works around
// the failure instead of stopping.
func (r *runtime) hookFailed(
	point HookPoint,
	name string,
	err error,
	warn bool,
) {
	text := boundedHookError(err)
	r.addHookResult(HookResult{
		Point:   point,
		Name:    name,
		Verdict: VerdictFailed,
		Error:   text,
	})
	if warn {
		r.pendingWarnings = append(r.pendingWarnings, Warning{
			Code:    WarningHookFailed,
			Message: fmt.Sprintf("%s hook %s failed: %s", point, name, text),
			Hook:    name,
		})
	}
}

func hookFailedStop() *Stop {
	return &Stop{Kind: StopFailed, Reason: StopReasonHookFailed}
}

// boundedReason makes a reason valid UTF-8, then cuts it at a rune boundary
// so it never exceeds maxHookReason bytes.
func boundedReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	if len(reason) <= maxHookReason {
		return reason
	}
	cut := maxHookReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

func boundedHookError(err error) string {
	text := err.Error()
	if text == "" {
		text = "error"
	}
	if len(text) > maxHookError {
		text = text[:maxHookError]
	}
	return strings.ToValidUTF8(text, "\uFFFD")
}
