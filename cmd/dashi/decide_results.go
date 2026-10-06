package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
// the hook results of its step, or returns nil. The hook records only a
// result it warned the model about, or a check that failed; a result
// decide passed leaves no mark.
func resultMark(results []dashi.HookResult) *decideMark {
	for _, h := range results {
		if h.Name != toolcheck.PromptInjectionName {
			continue
		}
		switch h.Verdict {
		case dashi.VerdictReplace:
			reason, flagged := strings.CutPrefix(h.Reason, toolcheck.FlaggedPrefix)
			answer, _, _ := strings.Cut(reason, " (")
			if flagged {
				return &decideMark{Flagged: true, Text: "decide: " + answer + " — flagged, agent warned"}
			}
			// Too long to check whole: the model is told to treat it as
			// untrusted.
			return &decideMark{Text: "decide: " + answer + " — agent warned"}
		case dashi.VerdictFailed:
			return &decideMark{Text: "decide: unchecked (" + decideCauseText(h.Error) + ")"}
		}
	}
	return nil
}

// watchHint returns the line that shows a turn-watcher reminder from a
// step's hook results, or false.
func watchHint(s dashi.Step) (Block, bool) {
	for i, h := range s.HookResults {
		if h.Name != turnwatch.Name || h.Verdict != dashi.VerdictChange {
			continue
		}
		return Block{
			ID:   fmt.Sprintf("decide:%s:%d:%d", s.TurnID, s.Seq, i),
			Kind: KindHint,
			Text: "decide: reminded the agent" + watchAnswer(h.Reason),
			Done: true,
		}, true
	}
	return Block{}, false
}

// watchAnswer reads the confirmed question and its probability from a
// turnwatch Reason, such as "repeating: failed_again: bash, 2 times;
// repeating 0.88, off_request 0.10 (request r_1)", as " (repeating
// 88%)". It returns "" for a Reason it cannot read.
func watchAnswer(reason string) string {
	key, rest, ok := strings.Cut(reason, ": ")
	if !ok {
		return ""
	}
	label := strings.ReplaceAll(key, "_", " ")
	if i := strings.LastIndex(rest, "; "); i >= 0 {
		answers, _, _ := strings.Cut(rest[i+2:], " (")
		for answer := range strings.SplitSeq(answers, ", ") {
			number, found := strings.CutPrefix(answer, key+" ")
			if p, err := strconv.ParseFloat(number, 64); found && err == nil {
				return " (" + label + " " + percent(p) + ")"
			}
		}
	}
	return " (" + label + ")"
}

// decideHooks are the hooks whose failures the CLI shows itself, as a row
// line and in /settings, instead of as a warning per call.
var decideHooks = []string{toolcheck.PromptInjectionName, turnwatch.Name}

// isDecideHookWarning reports whether w is the hook_failed warning of a
// decide hook.
func isDecideHookWarning(w dashi.Warning) bool {
	if w.Code != dashi.WarningHookFailed {
		return false
	}
	for _, name := range decideHooks {
		if strings.Contains(w.Message, " hook "+name+" failed: ") {
			return true
		}
	}
	return false
}

// decideStepCounts counts, from a turn's steps, the results the
// prompt-injection check flagged, warned about, or could not check, and
// the turn watcher's reminders.
func decideStepCounts(steps []dashi.Step) (checked, flagged, unchecked, reminded int) {
	for _, s := range steps {
		for _, h := range s.HookResults {
			switch {
			case h.Name == toolcheck.PromptInjectionName && h.Verdict == dashi.VerdictReplace:
				checked++
				if strings.HasPrefix(h.Reason, toolcheck.FlaggedPrefix) {
					flagged++
				}
			case h.Name == toolcheck.PromptInjectionName && h.Verdict == dashi.VerdictFailed:
				unchecked++
			case h.Name == turnwatch.Name && h.Verdict == dashi.VerdictChange:
				reminded++
			}
		}
	}
	return checked, flagged, unchecked, reminded
}

// statusCode finds the HTTP status in decide's error text, "decide: 503".
var statusCode = regexp.MustCompile(`decide: (\d{3})\b`)

// decideCauseText names, from a recorded hook error's text, why decide
// could not answer, in the words decideCause uses for the error itself.
// The record keeps the text only.
func decideCauseText(text string) string {
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("deadline exceeded"):
		return "timeout"
	case has("too large"):
		return "too large"
	case has("no API key", "no credentials"):
		return "no credentials"
	case has("authentication failed"):
		return "auth failed"
	case has("rate limited"):
		return "rate limited"
	case has("server error", "overloaded"):
		return "server error"
	case has("invalid answer"):
		return "invalid answer"
	case has("validation failed", "invalid request"):
		return "rejected"
	}
	if m := statusCode.FindStringSubmatch(text); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch {
		case code == 401, code == 403:
			return "auth failed"
		case code == 429:
			return "rate limited"
		case code >= 500:
			return "server error"
		default:
			return "rejected"
		}
	}
	switch {
	case has("Timeout", "timeout", "i/o timeout"):
		return "timeout"
	case has("connection refused", "connection reset", "no such host", "EOF", "dial tcp"):
		return "network error"
	}
	return "error"
}
