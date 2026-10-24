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

// TestForkByReferenceParentWithoutStart checks that a fork by reference
// finds a parent that sorts after it.
func TestForkByReferenceParentWithoutStart(t *testing.T) {
	root := fixture(t, 1, "", "fork_ref/root.jsonl")
	root.StartedAt = ""
	fork := fixture(t, 2, "ar", "fork_ref/agent-ar.jsonl")
	tr := convert(t, "ar", root, fork)
	check(t, "copy of prefix", tr.LineRefs[0].CopyID, int64(1))
}
