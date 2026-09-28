package kernel

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestModelFactDecodesRecordedCapabilityPolicy(t *testing.T) {
	const prefix = `{"attempt_id":"t1:model:1","model":"m"`
	tests := []struct {
		name   string
		fields string
		want   llm.CapabilityPolicy
		fails  bool
	}{
		{
			name:   "legacy fact without a policy",
			fields: ``,
			want:   llm.CapabilityStrict,
		},
		{
			name:   "legacy allow",
			fields: `,"unknown_capabilities":"allow"`,
			want:   llm.CapabilityAdjust,
		},
		{
			name:   "legacy reject",
			fields: `,"unknown_capabilities":"reject"`,
			want:   llm.CapabilityStrict,
		},
		{
			name:   "adjust",
			fields: `,"capability_policy":"adjust"`,
			want:   llm.CapabilityAdjust,
		},
		{
			name:   "strict",
			fields: `,"capability_policy":"strict"`,
			want:   llm.CapabilityStrict,
		},
		{
			name:   "unknown policy",
			fields: `,"capability_policy":"lenient"`,
			fails:  true,
		},
		{
			name:   "unknown legacy policy",
			fields: `,"unknown_capabilities":"maybe"`,
			fails:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var fact ModelFact
			err := json.Unmarshal([]byte(prefix+test.fields+"}"), &fact)
			if test.fails {
				if err == nil {
					t.Fatal("decoded an unrecognized capability policy")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fact.CapabilityPolicy != test.want {
				t.Fatalf(
					"recorded policy decoded as %q, want %q",
					fact.CapabilityPolicy, test.want,
				)
			}
			if fact.AttemptID != "t1:model:1" || fact.Model != "m" {
				t.Fatal("policy decoding dropped other fact fields")
			}
			raw, err := json.Marshal(fact)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(`"unknown_capabilities"`)) {
				t.Fatal("fact re-encoded the legacy policy field")
			}
			var again ModelFact
			if err := json.Unmarshal(raw, &again); err != nil {
				t.Fatal(err)
			}
			if again.CapabilityPolicy != test.want {
				t.Fatalf("policy %q did not round trip", test.want)
			}
		})
	}
}

func TestModelIntentRecordsResolvedCapabilityPolicy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var intent *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent {
			intent = step.Model
		}
	}
	if intent == nil {
		t.Fatal("turn recorded no model intent")
	}
	if intent.CapabilityPolicy != llm.CapabilityAdjust {
		t.Fatalf("intent recorded policy %q", intent.CapabilityPolicy)
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"capability_policy":"adjust"`)) {
		t.Fatalf("intent omitted the resolved policy: %s", raw)
	}
}
