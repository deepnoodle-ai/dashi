package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
)

// This file wires the two decide features that work through library
// hooks: the prompt-injection check, an AfterTool hook that warns the
// model about a tool result, and the turn watcher, a BeforeModel hook
// that reminds the agent when it repeats a failing call. The library
// hooks do the work and write the turn record; the CLI counts their checks
// and draws what the record says.

// decideWiring adds the hooks of the decide features that run as library
// hooks. The command-risk check is not one: it runs in the approval step.
func decideWiring(env Env) (wiring, error) {
	return wiring{Hooks: env.Decide.hooks()}, nil
}

// hooks returns the prompt-injection and turn-watcher hooks that are on
// and can run. With a setup error, it returns none.
func (rt *decideRuntime) hooks() dashi.Hooks {
	var sets []dashi.HookSet
	if rt.runs(decidePromptInjection) {
		sets = append(sets, toolcheck.PromptInjection{Client: rt.client, OnCheck: rt.onInjection}.Hooks())
	}
	if rt.runs(decideTurnWatcher) {
		w := &turnwatch.Watcher{Client: rt.client, OnCheck: rt.onWatch}
		sets = append(sets, w.Hook())
	}
	return dashi.JoinHooks(sets...)
}

// onInjection counts one prompt-injection check. It runs in the hook,
// from many turns at once.
func (rt *decideRuntime) onInjection(v toolcheck.Verdict, err error) {
	r, warning := rt.record(decidePromptInjection, &rt.results, errors.Is(err, context.Canceled), v, err)
	if warning != "" && rt.warn != nil {
		rt.warn(warning)
	}
	if r.Outage && rt.outage != nil {
		rt.outage(decidePromptInjection)
	}
}

// onWatch counts one turn-watcher check. It runs in the hook, before a
// model call.
func (rt *decideRuntime) onWatch(c turnwatch.Check) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.asked = true
	if c.Err != nil && errors.Is(c.Err, context.Canceled) {
		return
	}
	rt.totals.Requests++
	rt.totals.InputTokens += c.Usage.InputTokens
	rt.totals.OutputTokens += c.Usage.OutputTokens
	if c.Err != nil {
		rt.watch.Unchecked++
		return
	}
	rt.watch.Checked++
	if c.Model != "" {
		rt.model = c.Model
	}
	if c.Action == turnwatch.ActionRemind {
		rt.watch.Reminded++
	}
}

// resultMark reads the prompt-injection answer of a finished call from
// the hook results of its step, or returns nil when the check did not read
// the result.
func resultMark(results []dashi.HookResult) *decideMark {
	for _, h := range results {
		r, ok := toolcheck.ReadResultRecord(h)
		if !ok {
			continue
		}
		m := &decideMark{Check: decideCheckResult, Flagged: r.Flagged}
		switch {
		case !r.Checked:
			m.Text = "unchecked (" + string(r.Cause) + ")"
		default:
			m.Text = fmt.Sprintf("injection %s, hidden %s", percent(r.Probabilities[toolcheck.QuestionInjection]),
				percent(r.Probabilities[toolcheck.QuestionHidden]))
			if r.Partial {
				m.Text += ", start and end only"
			}
			m.OK = !r.Warned
		}
		switch {
		case r.Flagged:
			m.Text += " — flagged, agent warned"
		case r.Warned:
			m.Text += " — agent warned"
		}
		return m
	}
	return nil
}

// watchHint returns the line that shows a turn-watcher reminder from a
// step's hook results, or false.
func watchHint(s dashi.Step) (Block, bool) {
	for i, h := range s.HookResults {
		r, ok := turnwatch.ReadReminder(h)
		if !ok {
			continue
		}
		return Block{
			ID:   fmt.Sprintf("decide:%s:%d:%d", s.TurnID, s.Seq, i),
			Kind: KindHint,
			Text: "decide: reminded the agent" + watchAnswer(r),
			Done: true,
		}, true
	}
	return Block{}, false
}

// watchWords says each turn-watcher question in plain words.
var watchWords = map[string]string{
	turnwatch.QuestionRepeating:  "repeating a failed approach",
	turnwatch.QuestionOffRequest: "drifting from your request",
}

// watchAnswer is the confirmed question and its probability, such as
// " (repeating a failed approach, 88%)", or the question alone when the
// watcher did not ask decide.
func watchAnswer(r turnwatch.Reminder) string {
	words := cmp.Or(watchWords[r.Question], strings.ReplaceAll(r.Question, "_", " "))
	if r.Probability == 0 {
		return " (" + words + ")"
	}
	return " (" + words + ", " + percent(r.Probability) + ")"
}

// decideHooks are the hooks whose failures the CLI shows itself, as a row
// line and in /settings, instead of as a warning per call.
var decideHooks = []string{toolcheck.PromptInjectionName, turnwatch.Name}

// isDecideHookWarning reports whether w is the hook_failed warning of a
// decide hook.
func isDecideHookWarning(w dashi.Warning) bool {
	return w.Code == dashi.WarningHookFailed && slices.Contains(decideHooks, w.Hook)
}

// decideStepCounts counts, from a turn's steps, the results the
// prompt-injection check answered for, flagged, or could not check, and
// the turn watcher's reminders.
func decideStepCounts(steps []dashi.Step) (checked, flagged, unchecked, reminded int) {
	for _, s := range steps {
		for _, h := range s.HookResults {
			if r, ok := toolcheck.ReadResultRecord(h); ok {
				switch {
				case !r.Checked:
					unchecked++
				case r.Flagged:
					checked++
					flagged++
				default:
					checked++
				}
				continue
			}
			if _, ok := turnwatch.ReadReminder(h); ok {
				reminded++
			}
		}
	}
	return checked, flagged, unchecked, reminded
}
