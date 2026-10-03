// Package turnwatch watches a running turn for an agent that repeats a
// failing approach or wanders from the user's request, and reminds it to
// check its approach.
//
// A Watcher supplies a BeforeModel hook. Before each model call it runs
// free checks on the tool calls that finished since the previous call. Only
// when one triggers does it ask a decision model, through Decide, whether
// the agent is repeating a failed approach or has moved away from the
// request. A confirmed answer adds a short runtime reminder, named
// "turnwatch", before the next model call. The watcher never stops a turn.
//
// Everything the hook counts, it reads from the turn record, so a turn
// resumed in another process behaves the same. A failed Decide request adds
// no reminder: the hook returns the error, and Dashi records a hook_failed
// warning and goes on.
package turnwatch

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide"
)

// Name is the watcher's hook name, the name of its reminder slot, and the
// name of its reminders.
const Name = "turnwatch"

// Defaults for a Watcher's zero fields.
const (
	defaultSameCalls    = 3
	defaultMaxReminders = 2
	defaultTimeout      = 3 * time.Second
)

// Watcher checks a running turn for repeated failing calls and drift from
// the user's request. The zero value works: it reads a Decide client from
// the environment and uses the defaults below. One Watcher serves many
// turns at once; it keeps no state between calls.
type Watcher struct {
	// Client asks the decision model. Nil means decide.NewClient() from the
	// environment, such as TYPESAFE_API_KEY; if that fails, every check
	// fails open with the error.
	Client *decide.Client
	// MinProbability is the P(yes) a question's answer must reach to be
	// confirmed. Zero means the model leans yes: P(yes) above 0.5. A
	// negative value never asks Decide and reminds on every free trigger.
	MinProbability float64
	// SameCalls is how many earlier calls with the same tool, arguments,
	// and result trigger a check. Zero means 3; negative turns the check
	// off.
	SameCalls int
	// CheckEvery triggers a check every this many model calls, to catch
	// drift with no repeated call. Zero turns it off. It is ignored when
	// MinProbability is negative, since only Decide can judge drift.
	CheckEvery int
	// MaxReminders caps the reminders per turn; at the cap the watcher stops
	// checking that turn. Zero means 2. A negative value sends none but
	// still checks, so a host can watch OnCheck first.
	MaxReminders int
	// Timeout bounds each Decide request. Zero means 3 seconds.
	Timeout time.Duration
	// OnCheck, if set, sees every check. It runs in the hook, before the
	// model call, so keep it fast.
	OnCheck func(Check)
}

// Trigger names the free check that led to a check.
type Trigger string

const (
	// TriggerFailedAgain is a call that failed with the same tool,
	// arguments, and error as an earlier call in the turn.
	TriggerFailedAgain Trigger = "failed_again"
	// TriggerSameCall is a call with the same tool, arguments, and result
	// as SameCalls earlier calls.
	TriggerSameCall Trigger = "same_call"
	// TriggerCheckEvery is a model call count that reached a multiple of
	// CheckEvery.
	TriggerCheckEvery Trigger = "check_every"
)

// Action is what a check did.
type Action string

const (
	// ActionNone added nothing: no key was confirmed, the check failed, or
	// MaxReminders is negative.
	ActionNone Action = "none"
	// ActionRemind added a reminder before the next model call.
	ActionRemind Action = "remind"
)

// Check describes one check, for OnCheck. A check without Decide, in
// heuristic-only mode, has no Model, RequestID, Answers, or Usage.
type Check struct {
	TurnID string
	// ModelCalls is the turn's finished model calls at the check.
	ModelCalls int
	Trigger    Trigger
	// Model is the resolved decision model, such as "jev-1.13.0".
	Model     string
	RequestID string
	// Answers holds the decision model's answers by question key:
	// "repeating" and "off_request".
	Answers map[string]decide.Answer
	Usage   decide.Usage
	Action  Action
	// Err is why the check failed; the hook returns it too.
	Err error
}

// Question keys, which name the confirmed key in the record's Reason.
const (
	keyRepeating  = "repeating"
	keyOffRequest = "off_request"
)

// reminderText returns the fixed text for a confirmed key and the trigger
// that led to the check, so the reminder says only what the trigger saw: a
// same_call trigger is a call that did not fail, so its text does not say
// the call fails. Only this text reaches the agent; tool text can move the
// probabilities but never the words.
func reminderText(key string, trigger Trigger) string {
	if key == keyOffRequest {
		return "Your recent calls may have moved away from the user's " +
			"request. Re-read the request and check that your next call works " +
			"toward it."
	}
	switch trigger {
	case TriggerFailedAgain:
		return "A call you made keeps failing in the same way. Do not " +
			"repeat it unchanged. Read the error, work out why it fails, and " +
			"try a different approach."
	case TriggerSameCall:
		return "You made the same call several times and got the same " +
			"result each time. Do not repeat it unchanged. Use the result you " +
			"already have, or try a different approach."
	default:
		return "Your recent calls may repeat an approach that already " +
			"failed. Work out why it failed, and try a different approach."
	}
}

// Hook returns the watcher's BeforeModel hook, named "turnwatch". Add it
// to Runner.Hooks, so the record's ActiveHooks names it.
func (w *Watcher) Hook() dashi.ModelHook {
	return dashi.ModelHook{Name: Name, Func: w.check}
}

func (w *Watcher) check(
	ctx context.Context,
	p dashi.ModelPlan,
) (dashi.ModelDecision, error) {
	turn := p.Turn
	// Skip the summary call of active compaction (Purpose is set) and a
	// session compaction turn, which summarizes earlier turns rather than
	// working on a request. A child turn shares its parent's hooks through
	// delegate, and one loop should not get a reminder at every level.
	if p.Purpose != "" || turn.Origin == dashi.StartCompaction || turn.Parent != nil {
		return dashi.ModelDecision{}, nil
	}
	sent := remindersSent(turn)
	limit := cmp.Or(w.MaxReminders, defaultMaxReminders)
	if limit > 0 && sent >= limit {
		return dashi.ModelDecision{}, nil
	}
	latest, reminded := sinceLastReply(p.Input.Messages)
	if reminded {
		// This model call was already checked: Dashi asks the hooks again
		// when an urgent steer sends the run back before the request.
		return dashi.ModelDecision{}, nil
	}
	heuristic := w.MinProbability < 0
	trigger, detail := w.trigger(turn, latest, heuristic)
	if trigger == "" {
		return dashi.ModelDecision{}, nil
	}
	c := Check{
		TurnID:     turn.ID,
		ModelCalls: turn.ModelCalls,
		Trigger:    trigger,
		Action:     ActionNone,
	}
	key, reason := keyRepeating, detail
	if !heuristic {
		resp, err := w.ask(ctx, p)
		if resp != nil {
			c.Model, c.RequestID = resp.Model, resp.RequestID
			c.Answers, c.Usage = resp.Answers, resp.Usage
		}
		var probs map[string]float64
		if err == nil {
			probs, err = yesProbabilities(resp)
		}
		if err != nil {
			c.Err = err
			w.report(c)
			return dashi.ModelDecision{}, err
		}
		key = w.confirmed(probs)
		reason = fmt.Sprintf("%s; repeating %.2f, off_request %.2f (request %s)",
			detail, probs[keyRepeating], probs[keyOffRequest], resp.RequestID)
	}
	if key == "" || limit < 0 {
		w.report(c)
		return dashi.ModelDecision{}, nil
	}
	c.Action = ActionRemind
	w.report(c)
	return dashi.ModelDecision{
		AddMessages: []dashi.ContextItem{reminder(sent+1, reminderText(key, trigger))},
		// The confirmed key comes first, so the record says which reminder
		// the agent got.
		Reason:       key + ": " + reason,
		RulesVersion: c.Model,
	}, nil
}

func (w *Watcher) report(c Check) {
	if w.OnCheck != nil {
		w.OnCheck(c)
	}
}

// confirmed returns the key whose P(yes) clears the floor, preferring
// "repeating", or "" when neither does.
func (w *Watcher) confirmed(probs map[string]float64) string {
	for _, key := range []string{keyRepeating, keyOffRequest} {
		p := probs[key]
		if w.MinProbability == 0 && p > 0.5 ||
			w.MinProbability > 0 && p >= w.MinProbability {
			return key
		}
	}
	return ""
}

// reminder builds the n-th reminder of a turn.
func reminder(n int, text string) dashi.ContextItem {
	return dashi.ContextItem{
		ID:     Name + "-" + strconv.Itoa(n),
		Name:   Name,
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    Name,
			Tier:    llm.ReminderContextual,
			Content: text,
		}),
	}
}

// remindersSent reads how many reminders the turn holds from the latest
// one's ID, turnwatch-<n>.
func remindersSent(turn dashi.TurnView) int {
	item, ok := turn.Context(dashi.ContextRuntime, Name)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(item.ID, Name+"-"))
	if err != nil || n < 1 {
		// Not one of ours, but the slot is in use: count it once.
		return 1
	}
	return n
}

// sinceLastReply returns the tool call IDs of the latest assistant message
// in messages, and whether a watcher reminder follows it. With no assistant
// message, such as after a summary replaced them all, it returns no calls
// and whether any message is a watcher reminder.
func sinceLastReply(messages []llm.Message) (calls []string, reminded bool) {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant {
			last = i
			break
		}
	}
	for _, m := range messages[last+1:] {
		if isReminder(m) {
			reminded = true
		}
	}
	if last < 0 {
		return nil, reminded
	}
	for _, part := range messages[last].Content {
		if part.Kind == llm.ContentToolCall && part.ToolCall != nil {
			calls = append(calls, part.ToolCall.ID)
		}
	}
	return calls, reminded
}

// isReminder reports whether m is one of the watcher's reminders: a
// runtime context item in the watcher's slot or, when the slot is unknown,
// a reminder with the watcher's name.
func isReminder(m llm.Message) bool {
	if m.Context != nil {
		return m.Context.Origin == string(dashi.ContextRuntime) && m.Context.Name == Name
	}
	for _, part := range m.Content {
		if part.Kind == llm.ContentReminder && part.Reminder != nil &&
			part.Reminder.Name == Name {
			return true
		}
	}
	return false
}
