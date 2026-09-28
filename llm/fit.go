package llm

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// effortOrder ranks the reasoning effort names used across supported APIs,
// from least to most reasoning. Fit uses it to find the nearest accepted level;
// a name outside this list or effortAliases has no known neighbor and cannot
// be adjusted.
var effortOrder = []string{
	"none",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

// effortAliases ranks other spellings of a level as that level, so an effort
// written as "extra-high" fits like "xhigh" (design/decisions.md, entry 4). An
// alias is never sent: when no model lists it, Fit replaces it with an
// accepted name and records the change.
var effortAliases = map[string]string{
	"extra-high": "xhigh",
	"extra_high": "xhigh",
	"extra high": "xhigh",
}

// Fit brings settings within known limits before a provider encodes them.
// Under CapabilityAdjust a value beyond a known limit becomes the nearest
// accepted value, and a setting the destination rejects is omitted when the
// model default keeps the request's meaning; each change is an Adjustment that
// carries a warning. Under CapabilityStrict the same cases are errors. Cases
// with no safe substitute are errors under both policies.
//
// prior holds adjustments already applied to r, such as provider defaults. A
// default that Fit changes stays one default at the fitted value rather than
// becoming a warning, because the caller never asked for the original value.
// The returned slice replaces prior. Unknown limits are left alone; Check
// reports unknown feature support separately.
func (c Capabilities) Fit(
	r Input,
	policy CapabilityPolicy,
	prior []Adjustment,
) (Input, []Adjustment, error) {
	if !policy.Valid() {
		return Input{}, nil, fmt.Errorf(
			"llm: invalid capability policy %q",
			policy,
		)
	}
	f := fitter{
		model:       r.Model,
		policy:      policy,
		adjustments: slices.Clone(prior),
	}
	s := &r.Settings
	if limit := c.MaxOutputTokens; limit > 0 && s.MaxTokens > limit {
		err := f.clamp(
			SettingMaxTokens,
			s.MaxTokens,
			limit,
			fmt.Sprintf(
				"max_tokens %d exceeds the known limit %d; sent %d",
				s.MaxTokens,
				limit,
				limit,
			),
			fmt.Errorf(
				"llm: max_tokens exceeds model limit %d",
				limit,
			),
		)
		if err != nil {
			return Input{}, nil, err
		}
		s.MaxTokens = limit
	}
	if t := s.Temperature; t != nil {
		switch limit := c.MaxTemperature; {
		case c.Features[FeatureTemperature] == SupportUnsupported:
			err := f.omit(
				SettingTemperature,
				*t,
				"temperature is not accepted here; the model default applies",
				unsupportedError(r.Model, FeatureTemperature),
			)
			if err != nil {
				return Input{}, nil, err
			}
			s.Temperature = nil
		case limit > 0 && *t > limit:
			err := f.clamp(
				SettingTemperature,
				*t,
				limit,
				fmt.Sprintf(
					"temperature %g exceeds the known maximum %g; sent %g",
					*t,
					limit,
					limit,
				),
				fmt.Errorf(
					"llm: temperature must be at most %g",
					limit,
				),
			)
			if err != nil {
				return Input{}, nil, err
			}
			s.Temperature = &limit
		}
	}
	if reasoning := s.Reasoning; reasoning != nil {
		fitted, err := c.fitReasoning(&f, *reasoning)
		if err != nil {
			return Input{}, nil, err
		}
		s.Reasoning = &fitted
	}
	parallelRejected := c.Features[FeatureParallelTools] == SupportUnsupported
	if p := s.ParallelTools; p != nil && parallelRejected {
		// Omitting a request for parallel calls leaves the model free to call
		// tools one at a time, which the agent loop handles either way. A
		// request to disable them cannot be honored by omission, because the
		// model might then call tools in parallel.
		if !*p {
			return Input{}, nil, fmt.Errorf(
				"llm: model %q cannot be told to disable parallel tool calls; "+
					"remove ParallelTools or use a model that accepts the setting",
				r.Model,
			)
		}
		err := f.omit(
			SettingParallelTools,
			true,
			"parallel_tools is not accepted here; the model default applies",
			unsupportedError(r.Model, FeatureParallelTools),
		)
		if err != nil {
			return Input{}, nil, err
		}
		s.ParallelTools = nil
	}
	return r, f.adjustments, nil
}

// Intersect combines two sets of claims so the result accepts only what both
// accept. Providers use it to add limits that apply to a whole API, such as
// its effort names or temperature range, to a model's evidence before Fit.
func (c Capabilities) Intersect(o Capabilities) Capabilities {
	out := Capabilities{
		MaxOutputTokens: minLimit(
			c.MaxOutputTokens,
			o.MaxOutputTokens,
		),
		ContextWindow: minLimit(c.ContextWindow, o.ContextWindow),
		MinReasoningBudget: max(
			c.MinReasoningBudget,
			o.MinReasoningBudget,
		),
		MaxReasoningBudget: minLimit(
			c.MaxReasoningBudget,
			o.MaxReasoningBudget,
		),
		MaxTemperature: minLimit(
			c.MaxTemperature,
			o.MaxTemperature,
		),
		Features: make(map[Feature]Support),
	}
	for _, features := range []map[Feature]Support{
		c.Features,
		o.Features,
	} {
		for f := range features {
			a, b := c.Features[f], o.Features[f]
			switch {
			case a == SupportUnsupported ||
				b == SupportUnsupported:
				out.Features[f] = SupportUnsupported
			case a == SupportSupported && b == SupportSupported:
				out.Features[f] = SupportSupported
			}
		}
	}
	switch {
	case len(c.ReasoningEfforts) == 0:
		out.ReasoningEfforts = slices.Clone(o.ReasoningEfforts)
	case len(o.ReasoningEfforts) == 0:
		out.ReasoningEfforts = slices.Clone(c.ReasoningEfforts)
	default:
		for _, effort := range c.ReasoningEfforts {
			if slices.Contains(o.ReasoningEfforts, effort) {
				out.ReasoningEfforts = append(
					out.ReasoningEfforts,
					effort,
				)
			}
		}
		if len(out.ReasoningEfforts) == 0 {
			// An empty list means no constraint, so an empty intersection
			// must be stated as known rejection instead.
			out.Features[FeatureReasoningEffort] = SupportUnsupported
		}
	}
	return out
}

func (c Capabilities) fitReasoning(
	f *fitter,
	s ReasoningSettings,
) (ReasoningSettings, error) {
	requested := s
	if s.Effort != "" {
		if c.Features[FeatureReasoningEffort] == SupportUnsupported {
			return s, unsupportedError(
				f.model,
				FeatureReasoningEffort,
			)
		}
		levels := c.ReasoningEfforts
		if len(levels) == 0 || slices.Contains(levels, s.Effort) {
			return s, nil
		}
		accepted := strings.Join(c.ReasoningEfforts, ", ")
		strict := fmt.Errorf(
			"llm: reasoning effort %q is unsupported; use one of %s",
			s.Effort,
			accepted,
		)
		nearest, ok := nearestEffort(s.Effort, c.ReasoningEfforts)
		if !ok {
			return s, strict
		}
		s.Effort = nearest
		return s, f.clamp(
			SettingReasoning,
			requested,
			s,
			fmt.Sprintf(
				"reasoning effort %q is not accepted (accepted: %s); sent %q",
				requested.Effort,
				accepted,
				nearest,
			),
			strict,
		)
	}
	if c.Features[FeatureReasoningBudget] == SupportUnsupported {
		return s, unsupportedError(f.model, FeatureReasoningBudget)
	}
	low, high := c.MinReasoningBudget, c.MaxReasoningBudget
	if high > 0 && low > high {
		return s, fmt.Errorf(
			"llm: no reasoning budget fits between %d and %d; "+
				"raise max_tokens or remove the reasoning budget",
			low,
			high,
		)
	}
	switch {
	case s.BudgetTokens < low:
		s.BudgetTokens = low
	case high > 0 && s.BudgetTokens > high:
		s.BudgetTokens = high
	default:
		return s, nil
	}
	bounds := fmt.Sprintf("at least %d", low)
	if high > 0 {
		bounds = fmt.Sprintf("%d to %d", low, high)
	}
	return s, f.clamp(
		SettingReasoning,
		requested,
		s,
		fmt.Sprintf(
			"reasoning budget %d is outside the accepted range (%s); sent %d",
			requested.BudgetTokens,
			bounds,
			s.BudgetTokens,
		),
		fmt.Errorf(
			"llm: reasoning budget %d is outside model limits (%s)",
			requested.BudgetTokens,
			bounds,
		),
	)
}

// fitter accumulates Fit's decisions and applies the policy to each one.
type fitter struct {
	model       string
	policy      CapabilityPolicy
	adjustments []Adjustment
}

func (f *fitter) clamp(
	field Setting,
	requested, effective any,
	reason string,
	strict error,
) error {
	return f.record(
		field,
		AdjustmentClamp,
		requested,
		effective,
		reason,
		strict,
	)
}

func (f *fitter) omit(
	field Setting,
	requested any,
	reason string,
	strict error,
) error {
	return f.record(field, AdjustmentOmit, requested, nil, reason, strict)
}

// record applies the policy to one decision. Under strict it returns the
// prepared error; otherwise it adds the decision or folds it into an earlier
// one for the same field, since NewRequest allows one decision per field.
func (f *fitter) record(
	field Setting,
	action AdjustmentAction,
	requested, effective any,
	reason string,
	strict error,
) error {
	if f.policy == CapabilityStrict {
		return strict
	}
	value, _ := json.Marshal(effective)
	i := slices.IndexFunc(f.adjustments, func(a Adjustment) bool {
		return a.Field == field
	})
	if i < 0 {
		raw, _ := json.Marshal(requested)
		f.adjustments = append(
			f.adjustments,
			Adjustment{
				Field:     field,
				Action:    action,
				Requested: raw,
				Effective: value,
				Reason:    reason,
			},
		)
		return nil
	}
	prior := &f.adjustments[i]
	switch {
	case prior.Action == AdjustmentDefault && action == AdjustmentOmit:
		// A default that the destination rejects is simply not applied.
		f.adjustments = slices.Delete(f.adjustments, i, i+1)
	case prior.Action == AdjustmentDefault:
		prior.Effective = value
		prior.Reason += "; " + reason
	default:
		prior.Action = action
		prior.Effective = value
		prior.Reason += "; " + reason
	}
	return nil
}

// nearestEffort picks the highest accepted level at or below the request, so
// an adjustment never adds reasoning spend, and otherwise the lowest accepted
// level above it. The downward search stops above "none": a caller who asked
// for reasoning must not have it silently turned off. For the same reason
// "none" is never raised, because turning reasoning on is not a nearer version
// of turning it off. With no level on either side there is no safe substitute.
func nearestEffort(requested string, accepted []string) (string, bool) {
	rank := effortRank(requested)
	if rank <= 0 {
		return "", false
	}
	for i := rank; i > 0; i-- {
		if slices.Contains(accepted, effortOrder[i]) {
			return effortOrder[i], true
		}
	}
	for i := rank + 1; i < len(effortOrder); i++ {
		if slices.Contains(accepted, effortOrder[i]) {
			return effortOrder[i], true
		}
	}
	return "", false
}

// effortRank returns the index of name, or of the level it aliases, in
// effortOrder, and -1 for an unranked name.
func effortRank(name string) int {
	if level, ok := effortAliases[name]; ok {
		name = level
	}
	return slices.Index(effortOrder, name)
}

// unsupportedError names the fix for a feature the model is known to reject.
// Switching policy is not a fix, since no policy can make the model accept it.
func unsupportedError(model string, f Feature) error {
	fix := "remove it from the request or use a model that supports it"
	switch f {
	case FeatureImage, FeatureDocument, FeatureAudio, FeatureResource:
		fix = fmt.Sprintf(
			"remove the %s content or use a model that accepts it",
			f,
		)
	case FeatureTools:
		fix = "remove tools and tool history or use a model with tool support"
	case FeatureToolChoice:
		fix = "remove ToolChoice or use a model that supports it"
	case FeatureStreaming:
		fix = "set Stream to false or use a model that supports streaming"
	case FeatureReasoningEffort:
		fix = "use a reasoning budget, remove Reasoning, " +
			"or use a model with effort levels"
	case FeatureReasoningBudget:
		fix = "use a reasoning effort, remove Reasoning, " +
			"or use a model with reasoning budgets"
	}
	return fmt.Errorf(
		"llm: model %q does not support %s; %s",
		model,
		f,
		fix,
	)
}

// minLimit returns the tighter of two limits where zero means no limit.
func minLimit[T int | float64](a, b T) T {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}
