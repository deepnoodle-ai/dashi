package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Setting identifies a field in the canonical Settings JSON representation.
type Setting string

const (
	// SettingMaxTokens identifies the output token limit.
	SettingMaxTokens Setting = "max_tokens"
	// SettingTemperature identifies the sampling setting, including zero.
	SettingTemperature Setting = "temperature"
	// SettingReasoning identifies the effort or budget selection as one value.
	SettingReasoning Setting = "reasoning"
	// SettingReasoningSummary identifies the public-summary request.
	SettingReasoningSummary Setting = "reasoning_summary"
	// SettingToolChoice identifies the tool selection constraint.
	SettingToolChoice Setting = "tool_choice"
	// SettingParallelTools identifies the parallel tool call setting.
	SettingParallelTools Setting = "parallel_tools"
	// SettingCitations identifies the document citations setting.
	SettingCitations Setting = "citations"
	// SettingPromptCacheTTL identifies the prompt cache lifetime,
	// PromptCache.TTL. Its values are duration strings such as "1h".
	SettingPromptCacheTTL Setting = "prompt_cache.ttl"
	// SettingTaskBudget identifies the task budget as one value.
	SettingTaskBudget Setting = "task_budget"
)

// AdjustmentAction names an authorized preparation behavior. Defaults fill
// omissions silently; clamps and omissions replace a caller's value and are
// therefore always warnings. Emulation has no action in the current contract.
type AdjustmentAction string

const (
	// AdjustmentDefault fills an omitted setting without replacing what the
	// user asked for.
	AdjustmentDefault AdjustmentAction = "default"
	// AdjustmentClamp replaces a requested value with the nearest value the
	// model or API is known to accept, such as effort "max" becoming "xhigh".
	AdjustmentClamp AdjustmentAction = "clamp"
	// AdjustmentOmit removes a setting the destination is known to reject when
	// the model's own default keeps the request's meaning.
	AdjustmentOmit AdjustmentAction = "omit"
)

// Adjustment records a setting change without requiring callers to parse
// prose. Requested and Effective use the setting's JSON encoding; null means
// omitted, including MaxTokens zero. Copies share JSON bytes; Clone detaches.
type Adjustment struct {
	// Field selects the setting whose value changed.
	Field Setting `json:"field"`

	// Action identifies the policy applied to this setting.
	Action AdjustmentAction `json:"action"`

	// Requested preserves the original setting value, or null for omission.
	Requested json.RawMessage `json:"requested"`

	// Effective records the value encoded in the prepared body, or null when
	// the setting was omitted.
	Effective json.RawMessage `json:"effective"`

	// Reason supplies diagnostic context; branch on Field and Action instead.
	Reason string `json:"reason"`
}

// Clone returns a decision without shared JSON bytes.
func (a Adjustment) Clone() Adjustment {
	a.Requested = slices.Clone(a.Requested)
	a.Effective = slices.Clone(a.Effective)
	return a
}

// Warning reports whether this decision replaced a value the caller asked
// for, and describes it for the caller. Defaults are not warnings because
// they fill an omission rather than overriding what the caller asked for.
func (a Adjustment) Warning() (Warning, bool) {
	switch a.Action {
	case AdjustmentClamp:
		return Warning{
			Code:    WarningSettingClamped,
			Field:   a.Field,
			Message: a.Reason,
		}, true
	case AdjustmentOmit:
		return Warning{
			Code:    WarningSettingOmitted,
			Field:   a.Field,
			Message: a.Reason,
		}, true
	}
	return Warning{}, false
}

// Validate requires a supported action and values in canonical Settings form.
// NewRequest also checks that each decision agrees with the actual snapshots.
func (a Adjustment) Validate() error {
	switch a.Field {
	case SettingMaxTokens,
		SettingTemperature,
		SettingReasoning,
		SettingReasoningSummary,
		SettingToolChoice,
		SettingParallelTools,
		SettingCitations,
		SettingPromptCacheTTL,
		SettingTaskBudget:
	default:
		return fmt.Errorf("llm: invalid adjustment field %q", a.Field)
	}
	requested := jsonValue(a.Requested)
	effective := jsonValue(a.Effective)
	switch {
	case a.Reason == "":
		return errors.New("llm: adjustment requires a reason")
	case a.Action == AdjustmentDefault &&
		jsonNull(a.Requested) &&
		effective:
		return nil
	case a.Action == AdjustmentClamp && requested && effective &&
		!bytes.Equal(
			compactJSON(a.Requested),
			compactJSON(a.Effective),
		):
		return nil
	case a.Action == AdjustmentOmit && requested && jsonNull(a.Effective):
		return nil
	}
	return fmt.Errorf(
		"llm: invalid %q adjustment values for %s",
		a.Action,
		a.Field,
	)
}

// WarningCode is a stable reason an application can branch on or forward in
// its own API responses.
type WarningCode string

const (
	// WarningSettingClamped means a requested setting was replaced by the
	// nearest value the model or API is known to accept.
	WarningSettingClamped WarningCode = "setting_clamped"
	// WarningSettingOmitted means a requested setting was not sent because the
	// destination rejects it; the model's default applies.
	WarningSettingOmitted WarningCode = "setting_omitted"
	// WarningMIMETypeUnknown means inline bytes had no MIME type and detection
	// could not identify them, so they were sent as
	// content.UnknownMIMEType. Set Source.MIMEType to fix it.
	WarningMIMETypeUnknown WarningCode = "mime_type_unknown"
)

// Warning describes something the caller asked for that was not sent as
// asked. It is a view of the recorded Adjustment, flattened so an
// application can surface it without decoding raw setting values.
type Warning struct {
	// Code identifies the kind of warning.
	Code WarningCode `json:"code"`

	// Field names the affected setting, when the warning concerns one.
	Field Setting `json:"field,omitempty"`

	// Message explains the change in prose for logs and end users.
	Message string `json:"message"`
}

// collectWarnings returns the caller-facing warnings of the given decisions,
// in order: setting adjustments first, then content changes.
func collectWarnings(
	adjustments []Adjustment,
	changes []ContentChange,
) []Warning {
	var warnings []Warning
	for _, a := range adjustments {
		if w, ok := a.Warning(); ok {
			warnings = append(warnings, w)
		}
	}
	for _, c := range changes {
		if w, ok := c.Warning(); ok {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

func validateAdjustments(
	requested, effective Settings,
	adjustments []Adjustment,
) error {
	values := func(s Settings) map[Setting]json.RawMessage {
		raw, _ := json.Marshal(s)
		var fields map[Setting]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		// Prompt cache fields are decided one by one, so each is a setting.
		if cache, ok := fields["prompt_cache"]; ok {
			delete(fields, "prompt_cache")
			var parts map[string]json.RawMessage
			_ = json.Unmarshal(cache, &parts)
			for name, value := range parts {
				fields[Setting("prompt_cache."+name)] = value
			}
		}
		return fields
	}
	before, after := values(requested), values(effective)
	seen := make(map[Setting]bool, len(adjustments))
	for _, a := range adjustments {
		if err := a.Validate(); err != nil {
			return err
		}
		if seen[a.Field] ||
			!bytes.Equal(
				settingJSON(a.Requested),
				before[a.Field],
			) ||
			!bytes.Equal(
				settingJSON(a.Effective),
				after[a.Field],
			) {
			return fmt.Errorf(
				"llm: adjustment for %s disagrees with settings",
				a.Field,
			)
		}
		seen[a.Field] = true
	}
	for _, fields := range []map[Setting]json.RawMessage{before, after} {
		for field := range fields {
			if !bytes.Equal(
				before[field],
				after[field],
			) && !seen[field] {
				return fmt.Errorf(
					"llm: setting %s changed without an adjustment",
					field,
				)
			}
		}
	}
	return nil
}

// settingJSON puts a decision value in the form Settings marshals to, where
// an omitted field has no bytes at all.
func settingJSON(raw json.RawMessage) []byte {
	if !jsonValue(raw) {
		return nil
	}
	return compactJSON(raw)
}

func compactJSON(raw json.RawMessage) []byte {
	var compact bytes.Buffer
	_ = json.Compact(&compact, raw)
	return compact.Bytes()
}

// jsonValue reports whether raw is valid JSON other than null.
func jsonValue(raw json.RawMessage) bool {
	return json.Valid(raw) && !jsonNull(raw)
}

func jsonNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
