package llm

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
)

func TestRequestSnapshotDecodesRecordedCapabilityPolicy(t *testing.T) {
	const prefix = `{"origin":{"provider":"p","api":"api","model":"m"},` +
		`"input":{"model":"m"},"effective":{},"body":"e30="`
	tests := []struct {
		name   string
		fields string
		want   CapabilityPolicy
		fails  bool
	}{
		{
			name:   "legacy record without a policy",
			fields: ``,
			want:   CapabilityStrict,
		},
		{
			name:   "legacy allow",
			fields: `,"unknown_capabilities":"allow"`,
			want:   CapabilityAdjust,
		},
		{
			name:   "legacy reject",
			fields: `,"unknown_capabilities":"reject"`,
			want:   CapabilityStrict,
		},
		{
			name:   "adjust",
			fields: `,"capability_policy":"adjust"`,
			want:   CapabilityAdjust,
		},
		{
			name:   "strict",
			fields: `,"capability_policy":"strict"`,
			want:   CapabilityStrict,
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
			var snapshot RequestSnapshot
			err := json.Unmarshal([]byte(prefix+test.fields+"}"), &snapshot)
			if test.fails {
				if err == nil {
					t.Fatal("decoded an unrecognized capability policy")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.CapabilityPolicy != test.want {
				t.Fatalf(
					"recorded policy decoded as %q, want %q",
					snapshot.CapabilityPolicy, test.want,
				)
			}
			if !bytes.Equal(snapshot.Body, []byte("{}")) {
				t.Fatal("policy decoding dropped the recorded body")
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(`"unknown_capabilities"`)) {
				t.Fatal("snapshot re-encoded the legacy policy field")
			}
			var again RequestSnapshot
			if err := json.Unmarshal(raw, &again); err != nil {
				t.Fatal(err)
			}
			if again.CapabilityPolicy != test.want {
				t.Fatalf("policy %q did not round trip", test.want)
			}
		})
	}
}

func TestNewRequestRecordsResolvedCapabilityPolicy(t *testing.T) {
	req, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "p",
			API:      "api",
			Model:    "m",
			Scope:    "scope",
		},
		Input: Input{Model: "m"},
		Body:  []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Snapshot().CapabilityPolicy != CapabilityAdjust {
		t.Fatal("zero policy was not recorded as adjust")
	}
	raw, err := json.Marshal(req.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"capability_policy":"adjust"`)) {
		t.Fatalf("snapshot omitted the resolved policy: %s", raw)
	}
}
