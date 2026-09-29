package gemini

import (
	"bytes"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestContextIdentityStaysOutsideGeminiBody(t *testing.T) {
	client, err := New(Config{APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(false)
	plain, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Messages[0].Context = &llm.MessageContext{Origin: "runtime", Name: "private-slot-name"}
	identified, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Body(), identified.Body()) {
		t.Fatalf("metadata changed Gemini wire body: %s", identified.Body())
	}
	if identified.Input().Messages[0].Context == nil || identified.Input().Messages[0].Context.Name != "private-slot-name" {
		t.Fatal("request lost metadata")
	}
}
