package llm

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

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
