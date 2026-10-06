package toolcheck

import (
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi"
)

// ResultRecord is what the PromptInjection hook recorded about one tool
// result, read back from the turn by ReadResultRecord.
type ResultRecord struct {
	// Checked is true when Decide answered.
	Checked bool
	Flagged bool
	// Partial is true when only the start and end of the result were
	// checked.
	Partial bool
	// Warned is true when the model read a note before the result: a
	// warning, or a notice that all or part of it was not checked.
	Warned bool
	// Probabilities holds P(yes) by question, such as QuestionInjection,
	// to the whole percent.
	Probabilities map[string]float64
	Model         string
	RequestID     string
	// Cause is why Decide did not answer, when Checked is false.
	Cause Cause
}

// ReadResultRecord reads what the PromptInjection hook recorded in h, a
// hook result of a tool_finished step. It returns false for another
// hook's result or a record it cannot read.
func ReadResultRecord(h dashi.HookResult) (ResultRecord, bool) {
	if h.Name != PromptInjectionName {
		return ResultRecord{}, false
	}
	r := ResultRecord{Warned: h.Verdict == dashi.VerdictReplace}
	switch h.Verdict {
	case dashi.VerdictReplace, dashi.VerdictNoChange:
	case dashi.VerdictFailed:
		// A panic, or a record from before the hook recorded failures.
		r.Cause = CauseError
		return r, true
	default:
		return ResultRecord{}, false
	}
	if c, ok := strings.CutPrefix(h.Reason, uncheckedPrefix); ok {
		r.Cause = Cause(c)
		return r, true
	}
	text, flagged := strings.CutPrefix(h.Reason, flaggedPrefix)
	answers, rest, ok := strings.Cut(text, " (")
	if !ok {
		return ResultRecord{}, false
	}
	r.Checked, r.Flagged = true, flagged
	r.Model, r.RequestID, _ = strings.Cut(strings.TrimSuffix(rest, ")"), ", request ")
	r.Probabilities = map[string]float64{}
	for answer := range strings.SplitSeq(answers, ", ") {
		if answer == partialWords {
			r.Partial = true
			continue
		}
		key, number, ok := strings.Cut(answer, " ")
		p, err := strconv.Atoi(strings.TrimSuffix(number, "%"))
		if !ok || err != nil {
			return ResultRecord{}, false
		}
		r.Probabilities[key] = float64(p) / 100
	}
	return r, true
}
