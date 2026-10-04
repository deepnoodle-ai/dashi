package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
)

// fixture returns a copy of a testdata file. Copy ids follow the order
// given, and StartedAt is the file's first timestamp.
func fixture(t *testing.T, id int64, agentID, name string) sessionfile.Copy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return fixtureBytes(t, id, agentID, name, b)
}

func fixtureBytes(t *testing.T, id int64, agentID, name string, b []byte) sessionfile.Copy {
	t.Helper()
	c := sessionfile.Copy{ID: id, Name: name, AgentID: agentID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
	for _, l := range strings.Split(string(b), "\n") {
		var x struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(l), &x) == nil && x.Timestamp != "" {
			c.StartedAt = x.Timestamp
			break
		}
	}
	if meta, err := os.ReadFile(filepath.Join("testdata", strings.TrimSuffix(name, ".jsonl")+".meta.json")); err == nil {
		c.Meta = meta
	}
	return c
}

func parseOne(t *testing.T, copies ...sessionfile.Copy) []sessionfile.ParsedCopy {
	t.Helper()
	ps, err := Parse(copies)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func convert(t *testing.T, agentID string, copies ...sessionfile.Copy) sessionfile.Transcript {
	t.Helper()
	tr, err := Convert(copies, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) != len(tr.LineRefs) {
		t.Fatalf("%d messages, %d line refs", len(tr.Messages), len(tr.LineRefs))
	}
	for i, m := range tr.Messages {
		for _, c := range m.Content {
			if err := c.Validate(); err != nil {
				t.Fatalf("message %d: %v", i, err)
			}
		}
	}
	return tr
}

// shape renders a transcript as one short line per message, for
// comparing.
func shape(tr sessionfile.Transcript) []string {
	var out []string
	for i, m := range tr.Messages {
		ref := tr.LineRefs[i]
		var parts []string
		for _, c := range m.Content {
			switch c.Kind {
			case llm.ContentBlock:
				switch {
				case c.Block.Text != nil:
					parts = append(parts, "text:"+c.Block.Text.Text)
				default:
					parts = append(parts, string(c.Block.Kind))
				}
			case llm.ContentToolCall:
				parts = append(parts, "call:"+c.ToolCall.ID)
			case llm.ContentToolResult:
				s := "result:" + c.ToolResult.CallID
				if c.ToolResult.IsError {
					s += "!"
				}
				parts = append(parts, s)
			case llm.ContentReasoning:
				parts = append(parts, "reasoning:"+c.Reasoning.Text)
			case llm.ContentReminder:
				parts = append(parts, "reminder:"+c.Reminder.Name)
			default:
				parts = append(parts, string(c.Kind))
			}
		}
		s := string(m.Role) + " " + strings.Join(parts, " ") + " @" + joinInts(ref.Lines)
		if ref.Inherited {
			s += " inherited"
		}
		if ref.CompactionSummary {
			s += " summary"
		}
		if ref.Usage != nil {
			s += " usage"
		}
		out = append(out, s)
	}
	return out
}

func joinInts(ns []int) string {
	var b strings.Builder
	for i, n := range ns {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

func check[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

func TestSplitModelCall(t *testing.T) {
	c := fixture(t, 1, "", "split_call.jsonl")
	ps := parseOne(t, c)
	check(t, "usage", ps[0].Usage, []sessionfile.Usage{
		{Line: 3, ModelCallID: "msg_1", Model: "claude-opus-5-5", Speed: "standard", Usage: llm.Usage{
			InputTokens: 3, OutputTokens: 30, CacheCreationInputTokens: 100, CacheReadInputTokens: 200,
			CacheCreation1hInputTokens: 100, ReasoningTokens: 6, Requests: map[string]int{"web_search": 1},
		}},
		// The served model differs from message.model: usage goes to it.
		{Line: 7, ModelCallID: "msg_2", Model: "claude-opus-5-5-fast", Speed: "fast", Usage: llm.Usage{
			InputTokens: 5, OutputTokens: 8, CacheReadInputTokens: 300, ReasoningTokens: 3,
		}},
	})
	s := ps[0].Session
	check(t, "session", []any{s.AgentKind, s.Entrypoint, s.GitBranch, s.UserPrompts, s.ToolCalls, s.ToolErrors, s.EndedAt, s.UsageSource},
		[]any{"root", "cli", "main", 1, 1, 0, "2026-09-01T10:00:05.000Z", "message_usage"})

	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user text:List the files. @2",
		"assistant text:Listing the folder. call:toolu_1 @3,4,5 usage",
		"user result:toolu_1 @6",
		"assistant reasoning:Two files. text:There are two files. @7 usage",
	})
	check(t, "changed", tr.ChangedBlocks, map[string]int{"dropped:thinking": 1, "dropped:signature": 1})
	check(t, "skipped", tr.SkippedLines, map[string]int{"ignored:permission-mode": 1})
	src := tr.Messages[3].Source
	check(t, "source", []string{src.Requested.Model, src.Served.Model, src.ResponseID, src.API},
		[]string{"claude-opus-5-5", "claude-opus-5-5-fast", "msg_2", "anthropic.messages"})
	check(t, "ref model", tr.LineRefs[3].Model, "claude-opus-5-5-fast")
	check(t, "ref usage", tr.LineRefs[1].Usage.OutputTokens, 30)
}

func TestParallelToolResults(t *testing.T) {
	c := fixture(t, 1, "", "parallel_results.jsonl")
	tr := convert(t, "", c)
	// The walk up from the last line misses the first result; it answers
	// a call on the path, so it is kept.
	check(t, "messages", shape(tr), []string{
		"user text:Read both files. @1",
		"assistant call:toolu_a call:toolu_b @2,4 usage",
		"user result:toolu_a @3",
		"user result:toolu_b! @5",
		"assistant text:One file is missing. @6 usage",
	})
	check(t, "skipped", tr.SkippedLines, map[string]int{})
	s := parseOne(t, c)[0].Session
	check(t, "tool calls and errors", []int{s.ToolCalls, s.ToolErrors}, []int{2, 1})
}

func TestRewoundLines(t *testing.T) {
	c := fixture(t, 1, "", "rewound.jsonl")
	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user text:<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args> @1",
		"user text:<local-command-stdout>Set model to opus</local-command-stdout> @2",
		"user text:Write a haiku. @3",
		"assistant text:First haiku. @4 usage",
		"user text:Make it happier. @7",
		"assistant call:toolu_r @8 usage",
		"user result:toolu_r! @9",
		"user text:[Request interrupted by user for tool use] @10",
		"user text:<task-notification>done</task-notification> @11",
	})
	check(t, "skipped", tr.SkippedLines, map[string]int{"rewound": 2, "ignored:last-prompt": 1})
	p := parseOne(t, c)[0]
	// Rewound model calls were still billed.
	check(t, "usage rows", len(p.Usage), 3)
	// Command wrappers, command output, interrupts, notifications, and the
	// rewound prompt are not prompts.
	check(t, "user prompts", p.Session.UserPrompts, 2)
	check(t, "skipped lines", p.SkippedLines, map[string]int{"rewound": 2, "ignored:last-prompt": 1})
}

func TestCompaction(t *testing.T) {
	c := fixture(t, 1, "", "compaction.jsonl")
	tr := convert(t, "", c)
	// The full history, across the boundary by logicalParentUuid.
	check(t, "messages", shape(tr), []string{
		"user text:Start the long task. @1",
		"assistant text:Working on it. @2 usage",
		"user text:Summary: the task is half done. @4 summary",
		"assistant text:Continuing. @5 usage",
	})
	check(t, "skipped", tr.SkippedLines, map[string]int{"ignored:system": 1})
	check(t, "compactions", parseOne(t, c)[0].Session.Compactions, 1)
}

func TestErrorAndDuplicateLines(t *testing.T) {
	c := fixture(t, 1, "", "error_and_duplicate.jsonl")
	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user text:Try again. @1",
		"assistant text:Done. @4 usage",
	})
	check(t, "skipped", tr.SkippedLines, map[string]int{"duplicate_line": 1, "api_error": 1, "bad_line": 1})
	p := parseOne(t, c)[0]
	check(t, "usage", len(p.Usage), 1)
	check(t, "prompts", p.Session.UserPrompts, 1)
}

func TestRemindersAndMetadata(t *testing.T) {
	c := fixture(t, 1, "", "reminders.jsonl")
	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user reminder:hook-additional-context @1",
		"user reminder:claude-code-meta @3",
		"user text:What is in this picture? image @4",
		"user reminder:date @5",
		"assistant call:toolu_s @7 usage",
		"user result:toolu_s @8",
		"assistant text:A small image. @9 usage",
	})
	check(t, "date reminder", tr.Messages[3].Content[0].Reminder.Content, "Today is 2026-09-05.\nPlaceholder second part.")
	check(t, "tier", tr.Messages[0].Content[0].Reminder.Tier, llm.ReminderContextual)
	check(t, "result output", len(tr.Messages[5].Content[0].ToolResult.Output.Content), 1)
	check(t, "changed", tr.ChangedBlocks, map[string]int{"dropped:tool_reference": 1})
	check(t, "skipped", tr.SkippedLines, map[string]int{
		"ignored:attachment": 1, "unknown:brand-new-thing": 1, "ignored:custom-title": 2, "ignored:ai-title": 1,
		"ignored:pr-link": 1, "ignored:cost-state": 2, "ignored:agent-name": 1,
	})
	check(t, "unknown types", sessionfile.UnknownTypes(tr.SkippedLines), []string{"brand-new-thing"})
	img := tr.Messages[2].Content[1].Block.Image
	check(t, "image", []any{img.Source.MIMEType, len(img.Source.Data)}, []any{"image/png", 8})

	s := parseOne(t, c)[0].Session
	check(t, "session", []any{s.Title, s.GitRemoteURL, *s.AppCostUSD, s.Entrypoint, s.GitBranch, s.UserPrompts},
		[]any{"Second custom title", "https://github.com/example-org/example-repo", 0.5, "sdk-cli", "feature/x", 1})
}

func TestLargeReminderBecomesText(t *testing.T) {
	big := strings.Repeat("x", sessionfile.MaxReminderBytes+1)
	line := `{"type":"attachment","uuid":"t1","parentUuid":null,"timestamp":"2026-09-05T08:00:00.000Z",` +
		`"attachment":{"type":"skill_listing"},"rendered":[{"content":"` + big + `"}]}` + "\n" +
		`{"type":"attachment","uuid":"t2","parentUuid":"t1","timestamp":"2026-09-05T08:00:00.000Z",` +
		`"attachment":{"type":"9 Odd Name!"},"rendered":[{"content":"small"}]}` + "\n" +
		`{"type":"attachment","uuid":"t3","parentUuid":"t2","timestamp":"2026-09-05T08:00:00.000Z",` +
		`"attachment":{"type":"___"},"rendered":[{"content":""}]}` + "\n" +
		`{"type":"attachment","uuid":"t4","parentUuid":"t3","timestamp":"2026-09-05T08:00:00.000Z",` +
		`"attachment":{"type":"___"},"rendered":[{"content":"named"}]}` + "\n"
	tr := convert(t, "", fixtureBytes(t, 1, "", "big.jsonl", []byte(line)))
	check(t, "messages", shape(tr), []string{
		"user text:" + big + " @1",
		"user reminder:odd-name- @2",
		"user reminder:attachment @4",
	})
	check(t, "changed", tr.ChangedBlocks, map[string]int{"reminder_as_text": 1, "dropped:reminder": 1})
}

func TestForkRepeatsLines(t *testing.T) {
	root := fixture(t, 1, "", "fork/root.jsonl")
	fork := fixture(t, 2, "af", "fork/agent-af.jsonl")
	ps := parseOne(t, fork, root) // order does not matter
	check(t, "copies", []int64{ps[0].CopyID, ps[1].CopyID}, []int64{1, 2})
	// msg_f1 is in both files, under a different uuid for one line; it is
	// counted once, on the root, which started first.
	check(t, "root usage", []string{ps[0].Usage[0].ModelCallID, ps[0].Usage[1].ModelCallID}, []string{"msg_f1", "msg_f2"})
	check(t, "fork usage", len(ps[1].Usage), 1)
	check(t, "fork usage id", ps[1].Usage[0].ModelCallID, "msg_f3")
	s := ps[1].Session
	check(t, "fork session", []any{s.AgentKind, s.ParentAgentID, s.ParentToolCallID, s.Title, s.ToolCalls, s.UserPrompts},
		[]any{"fork", "", "toolu_f", "Placeholder fork", 1, 1})

	tr := convert(t, "af", root, fork)
	check(t, "fork messages", shape(tr), []string{
		"user text:Plan, then fork a helper. @1 inherited",
		"assistant text:Here is the plan. call:toolu_f @2,3 inherited",
		"user text:Placeholder task. @4",
		"assistant text:Helper finished. @5 usage",
	})
}

func TestForkByReference(t *testing.T) {
	root := fixture(t, 1, "", "fork_ref/root.jsonl")
	fork := fixture(t, 2, "ar", "fork_ref/agent-ar.jsonl")
	tr := convert(t, "ar", root, fork)
	// The parent's lines up to parentLastUuid come first, inherited; its
	// tool result was written after the fork, so it is left out.
	check(t, "messages", shape(tr), []string{
		"user text:Fork a helper by reference. @1 inherited",
		"assistant call:toolu_x @2 inherited",
		"user text:Placeholder. @2",
		"assistant text:Done by reference. @3 usage",
	})
	check(t, "copy of prefix", tr.LineRefs[0].CopyID, int64(1))
	check(t, "skipped", tr.SkippedLines, map[string]int{"ignored:fork-context-ref": 1})
	if _, err := Convert([]sessionfile.Copy{root}, "missing"); err == nil {
		t.Fatal("converting a missing agent should fail")
	}
}

func TestUnreadableCopy(t *testing.T) {
	c := sessionfile.Copy{ID: 1, Open: func() (io.ReadCloser, error) { return nil, os.ErrNotExist }}
	if _, err := Parse([]sessionfile.Copy{c}); err == nil {
		t.Fatal("want an error")
	}
}

// TestUsageOnLaterCopy checks that a model call whose line has no usage on
// an earlier copy keeps its usage on the later copy that has it, in Parse
// and in Convert alike.
func TestUsageOnLaterCopy(t *testing.T) {
	root := fixtureBytes(t, 1, "", "later/root.jsonl", []byte(`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s-later","timestamp":"2026-09-06T08:00:00.000Z","message":{"role":"user","content":"Start."}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s-later","timestamp":"2026-09-06T08:00:01.000Z","message":{"id":"msg_l1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Partial."}]}}
`))
	agent := fixtureBytes(t, 2, "al", "later/agent-al.jsonl", []byte(`{"type":"user","uuid":"lu1","parentUuid":null,"isSidechain":true,"agentId":"al","sessionId":"s-later","timestamp":"2026-09-06T08:00:02.000Z","message":{"role":"user","content":"Start."}}
{"type":"assistant","uuid":"la1","parentUuid":"lu1","isSidechain":true,"agentId":"al","sessionId":"s-later","timestamp":"2026-09-06T08:00:03.000Z","message":{"id":"msg_l1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Partial."}],"usage":{"input_tokens":2,"output_tokens":4}}}
`))
	ps := parseOne(t, root, agent)
	check(t, "root usage", len(ps[0].Usage), 0)
	check(t, "agent usage", len(ps[1].Usage), 1)

	tr := convert(t, "al", root, agent)
	check(t, "messages", shape(tr), []string{
		"user text:Start. @1",
		"assistant text:Partial. @2 usage",
	})
}
