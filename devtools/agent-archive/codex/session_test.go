package codex

import (
	"io"
	"os"
	"testing"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
)

func TestNoTokensAndUnknownTypes(t *testing.T) {
	c := fixture(t, 1, "", "no_tokens.jsonl")
	p := parse(t, c)[1]
	check(t, "session", []any{p.Session.UsageSource, len(p.Usage), p.Session.UserPrompts, p.Session.GitBranch},
		[]any{"none", 0, 1, "dev"})
	check(t, "unknown", len(sessionfile.UnknownTypes(p.SkippedLines)), 2)
	tr := convert(t, "", c)
	check(t, "skipped", tr.SkippedLines, map[string]int{
		"ignored:session_meta": 1, "ignored:event_msg": 1, "unknown:realtime_item": 1, "unknown:ghost_snapshot": 1,
	})
	check(t, "last", shape(tr)[3], "assistant text:It parses files. @6")
}

func TestSessionInTwoFiles(t *testing.T) {
	part1 := fixture(t, 1, "", "two_files/part1.jsonl")
	part2 := fixture(t, 2, "", "two_files/part2.jsonl")
	tr := convert(t, "", part2, part1) // converted in time order
	check(t, "messages", shape(tr), []string{
		"user text:First part. @2",
		"assistant text:Reply one. @3 usage",
		"user text:Second part. @2",
		"assistant text:Reply two. @3 usage",
	})
	check(t, "copies", []int64{tr.LineRefs[0].CopyID, tr.LineRefs[2].CopyID}, []int64{1, 2})
	ps := parse(t, part1, part2)
	check(t, "usage", []int{len(ps[1].Usage), len(ps[2].Usage)}, []int{1, 1})
}

func forkSession(t *testing.T) []sessionfile.Copy {
	return []sessionfile.Copy{
		fixture(t, 1, "", "fork/root.jsonl"),
		fixture(t, 2, "r-fork", "fork/helper.jsonl"),
		fixture(t, 3, "r-nested", "fork/nested.jsonl"),
		fixture(t, 4, "r-guard", "fork/reviewer.jsonl"),
		fixture(t, 5, "r-eq", "fork/fork_no_prefix.jsonl"),
	}
}

func TestAgentTree(t *testing.T) {
	ps := parse(t, forkSession(t)...)
	type facts struct{ kind, parent, call string }
	got := map[int64]facts{}
	for id, p := range ps {
		got[id] = facts{p.Session.AgentKind, p.Session.ParentAgentID, p.Session.ParentToolCallID}
	}
	check(t, "agents", got, map[int64]facts{
		1: {"root", "", ""},
		// Linked by task_name, equal to the child's agent_path.
		2: {"fork", "", "call_sp"},
		// Linked by agent_id (older output); its parent is a subagent.
		3: {"subagent", "r-fork", "call_nest"},
		4: {"approval_reviewer", "", ""},
		5: {"fork", "", ""},
	})
	// The fork's prefix holds no token line, and its web search is not
	// counted.
	helper := ps[2]
	check(t, "helper usage", len(helper.Usage), 2)
	check(t, "helper searches", helper.Usage[0].Usage.Requests == nil, true)
	check(t, "helper model", helper.Usage[0].Model, "gpt-5.5-mini")
	check(t, "helper prompts", helper.Session.UserPrompts, 0)
	// A start ordinal that is not below the line count marks no prefix.
	check(t, "eq prompts", ps[5].Session.UserPrompts, 1)
}

func TestForkPrefixInherited(t *testing.T) {
	tr := convert(t, "r-fork", forkSession(t)...)
	check(t, "messages", shape(tr), []string{
		"user text:Spread the work. @4 inherited",
		"assistant provider:ws_parent @5 inherited",
		"user reminder:agent-message</root @7",
		`assistant reasoning:Spawn a nested helper. call:spawn_agent({"message":"Nested."}) @9,10 usage`,
		"user result:call_nest @12",
		"assistant text:Helper done. @13 usage",
	})
	check(t, "reminder", tr.Messages[2].Content[0].Reminder.Content, "Placeholder task.")
	check(t, "changed", tr.ChangedBlocks, map[string]int{"dropped:encrypted_content": 2})

	// Nothing is inherited when the start ordinal is not a prefix.
	tr = convert(t, "r-eq", forkSession(t)...)
	check(t, "eq", shape(tr), []string{"user text:Other task. @2", "assistant text:Other done. @3 usage"})
	if _, err := Convert(forkSession(t), "missing"); err == nil {
		t.Fatal("converting a missing agent should fail")
	}
}

func TestTokenLineAfterToolOutput(t *testing.T) {
	// Codex can write a reply's token line after the call's output.
	tr := convert(t, "", fixture(t, 1, "", "late_token.jsonl"))
	check(t, "messages", shape(tr), []string{
		"user text:Check the build. @3",
		`assistant call:exec("make build") @4 usage`,
		"user result:call_b @5",
		"assistant text:It builds. @7 usage",
	})
	check(t, "usage", []int{tr.LineRefs[1].Usage.InputTokens, tr.LineRefs[3].Usage.InputTokens}, []int{30, 10})
	check(t, "model", tr.Messages[1].Source.Served.Model, "gpt-5.6")
}

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		ok   bool
	}{
		{`"Chunk ID: 1\nProcess exited with code 2\nOutput:"`, 2, true},
		{`"Exit code: 0\nWall time: 0 seconds"`, 0, true},
		{`"{\"output\":\"x\",\"metadata\":{\"exit_code\":127}}"`, 127, true},
		{`[{"type":"input_text","text":"Exit code: 1\n"}]`, 1, true},
		{`"Plan updated"`, 0, false},
		{`"Output mentions Exit code: 5 mid-line"`, 0, false},
	} {
		code, ok := exitCode([]byte(tc.out))
		if code != tc.code || ok != tc.ok {
			t.Errorf("exitCode(%s) = %d, %v; want %d, %v", tc.out, code, ok, tc.code, tc.ok)
		}
	}
}

func TestUnreadableCopy(t *testing.T) {
	c := sessionfile.Copy{ID: 1, Open: func() (io.ReadCloser, error) { return nil, os.ErrNotExist }}
	if _, err := Parse([]sessionfile.Copy{c}); err == nil {
		t.Fatal("want an error")
	}
}
