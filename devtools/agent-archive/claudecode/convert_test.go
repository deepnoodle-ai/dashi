package claudecode

import "testing"

// TestConvertEveryCopyOfAnAgent checks that an agent with two copies is
// converted from both, in start order.
func TestConvertEveryCopyOfAnAgent(t *testing.T) {
	first := fixtureBytes(t, 1, "", "two/first.jsonl", []byte(`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s-two","timestamp":"2026-09-08T08:00:00.000Z","message":{"role":"user","content":"First."}}
`))
	second := fixtureBytes(t, 2, "", "two/second.jsonl", []byte(`{"type":"user","uuid":"u2","parentUuid":null,"sessionId":"s-two","timestamp":"2026-09-08T09:00:00.000Z","message":{"role":"user","content":"Second."}}
`))
	tr := convert(t, "", second, first)
	check(t, "messages", shape(tr), []string{"user text:First. @1", "user text:Second. @1"})
}

// TestTwoForkingCopiesAddTheParentOnce checks that the parent's context
// appears once when two copies of one agent both fork by reference.
func TestTwoForkingCopiesAddTheParentOnce(t *testing.T) {
	root := fixture(t, 1, "", "fork_ref/root.jsonl")
	first := fixture(t, 2, "ar", "fork_ref/agent-ar.jsonl")
	second := fixtureBytes(t, 3, "ar", "fork_ref/agent-ar-2.jsonl", []byte(`{"type":"fork-context-ref","agentId":"ar","parentSessionId":"s-ref","parentLastUuid":"a1","contextLength":2}
{"type":"user","uuid":"h1","parentUuid":"a1","isSidechain":true,"agentId":"ar","sessionId":"s-ref","timestamp":"2026-09-07T09:00:02.000Z","message":{"role":"user","content":"Again."}}
`))
	tr := convert(t, "ar", root, first, second)
	parent := 0
	for _, ref := range tr.LineRefs {
		if ref.CopyID == 1 {
			parent++
		}
	}
	// The fork context is the root's first two lines, u1 and a1.
	if parent != 2 {
		t.Fatalf("%d messages from the parent, want 2: %q", parent, shape(tr))
	}
}

// TestForkByReferenceParentWithoutStart checks that a fork by reference
// finds a parent that sorts after it.
func TestForkByReferenceParentWithoutStart(t *testing.T) {
	root := fixture(t, 1, "", "fork_ref/root.jsonl")
	root.StartedAt = ""
	fork := fixture(t, 2, "ar", "fork_ref/agent-ar.jsonl")
	tr := convert(t, "ar", root, fork)
	check(t, "copy of prefix", tr.LineRefs[0].CopyID, int64(1))
}
