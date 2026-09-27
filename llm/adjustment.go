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
	// SettingToolChoice identifies the tool selection constraint.
	SettingToolChoice Setting = "tool_choice"
	// SettingParallelTools identifies the parallel tool call setting.
	SettingParallelTools Setting = "parallel_tools"
)

// AdjustmentAction names an authorized preparation behavior. Clamping,
// emulation, and omission have no policy or action in the current contract.
type AdjustmentAction string

const (
	// AdjustmentDefault fills an omitted setting without replacing user intent.
	AdjustmentDefault AdjustmentAction = "default"
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

	// Effective records the value encoded in the prepared body.
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

// Validate requires a supported action and values in canonical Settings form.
// NewRequest also checks that each decision agrees with the actual snapshots.
func (a Adjustment) Validate() error {
	switch a.Field {
	case SettingMaxTokens, SettingTemperature, SettingReasoning, SettingToolChoice, SettingParallelTools:
	default:
		return fmt.Errorf("llm: invalid adjustment field %q", a.Field)
	}
	if a.Action != AdjustmentDefault || !bytes.Equal(bytes.TrimSpace(a.Requested), []byte("null")) ||
		!json.Valid(a.Effective) || bytes.Equal(bytes.TrimSpace(a.Effective), []byte("null")) || a.Reason == "" {
		return errors.New("llm: adjustment requires a default action, omitted request, effective value, and reason")
	}
	return nil
}

func validateAdjustments(requested, effective Settings, adjustments []Adjustment) error {
	values := func(s Settings) map[Setting]json.RawMessage {
		raw, _ := json.Marshal(s)
		var fields map[Setting]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		return fields
	}
	before, after := values(requested), values(effective)
	seen := make(map[Setting]bool, len(adjustments))
	for _, a := range adjustments {
		if err := a.Validate(); err != nil {
			return err
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, a.Effective)
		if seen[a.Field] || len(before[a.Field]) != 0 || !bytes.Equal(compact.Bytes(), after[a.Field]) {
			return fmt.Errorf("llm: adjustment for %s disagrees with settings", a.Field)
		}
		seen[a.Field] = true
	}
	for _, fields := range []map[Setting]json.RawMessage{before, after} {
		for field := range fields {
			if !bytes.Equal(before[field], after[field]) && !seen[field] {
				return fmt.Errorf("llm: setting %s changed without an adjustment", field)
			}
		}
	}
	return nil
}
