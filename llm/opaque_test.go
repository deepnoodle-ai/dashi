package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
)

func TestOpaqueValidateRequiresRecognizedUnit(t *testing.T) {
	for _, test := range []struct {
		name  string
		unit  OpaqueUnit
		valid bool
	}{
		{name: "item", unit: OpaqueUnitItem, valid: true},
		{
			name:  "content block",
			unit:  OpaqueUnitContentBlock,
			valid: true,
		},
		{
			name:  "message part",
			unit:  OpaqueUnitMessagePart,
			valid: true,
		},
		{name: "missing"},
		{name: "unknown", unit: "future_unit"},
		{name: "old content scope", unit: "content"},
		{name: "old partial scope", unit: "partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := Opaque{
				Version: 1,
				Origin: content.Origin{
					Provider: "p",
					API:      "api",
					Model:    "m",
					Scope:    "scope",
				},
				Type: "future_provider_type",
				Unit: test.unit,
				Raw:  json.RawMessage(`{"private":1}`),
			}
			if err := o.Validate(); (err == nil) != test.valid {
				t.Fatalf(
					"unit validity = %t, want %t: %v",
					err == nil,
					test.valid,
					err,
				)
			}
		})
	}
}

func TestOpaqueRoundTripPreservesPartialUnitAndProviderMeaning(t *testing.T) {
	o := Opaque{
		Version: 1,
		Origin: content.Origin{
			Provider: "p",
			API:      "api",
			Model:    "m",
			Scope:    "scope",
		},
		Type:    "future_type",
		Unit:    OpaqueUnitItem,
		Partial: true,
		Raw:     json.RawMessage(`{"private":9007199254740993,"fragment":"{"}`),
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("partial evidence cannot be recorded: %v", err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var copy Opaque
	if err := json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	if copy.Unit != o.Unit ||
		!copy.Partial ||
		copy.Type != o.Type ||
		copy.Origin != o.Origin ||
		!bytes.Equal(
			copy.Raw,
			o.Raw,
		) {
		t.Fatal("round trip lost unit, completeness, origin, or provider meaning")
	}
	detached := o.Clone()
	detached.Raw[0] = '['
	if !json.Valid(o.Raw) {
		t.Fatal("opaque clone shared raw bytes")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, old := fields["scope"]; old ||
		string(fields["unit"]) != `"item"` ||
		string(fields["partial"]) != "true" {
		t.Fatal("opaque record used legacy or ambiguous field names")
	}
}

func TestInputRejectsPartialDataInEveryReplayAttachment(t *testing.T) {
	for _, test := range []struct {
		name    string
		content func(*Opaque) Content
	}{
		{
			name: "opaque",
			content: func(o *Opaque) Content {
				return Content{Kind: ContentOpaque, Opaque: o}
			},
		},
		{
			name: "known content attachment",
			content: func(o *Opaque) Content {
				c := Text("text")
				c.Replay = o
				return c
			},
		},
		{
			name: "reasoning",
			content: func(o *Opaque) Content {
				return Content{
					Kind:      ContentReasoning,
					Reasoning: &Reasoning{},
					Replay:    o,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := &Opaque{
				Version: 1,
				Origin: content.Origin{
					Provider: "p",
					API:      "api",
					Model:    "m",
					Scope:    "scope",
				},
				Type: "future_type",
				Unit: OpaqueUnitItem,
				Raw:  json.RawMessage(`{}`),
			}
			c := test.content(o)
			input := Input{
				Model: "m",
				Messages: []Message{
					{
						Role: RoleAssistant,
						Content: []Content{
							c,
						},
					},
				},
			}
			if err := input.Validate(); err != nil {
				t.Fatalf(
					"complete input rejected: %v",
					err,
				)
			}
			o.Partial = true
			if err := c.Validate(); err != nil {
				t.Fatalf(
					"partial result is not recordable: %v",
					err,
				)
			}
			if err := input.Validate(); err == nil {
				t.Fatal("partial data was admitted for replay")
			}
		})
	}
}

func TestOpaqueRejectsLegacyScopeWithoutMigration(t *testing.T) {
	legacy := []byte(`{"origin":{"provider":"p","api":"api","model":"m","scope":"scope"},"type":"future","scope":"item","raw":{}}`)
	var o Opaque
	if err := json.Unmarshal(legacy, &o); err != nil {
		t.Fatal(err)
	}
	if err := o.Validate(); err == nil {
		t.Fatal("legacy scope was silently interpreted as a replay unit")
	}
}

func TestOpaqueVersionsRemainInspectableButOnlyVersionOneCanReplay(
	t *testing.T,
) {
	for _, version := range []uint16{0, 1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			o := &Opaque{
				Version: version,
				Origin: content.Origin{
					Provider: "p",
					API:      "api",
					Model:    "m",
					Scope:    "scope",
				},
				Type: "future",
				Unit: OpaqueUnitItem,
				Raw:  json.RawMessage(`{"private":9007199254740993}`),
			}
			c := Content{Kind: ContentOpaque, Opaque: o}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			var restored Content
			if err := json.Unmarshal(
				raw,
				&restored,
			); err != nil {
				t.Fatal(err)
			}
			if err := restored.Validate(); err != nil ||
				restored.Opaque.Version != version {
				t.Fatal(
					"unrecognized version was not inspectable",
					err,
				)
			}
			input := Input{
				Model: "m",
				Messages: []Message{
					AssistantMessage(restored),
				},
			}
			if err := input.Validate(); (err == nil) != (version == 1) {
				t.Fatalf(
					"version %d replay eligibility: %v",
					version,
					err,
				)
			}
		})
	}
}

func TestContentRejectsLegacyReplayFields(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"block","block":{"kind":"text","text":{"text":"hello"}},"provider_data":{}}`,
		`{"kind":"reasoning","reasoning":{"text":"thought","replay":{}}}`,
	} {
		var c Content
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Fatal("legacy private data was silently discarded")
		}
	}
}

func TestContentReplayRoundTripUsesOneAttachment(t *testing.T) {
	o := &Opaque{
		Version: 1,
		Origin: content.Origin{
			Provider: "p",
			API:      "api",
			Model:    "m",
			Scope:    "scope",
		},
		Type: "reasoning",
		Unit: OpaqueUnitItem,
		Raw:  json.RawMessage(`{"signed":true}`),
	}
	original := Content{
		Kind: ContentReasoning,
		Reasoning: &Reasoning{
			Text: "display",
		},
		Replay: o,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored Content
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil ||
		restored.Replay == nil ||
		restored.Reasoning.Text != "display" {
		t.Fatal("unified reasoning replay was lost", err)
	}
	clone := restored.Clone()
	clone.Replay.Raw[0] = '['
	if !json.Valid(restored.Replay.Raw) {
		t.Fatal("content clone shared replay bytes")
	}
	if bytes.Count(raw, []byte(`"replay"`)) != 1 {
		t.Fatal("reasoning used multiple replay locations")
	}
	restored.Kind, restored.Opaque, restored.Reasoning = ContentOpaque, o, nil
	if err := restored.Validate(); err == nil {
		t.Fatal("unknown content accepted a second replay attachment")
	}
}
