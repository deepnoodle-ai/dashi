package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// beforeModel runs the BeforeModel chain, applying each hook's settings and
// hides to input. It returns the items to add, the tools hidden from
// this call in the order they were first hidden, or a hook's stop. A hook
// that fails or answers invalidly is skipped with a warning, since
// BeforeModel only shapes the request. The returned RunResult is set when the
// run's context ended during a hook.
func (r *runtime) beforeModel(
	s *replayState,
	input *llm.Input,
	finalReplyOnly bool,
	purpose ModelPurpose,
) ([]ContextItem, []string, *Stop, *RunResult) {
	var deliver []ContextItem
	var hidden []string
	ignored := hideIgnored{}
	bound := map[string]bool{}
	for _, def := range input.Tools {
		bound[def.Name] = true
	}
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeModel {
		d, err := callHook(hookContext(r.ctx, s.TurnView), h, ModelPlan{
			Purpose: purpose,
			Input:   llmclone.Input(*input),
			Turn:    snapshotView(s.TurnView),
		})
		// The candidate input is this hook's settings and hides applied on
		// top of every earlier hook's, so validation sees what the call
		// would send whatever the hooks' order.
		candidate := llmclone.Input(*input)
		candidateHidden := hidden
		var warnings []Warning
		hookIgnored := hideIgnored{}
		if err == nil {
			if d.Settings != nil {
				candidate.Settings = *d.Settings
			}
			if !finalReplyOnly {
				candidateHidden = hideTools(
					&candidate,
					hidden,
					d.Hide,
					bound,
					&hookIgnored,
				)
				warnings = keepToolChoiceOffered(&candidate, h.Name, candidateHidden)
			}
			err = validModelDecision(d, candidate)
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return nil, nil, nil, &o
			}
			r.hookFailed(HookBeforeModel, h.Name, err, true)
			continue
		}
		if d.Stop != nil {
			r.addHookResult(HookResult{
				Point:        HookBeforeModel,
				Name:         h.Name,
				Verdict:      VerdictStop,
				RulesVersion: d.RulesVersion,
				Reason:       cmp.Or(d.Reason, d.Stop.Reason),
			})
			stop := *d.Stop
			if stop.Limit != nil {
				limit := *stop.Limit
				stop.Limit = &limit
			}
			return nil, nil, &stop, nil
		}
		ignored.merge(hookIgnored)
		r.pendingWarnings = append(r.pendingWarnings, warnings...)
		hid := len(candidateHidden) > len(hidden)
		if d.Settings == nil && len(d.AddMessages) == 0 && !hid {
			if d.Reason != "" || d.RulesVersion != "" {
				// A hook that looked and changed nothing can still leave
				// a record, so it knows on a retried call that it ran.
				r.addHookResult(HookResult{
					Point:        HookBeforeModel,
					Name:         h.Name,
					Verdict:      VerdictNoChange,
					RulesVersion: d.RulesVersion,
					Reason:       d.Reason,
				})
			}
			continue
		}
		input.Settings = candidate.Settings
		input.Tools = candidate.Tools
		hidden = candidateHidden
		for _, item := range d.AddMessages {
			deliver = append(deliver, cloneContextItem(item))
		}
		r.addHookResult(HookResult{
			Point:        HookBeforeModel,
			Name:         h.Name,
			Verdict:      VerdictChange,
			RulesVersion: d.RulesVersion,
			Reason:       d.Reason,
		})
	}
	if w, ok := ignored.warning(); ok {
		r.pendingWarnings = append(r.pendingWarnings, w)
	}
	return deliver, hidden, nil, nil
}

// hideTools removes names from in.Tools and returns the hidden names so
// far, earlier hides first. dashi_submit_output is never hidden, and a name
// no tool of this call has is noted in ignored; a name an earlier hook
// already hid is not.
func hideTools(
	in *llm.Input,
	hidden []string,
	names []string,
	bound map[string]bool,
	ignored *hideIgnored,
) []string {
	out := slices.Clone(hidden)
	hide := map[string]bool{}
	for _, name := range names {
		switch {
		case name == SubmitOutputTool:
			ignored.submit = true
		case !bound[name]:
			ignored.add(name)
		case !slices.Contains(out, name):
			out = append(out, name)
			hide[name] = true
		}
	}
	if len(hide) == 0 {
		return out
	}
	kept := make([]tool.Definition, 0, len(in.Tools))
	for _, def := range in.Tools {
		if !hide[def.Name] {
			kept = append(kept, def)
		}
	}
	in.Tools = kept
	return out
}

// keepToolChoiceOffered clears a forced tool choice that names a hidden
// tool, whichever hook hid it: the hide wins. When no tools remain, the tool
// choice and ParallelTools are cleared, as on the reserved final-reply
// call, since providers reject them without tools. Only losing a forced
// choice warns.
func keepToolChoiceOffered(
	in *llm.Input,
	hook string,
	hidden []string,
) []Warning {
	choice := in.Settings.ToolChoice
	forced := false
	if choice != nil {
		named := choice.Mode == llm.ToolChoiceTool &&
			slices.Contains(hidden, choice.Name)
		forced = named ||
			len(in.Tools) == 0 && len(hidden) > 0 &&
				(choice.Mode == llm.ToolChoiceAny || choice.Mode == llm.ToolChoiceTool)
		if forced {
			in.Settings.ToolChoice = nil
		}
	}
	if len(in.Tools) == 0 && len(hidden) > 0 {
		in.Settings.ToolChoice = nil
		in.Settings.ParallelTools = nil
	}
	if !forced {
		return nil
	}
	return []Warning{{
		Code: WarningToolChoiceCleared,
		Message: fmt.Sprintf(
			"after before_model hook %q, the forced tool choice named a "+
				"hidden tool or no tools remained; it was cleared for this call",
			hook,
		),
	}}
}

// hideIgnored collects the hide names a call could not apply, for one
// warning per call.
type hideIgnored struct {
	submit  bool
	unknown []string
}

func (h *hideIgnored) add(name string) {
	if !slices.Contains(h.unknown, name) {
		h.unknown = append(h.unknown, name)
	}
}

func (h *hideIgnored) merge(o hideIgnored) {
	h.submit = h.submit || o.submit
	for _, name := range o.unknown {
		h.add(name)
	}
}

func (h hideIgnored) warning() (Warning, bool) {
	var parts []string
	if h.submit {
		parts = append(parts, SubmitOutputTool+" cannot be hidden")
	}
	if len(h.unknown) > 0 {
		parts = append(parts, fmt.Sprintf(
			"no tool is named %s",
			strings.Join(h.unknown, ", "),
		))
	}
	if len(parts) == 0 {
		return Warning{}, false
	}
	return Warning{
		Code: WarningHideIgnored,
		Message: "before_model hide names were ignored: " +
			strings.Join(parts, "; "),
	}, true
}

// validModelDecision checks a BeforeModel answer before any of it applies,
// including its settings against the rest of the input, so an invalid
// answer skips the hook instead of failing request preparation.
func validModelDecision(d ModelDecision, input llm.Input) error {
	if d.Settings != nil || len(d.Hide) > 0 {
		if err := input.Validate(); err != nil {
			return fmt.Errorf("invalid settings or hide: %w", err)
		}
	}
	if d.Stop != nil && !validHookStop(*d.Stop) {
		return errors.New(
			"invalid stop: a hook may refuse, fail, or report a host, cost, " +
				"or token limit, and may not use a reserved stop reason",
		)
	}
	for _, item := range d.AddMessages {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("invalid context item: %w", err)
		}
	}
	return nil
}

// beforeSend runs the BeforeSend chain for a prepared request. It returns
// the reservations by hook name, or the stop that ends the run: a hook's
// denial, or StopReasonHookFailed when a hook fails, since BeforeSend guards
// spending.
func (r *runtime) beforeSend(
	s *replayState,
	req llm.Request,
	atRisk *llm.Cost,
	remaining *Money,
	purpose ModelPurpose,
) (map[string]json.RawMessage, *Stop, *RunResult) {
	var terms map[string]json.RawMessage
	total := 0
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeSend {
		plan := SendPlan{Purpose: purpose, Request: req, Turn: snapshotView(s.TurnView)}
		if atRisk != nil {
			plan.WorstCaseCost = new(*atRisk)
		}
		if remaining != nil {
			plan.Remaining = new(*remaining)
		}
		d, err := callHook(hookContext(r.ctx, s.TurnView), h, plan)
		if err == nil {
			err = validSendDecision(d, total)
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return nil, nil, &o
			}
			r.hookFailed(HookBeforeSend, h.Name, err, false)
			return nil, hookFailedStop(), nil
		}
		if d.Deny || d.Limit != nil {
			result := HookResult{
				Point:   HookBeforeSend,
				Name:    h.Name,
				Verdict: VerdictDeny,
				Reason:  d.Reason,
			}
			stop := &Stop{Kind: StopRefused, Reason: d.Reason}
			if d.Limit != nil {
				result.Verdict = VerdictLimit
				limit := *d.Limit
				stop = &Stop{Kind: StopLimited, Reason: d.Reason, Limit: &limit}
			}
			r.addHookResult(result)
			return nil, stop, nil
		}
		if len(d.Reservation) > 0 {
			if terms == nil {
				terms = map[string]json.RawMessage{}
			}
			terms[h.Name] = slices.Clone(d.Reservation)
			total += len(d.Reservation)
			r.addHookResult(HookResult{
				Point:   HookBeforeSend,
				Name:    h.Name,
				Verdict: VerdictReserve,
			})
		}
	}
	return terms, nil, nil
}

func validSendDecision(d SendDecision, used int) error {
	if d.Limit != nil && !validHookLimit(*d.Limit) {
		return errors.New(
			"invalid limit: a hook may report a host, cost, or token limit",
		)
	}
	if len(d.Reservation) > 0 && !json.Valid(d.Reservation) {
		return errors.New("reservation is not valid JSON")
	}
	if used+len(d.Reservation) > maxReservation {
		return errors.New("reservations exceed 16 KiB")
	}
	return nil
}
