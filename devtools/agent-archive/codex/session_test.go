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
		"ignored:session_meta": 1, "ignored:event_msg": 1, "unknown:future_item": 1, "unknown:ghost_snapshot": 1,
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

func TestToolErrors(t *testing.T) {
	const script = `{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"}`
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{`"Chunk ID: 1\nProcess exited with code 2\nOutput:"`, true},
		{`"Exit code: 0\nWall time: 0 seconds"`, false},
		{`"{\"output\":\"x\",\"metadata\":{\"exit_code\":127}}"`, true},
		{`[{"type":"input_text","text":"Exit code: 1\n"}]`, true},
		{`"Plan updated"`, false},
		{`"Output mentions Exit code: 5 mid-line"`, false},
		{`"exec_command failed: CreateProcess { message: \"Rejected\" }"`, true},
		{`[` + script + `,{"type":"input_text","text":"{\"chunk_id\":\"a\",\"exit_code\":0,\"output\":\"\"}"}]`, false},
		{`[` + script + `,{"type":"input_text","text":"{\"chunk_id\":\"a\",\"exit_code\":1,\"output\":\"\"}"}]`, true},
		{`[` + script + `,{"type":"input_text","text":"RESULT 1\n{\"exit_code\":0}\nRESULT 2\n{\"exit_code\": 2}"}]`, true},
		{`[` + script + `,{"type":"input_text","text":"Warning: truncated output\n\n{\"i\":0,\"status\":\"fulfilled\",\"value\":{\"exit_code\":1,\"output\":\"pa"}]`, true},
		{`[` + script + `,{"type":"input_text","text":"{\"exit_code\":0,\"output\":\"{\\\"exit_code\\\":4}\"}"}]`, false},
		{`[` + script + `,{"type":"input_text","text":"Exit code: 1"}]`, false}, // printed by the script
		{`[{"type":"input_text","text":"Script failed\nWall time 0.0 seconds\nOutput:\n"},{"type":"input_text","text":"Script error:\nboom"}]`, true},
	} {
		if got := isToolError([]byte(tc.out)); got != tc.want {
			t.Errorf("isToolError(%s) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

func TestCodeMode(t *testing.T) {
	// Code mode outputs hold exit codes as JSON; the first token line comes
	// before any line names the model; realtime_item lines are ignored.
	c := fixture(t, 1, "", "code_mode.jsonl")
	p := parse(t, c)[1]
	var lines []int
	var models []string
	for _, u := range p.Usage {
		lines, models = append(lines, u.Line), append(models, u.Model)
	}
	check(t, "usage", []any{lines, models}, []any{[]int{4, 13, 17}, []string{"gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol"}})
	check(t, "session", []any{p.Session.ToolCalls, p.Session.ToolErrors}, []any{3, 2})
	check(t, "unknown", len(sessionfile.UnknownTypes(p.SkippedLines)), 0)
	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user text:Is the build green? @2",
		"assistant text:Checking. @3 usage",
		`assistant call:exec("text(await exec_command({cmd: 'cat status.json'}))") @7`,
		"user result:call_1 @8",
		`assistant call:exec("const r = await Promise.allSettled([exec_command({cmd: 'make build'}), exec_command({cmd: 'make test'})])") @9`,
		"user result:call_2! @10",
		`assistant call:exec("text((await exec_command({cmd: 'make test'})") @11 usage`,
		"user result:call_3! @12",
		"assistant text:The tests fail. @16 usage",
	})
	check(t, "model", tr.Messages[1].Source.Served.Model, "gpt-5.6-sol")
}

func TestUnreadableCopy(t *testing.T) {
	c := sessionfile.Copy{ID: 1, Open: func() (io.ReadCloser, error) { return nil, os.ErrNotExist }}
	if _, err := Parse([]sessionfile.Copy{c}); err == nil {
		t.Fatal("want an error")
	}
}

// TestForkWithoutStartOrdinal checks a subagent fork whose start ordinal
// equals its own line count: the lines up to the turn after the last token
// line its parent also has are inherited, and its first own token line,
// which repeats the last inherited total, is not a model call.
func TestForkWithoutStartOrdinal(t *testing.T) {
	parent := fixture(t, 1, "", "replay/parent.jsonl")
	fork := fixture(t, 2, "p-fork", "replay/fork.jsonl")
	ps := parse(t, parent, fork)
	check(t, "parent usage", len(ps[1].Usage), 3)
	check(t, "fork usage", []int{len(ps[2].Usage), ps[2].Usage[0].Line}, []int{1, 15})
	s := ps[2].Session
	check(t, "fork session", []any{s.AgentKind, s.ToolCalls, s.UserPrompts}, []any{"fork", 0, 0})
	tr := convert(t, "p-fork", parent, fork)
	check(t, "messages", shape(tr), []string{
		"user text:Start the work. @4 inherited",
		"assistant text:Working. @5 inherited",
		`assistant call:spawn_agent({"fork_context":true}) @7 inherited`,
		"developer text:Placeholder fork instructions. @9 inherited",
		"assistant text:Fork done. @14 usage",
	})
}

// TestForkTotalEqualsLaterParentTotal checks that only the copied prefix
// is inherited: the fork's second own token line has the same total as a
// parent token line from after the fork, and the fork's lines are kept.
func TestForkTotalEqualsLaterParentTotal(t *testing.T) {
	parent := fixture(t, 1, "", "replay/parent.jsonl")
	fork := fixture(t, 2, "p-fork", "replay/fork_collide.jsonl")
	ps := parse(t, parent, fork)
	usage := ps[2].Usage
	check(t, "fork usage", len(usage), 2)
	check(t, "lines", []int{usage[0].Line, usage[1].Line}, []int{15, 17})
	tr := convert(t, "p-fork", parent, fork)
	check(t, "own messages", shape(tr)[4:], []string{
		"assistant text:Fork done. @14 usage",
		"assistant text:Fork checked. @16 usage",
	})
}

// TestForkedRoot checks a root forked from another session: that
// session's root, passed as a fork parent, shows what it repeats, and gets
// no parsed copy of its own.
func TestForkedRoot(t *testing.T) {
	parent := fixture(t, 1, "", "replay/parent.jsonl")
	parent.ForkParent = true
	root := fixture(t, 2, "", "replay/forked_root.jsonl")
	ps := parse(t, root, parent)
	check(t, "copies", len(ps), 1)
	s := ps[2].Session
	check(t, "session", []any{s.ForkedFromSessionID, s.ToolCalls, s.UserPrompts, len(ps[2].Usage)},
		[]any{"p-par", 0, 1, 1})
	check(t, "usage", []int{ps[2].Usage[0].Line, ps[2].Usage[0].Usage.OutputTokens}, []int{15, 7})
	tr := convert(t, "", root, parent)
	check(t, "messages", shape(tr), []string{
		"user text:Start the work. @3 inherited",
		"assistant text:Working. @5 inherited",
		`assistant call:spawn_agent({"fork_context":true}) @7 inherited`,
		"user result:call_fk @9 inherited",
		"user text:Do it differently. @12",
		"assistant text:Done differently. @14 usage",
	})
	// Without its parent, the whole file is the root's own.
	check(t, "alone", len(parse(t, root)[2].Usage), 3)
}
