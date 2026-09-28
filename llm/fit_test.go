package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestNearestEffortPrefersLowerLevels(t *testing.T) {
	messages := []string{"low", "medium", "high", "max"}
	responses := []string{
		"none",
		"minimal",
		"low",
		"medium",
		"high",
		"xhigh",
	}
	for _, test := range []struct {
		name      string
		requested string
		accepted  []string
		want      string
		ok        bool
	}{
		{
			name:      "xhigh on Messages",
			requested: "xhigh",
			accepted:  messages,
			want:      "high",
			ok:        true,
		},
		{
			name:      "max on Responses",
			requested: "max",
			accepted:  responses,
			want:      "xhigh",
			ok:        true,
		},
		{
			name:      "minimal raised to lowest",
			requested: "minimal",
			accepted:  messages,
			want:      "low",
			ok:        true,
		},
		{
			name:      "none is never raised",
			requested: "none",
			accepted:  messages,
		},
		{
			name:      "unranked name",
			requested: "ultra",
			accepted:  messages,
		},
		{
			name:      "minimal never lowered to none",
			requested: "minimal",
			accepted:  []string{"none", "low", "medium", "high"},
			want:      "low",
			ok:        true,
		},
		{
			name:      "low with only none",
			requested: "low",
			accepted:  []string{"none"},
		},
		{
			name:      "xhigh lowered to high",
			requested: "xhigh",
			accepted:  []string{"low", "medium", "high"},
			want:      "high",
			ok:        true,
		},
		{
			name:      "medium raised past none",
			requested: "medium",
			accepted:  []string{"none", "high"},
			want:      "high",
			ok:        true,
		},
		{
			name:      "extra-high alias",
			requested: "extra-high",
			accepted:  responses,
			want:      "xhigh",
			ok:        true,
		},
		{
			name:      "extra_high alias",
			requested: "extra_high",
			accepted:  messages,
			want:      "high",
			ok:        true,
		},
		{
			name:      "extra high alias",
			requested: "extra high",
			accepted:  responses,
			want:      "xhigh",
			ok:        true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := nearestEffort(
				test.requested,
				test.accepted,
			)
			if got != test.want || ok != test.ok {
				t.Fatalf(
					"nearestEffort = %q, %v; want %q, %v",
					got,
					ok,
					test.want,
					test.ok,
				)
			}
		})
	}
}

func TestFitRanksEffortAliasesUnderBothPolicies(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted []string
		want     string
	}{
		{
			name:     "xhigh accepted",
			accepted: []string{"low", "medium", "high", "xhigh"},
			want:     "xhigh",
		},
		{
			name:     "xhigh not accepted",
			accepted: []string{"low", "medium", "high"},
			want:     "high",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Capabilities{
				ReasoningEfforts: test.accepted,
			}
			r := Input{
				Model: "m",
				Settings: Settings{
					Reasoning: &ReasoningSettings{
						Effort: "extra-high",
					},
				},
			}
			if _, _, err := c.Fit(
				r,
				CapabilityStrict,
				nil,
			); err == nil {
				t.Fatal("strict policy sent an alias it had to adjust")
			}
			fitted, adjustments, err := c.Fit(
				r,
				CapabilityAdjust,
				nil,
			)
			if err != nil {
				t.Fatalf(
					"adjust policy rejected a ranked alias: %v",
					err,
				)
			}
			if got := fitted.Settings.Reasoning.Effort; got != test.want {
				t.Fatalf(
					"effort = %q, want %q",
					got,
					test.want,
				)
			}
			warnings := Warnings(adjustments)
			if len(warnings) != 1 ||
				warnings[0].Code != WarningSettingClamped {
				t.Fatalf(
					"alias adjustment warnings = %+v",
					warnings,
				)
			}
		})
	}
}

func TestFitClampsUnderAdjustAndFailsUnderStrict(t *testing.T) {
	c := Capabilities{
		MaxOutputTokens:  4096,
		ReasoningEfforts: []string{"low", "high"},
		MaxTemperature:   1,
	}
	r := Input{
		Model: "m",
		Settings: Settings{
			MaxTokens:   8192,
			Temperature: new(1.5),
			Reasoning:   &ReasoningSettings{Effort: "medium"},
		},
	}
	if _, _, err := c.Fit(r, CapabilityStrict, nil); err == nil {
		t.Fatal("strict policy accepted values beyond known limits")
	}
	fitted, adjustments, err := c.Fit(r, CapabilityAdjust, nil)
	if err != nil {
		t.Fatalf("adjust policy rejected fittable values: %v", err)
	}
	if fitted.Settings.MaxTokens != 4096 {
		t.Fatalf(
			"max tokens = %d, want 4096",
			fitted.Settings.MaxTokens,
		)
	}
	if *fitted.Settings.Temperature != 1 {
		t.Fatalf(
			"temperature = %g, want 1",
			*fitted.Settings.Temperature,
		)
	}
	if fitted.Settings.Reasoning.Effort != "low" {
		t.Fatalf(
			"effort = %q, want low",
			fitted.Settings.Reasoning.Effort,
		)
	}
	if r.Settings.Reasoning.Effort != "medium" ||
		*r.Settings.Temperature != 1.5 {
		t.Fatal("fitting changed the caller's settings")
	}
	if len(adjustments) != 3 || len(Warnings(adjustments)) != 3 {
		t.Fatalf("adjustments = %+v", adjustments)
	}
	if err := validateAdjustments(
		r.Settings,
		fitted.Settings,
		adjustments,
	); err != nil {
		t.Fatalf("fitted adjustments disagree with settings: %v", err)
	}
}

func TestFitKeepsFittedDefaultSilent(t *testing.T) {
	c := Capabilities{MaxOutputTokens: 2048}
	requested := Input{Model: "m"}
	effective := requested
	effective.Settings.MaxTokens = 4096
	prior := []Adjustment{
		{
			Field:     SettingMaxTokens,
			Action:    AdjustmentDefault,
			Requested: json.RawMessage("null"),
			Effective: json.RawMessage("4096"),
			Reason:    "default",
		},
	}
	fitted, adjustments, err := c.Fit(effective, CapabilityAdjust, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjustments) != 1 ||
		adjustments[0].Action != AdjustmentDefault {
		t.Fatalf("fitted default recorded as %+v", adjustments)
	}
	if string(adjustments[0].Effective) != "2048" ||
		string(prior[0].Effective) != "4096" {
		t.Fatal("default was not refitted without changing prior")
	}
	if len(Warnings(adjustments)) != 0 {
		t.Fatal("a default the caller never set produced a warning")
	}
	if err := validateAdjustments(
		requested.Settings,
		fitted.Settings,
		adjustments,
	); err != nil {
		t.Fatalf("fitted default disagrees with settings: %v", err)
	}
}

func TestFitDefaultsUnsetMaxTokens(t *testing.T) {
	for _, test := range []struct {
		name    string
		support Support
		limit   int
		policy  CapabilityPolicy
		want    int
	}{
		{
			name:   "unknown support under adjust",
			policy: CapabilityAdjust,
			want:   DefaultMaxTokens,
		},
		{
			name:   "lowered to a smaller model limit",
			limit:  8192,
			policy: CapabilityAdjust,
			want:   8192,
		},
		{
			name:   "larger model limit keeps the default",
			limit:  128_000,
			policy: CapabilityAdjust,
			want:   DefaultMaxTokens,
		},
		{
			name:    "known support under strict",
			support: SupportSupported,
			limit:   8192,
			policy:  CapabilityStrict,
			want:    8192,
		},
		{
			name:   "unknown support under strict",
			policy: CapabilityStrict,
		},
		{
			name:    "known rejection",
			support: SupportUnsupported,
			policy:  CapabilityAdjust,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Capabilities{
				Features:        map[Feature]Support{},
				MaxOutputTokens: test.limit,
			}
			if test.support != SupportUnknown {
				c.Features[FeatureMaxTokens] = test.support
			}
			r := Input{Model: "m"}
			fitted, adjustments, err := c.Fit(r, test.policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := fitted.Settings.MaxTokens; got != test.want {
				t.Fatalf("max tokens = %d, want %d", got, test.want)
			}
			if test.want == 0 {
				if len(adjustments) != 0 {
					t.Fatalf("skipped default was recorded: %+v", adjustments)
				}
				return
			}
			if len(adjustments) != 1 ||
				adjustments[0].Action != AdjustmentDefault {
				t.Fatalf("default recorded as %+v", adjustments)
			}
			if len(Warnings(adjustments)) != 0 {
				t.Fatal("a default the caller never set produced a warning")
			}
			if err := validateAdjustments(
				r.Settings,
				fitted.Settings,
				adjustments,
			); err != nil {
				t.Fatalf("default disagrees with settings: %v", err)
			}
		})
	}
}

func TestFitKeepsExplicitMaxTokens(t *testing.T) {
	c := Capabilities{MaxOutputTokens: 128_000}
	r := Input{Model: "m", Settings: Settings{MaxTokens: 100}}
	fitted, adjustments, err := c.Fit(r, CapabilityAdjust, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fitted.Settings.MaxTokens != 100 || len(adjustments) != 0 {
		t.Fatal("an explicit max tokens was replaced by the default")
	}
}

func TestFitOmitsOnlyParallelRequests(t *testing.T) {
	c := Capabilities{
		Features: map[Feature]Support{
			FeatureParallelTools: SupportUnsupported,
		},
	}
	enabled, disabled := true, false
	r := Input{Model: "m", Settings: Settings{ParallelTools: &enabled}}
	fitted, adjustments, err := c.Fit(r, CapabilityAdjust, nil)
	if err != nil {
		t.Fatalf("parallel request was not omitted: %v", err)
	}
	if fitted.Settings.ParallelTools != nil {
		t.Fatal("omitted setting was kept")
	}
	warnings := Warnings(adjustments)
	if len(warnings) != 1 || warnings[0].Code != WarningSettingOmitted {
		t.Fatalf("omission warnings = %+v", warnings)
	}
	r.Settings.ParallelTools = &disabled
	_, _, err = c.Fit(r, CapabilityAdjust, nil)
	if err == nil || !strings.Contains(
		err.Error(),
		"remove ParallelTools",
	) {
		t.Fatalf(
			"disabling parallel calls was dropped or unexplained: %v",
			err,
		)
	}
}

func TestCheckUnsupportedFeatureNamesFix(t *testing.T) {
	c := Capabilities{
		Features: map[Feature]Support{
			FeatureText:  SupportSupported,
			FeatureTools: SupportUnsupported,
		},
	}
	r := Input{
		Model: "m",
		Messages: []Message{
			AssistantMessage(Content{
				Kind: ContentToolCall,
				ToolCall: &tool.Call{
					ID:   "call",
					Name: "act",
					Args: json.RawMessage(`{}`),
				},
			}),
		},
	}
	for _, policy := range []CapabilityPolicy{
		CapabilityAdjust,
		CapabilityStrict,
	} {
		_, err := c.Check(r, policy)
		if err == nil || !strings.Contains(
			err.Error(),
			"use a model with tool support",
		) {
			t.Fatalf(
				"policy %q: known missing tool support lacked a fix: %v",
				policy,
				err,
			)
		}
	}
}

func TestIntersectKeepsTighterLimits(t *testing.T) {
	model := Capabilities{
		Features: map[Feature]Support{
			FeatureTemperature: SupportSupported,
		},
		MaxOutputTokens:  8192,
		ReasoningEfforts: []string{"low", "high"},
	}
	api := Capabilities{
		Features: map[Feature]Support{
			FeatureParallelTools: SupportUnsupported,
		},
		ReasoningEfforts:   []string{"high", "max"},
		MinReasoningBudget: 1024,
		MaxTemperature:     1,
	}
	got := model.Intersect(api)
	if got.MaxOutputTokens != 8192 ||
		got.MinReasoningBudget != 1024 ||
		got.MaxTemperature != 1 {
		t.Fatalf("limits were not combined: %+v", got)
	}
	if len(got.ReasoningEfforts) != 1 ||
		got.ReasoningEfforts[0] != "high" {
		t.Fatalf(
			"effort levels = %v, want [high]",
			got.ReasoningEfforts,
		)
	}
	if got.Features[FeatureParallelTools] != SupportUnsupported {
		t.Fatal("API rejection was lost")
	}
	if got.Features[FeatureTemperature] != SupportUnknown {
		t.Fatal("support claimed by only one side was promoted")
	}
}

func TestIntersectKeepsTighterContextWindow(t *testing.T) {
	model := Capabilities{ContextWindow: 200_000}
	cases := []struct {
		name string
		api  Capabilities
		want int
	}{
		{
			name: "unknown API window",
			api:  Capabilities{},
			want: 200_000,
		},
		{
			name: "tighter API window",
			api:  Capabilities{ContextWindow: 128_000},
			want: 128_000,
		},
		{
			name: "looser API window",
			api:  Capabilities{ContextWindow: 1_000_000},
			want: 200_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := model.Intersect(tc.api).ContextWindow
			if got != tc.want {
				t.Fatalf(
					"context window = %d, want %d",
					got,
					tc.want,
				)
			}
		})
	}
}
