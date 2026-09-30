package llm

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestReasoningSummaryPolicy(t *testing.T) {
	for _, support := range []Support{SupportSupported, SupportUnsupported, SupportUnknown} {
		for _, policy := range []CapabilityPolicy{CapabilityAdjust, CapabilityStrict} {
			for _, value := range []*bool{nil, new(false), new(true)} {
				caps := Capabilities{Features: map[Feature]Support{FeatureText: SupportSupported, FeatureMaxTokens: SupportSupported, FeatureReasoningSummary: support}}
				input := Input{Model: "fixture", Messages: []Message{UserMessage(Text("hello"))}, Settings: Settings{ReasoningSummary: value}}
				effective, adjustments, err := caps.Fit(input, policy, nil)
				on := value != nil && *value
				reject := on && support == SupportUnsupported && policy == CapabilityStrict
				if (err != nil) != reject {
					t.Fatalf("support=%s policy=%s value=%v Fit: %v", support, policy, value, err)
				}
				if reject {
					continue
				}
				unverified, err := caps.Check(effective, policy)
				reject = on && support == SupportUnknown && policy == CapabilityStrict
				if (err != nil) != reject {
					t.Fatalf("support=%s policy=%s value=%v Check: %v", support, policy, value, err)
				}
				if reject {
					continue
				}
				if slices.Contains(unverified, FeatureReasoningSummary) != (on && support == SupportUnknown) {
					t.Fatalf("unverified=%v", unverified)
				}
				omitted := on && support == SupportUnsupported
				if omitted {
					if effective.Settings.ReasoningSummary != nil {
						t.Fatal("unsupported opt-in retained")
					}
					var decision *Adjustment
					for i := range adjustments {
						if adjustments[i].Field == SettingReasoningSummary {
							decision = &adjustments[i]
						}
					}
					if decision == nil || decision.Action != AdjustmentOmit || string(decision.Requested) != "true" || string(decision.Effective) != "null" {
						t.Fatalf("adjustments=%+v", adjustments)
					}
					if _, warning := decision.Warning(); !warning {
						t.Fatal("omission lacks warning")
					}
				} else if effective.Settings.ReasoningSummary != value {
					t.Fatal("summary preference changed")
				}
				if value != nil && input.Settings.ReasoningSummary != value {
					t.Fatal("caller input mutated")
				}
			}
		}
	}
}

func TestSummaryPreferenceJSONPreservesFalse(t *testing.T) {
	for _, value := range []*bool{nil, new(false), new(true)} {
		raw, err := json.Marshal(Settings{ReasoningSummary: value})
		if err != nil {
			t.Fatal(err)
		}
		var restored Settings
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if (restored.ReasoningSummary == nil) != (value == nil) || value != nil && *restored.ReasoningSummary != *value {
			t.Fatalf("round trip %s", raw)
		}
	}
}
