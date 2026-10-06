package turnwatch

import (
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi"
)

// Reminder is a reminder the watcher recorded, read back from the turn by
// ReadReminder.
type Reminder struct {
	// Question is the confirmed question: QuestionRepeating or
	// QuestionOffRequest.
	Question string
	// Probability is the question's P(yes), to the hundredth, or zero for
	// a reminder sent without asking Decide.
	Probability float64
	// Trigger is the free check that led to the check.
	Trigger Trigger
}

// ReadReminder reads the reminder the watcher recorded in h, a hook result
// of the step before a model call. It returns false for another hook's
// result, or for a check that sent no reminder.
func ReadReminder(h dashi.HookResult) (Reminder, bool) {
	if h.Name != Name || h.Verdict != dashi.VerdictChange {
		return Reminder{}, false
	}
	// The Reason is "<question>: <trigger>: <detail>", then, when Decide
	// was asked, "; repeating 0.88, off_request 0.10 (request r_1)".
	question, rest, ok := strings.Cut(h.Reason, ": ")
	if !ok || question != QuestionRepeating && question != QuestionOffRequest {
		return Reminder{}, false
	}
	trigger, _, _ := strings.Cut(rest, ": ")
	r := Reminder{Question: question, Trigger: Trigger(trigger)}
	if i := strings.LastIndex(rest, "; "); i >= 0 {
		answers, _, _ := strings.Cut(rest[i+2:], " (")
		for answer := range strings.SplitSeq(answers, ", ") {
			number, found := strings.CutPrefix(answer, question+" ")
			if p, err := strconv.ParseFloat(number, 64); found && err == nil {
				r.Probability = p
			}
		}
	}
	return r, true
}
