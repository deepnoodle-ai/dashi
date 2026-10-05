package codex

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

// fixture returns a copy of a testdata file, with StartedAt from its first
// timestamp.
func fixture(t *testing.T, id int64, agentID, name string) sessionfile.Copy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	c := sessionfile.Copy{ID: id, Name: name, AgentID: agentID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
	var first struct {
		Timestamp string `json:"timestamp"`
	}
	_ = json.Unmarshal(b[:bytes.IndexByte(b, '\n')], &first)
	c.StartedAt = first.Timestamp
	return c
}

func parse(t *testing.T, copies ...sessionfile.Copy) map[int64]sessionfile.ParsedCopy {
	t.Helper()
	ps, err := Parse(copies)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]sessionfile.ParsedCopy{}
	for _, p := range ps {
		out[p.CopyID] = p
	}
	return out
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

// shape renders a transcript as one short line per message.
func shape(tr sessionfile.Transcript) []string {
	var out []string
	for i, m := range tr.Messages {
		ref := tr.LineRefs[i]
		var parts []string
		for _, c := range m.Content {
			switch c.Kind {
			case llm.ContentBlock:
				if c.Block.Text != nil {
					parts = append(parts, "text:"+c.Block.Text.Text)
				} else {
					parts = append(parts, string(c.Block.Kind))
				}
			case llm.ContentToolCall:
				parts = append(parts, "call:"+c.ToolCall.Name+"("+string(c.ToolCall.Args)+")")
			case llm.ContentToolResult:
				s := "result:" + c.ToolResult.CallID
				if c.ToolResult.IsError {
					s += "!"
				}
				parts = append(parts, s)
			case llm.ContentReasoning:
				parts = append(parts, "reasoning:"+c.Reasoning.Text)
			case llm.ContentReminder:
				parts = append(parts, "reminder:"+c.Reminder.Name+"<"+c.Reminder.From)
			case llm.ContentProviderToolUse:
				parts = append(parts, "provider:"+c.ProviderToolUse.ID)
			default:
				parts = append(parts, string(c.Kind))
			}
		}
		var lines []string
		for _, n := range ref.Lines {
			lines = append(lines, strconv.Itoa(n))
		}
		s := string(m.Role) + " " + strings.Join(parts, " ") + " @" + strings.Join(lines, ",")
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

func check[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

func TestTokenUsageRecordPreferred(t *testing.T) {
	c := fixture(t, 1, "", "token_both.jsonl")
	p := parse(t, c)[1]
	check(t, "usage", p.Usage, []sessionfile.Usage{
		{Line: 10, ModelCallID: "resp_1", Model: "gpt-5.5", CalledAt: "2026-08-01T10:00:01.300Z", Usage: llm.Usage{
			InputTokens: 40, CacheReadInputTokens: 60, OutputTokens: 20, ReasoningTokens: 5,
			CacheCreationInputTokensUnavailable: true,
		}},
		{Line: 15, ModelCallID: "resp_2", Model: "gpt-5.5", CalledAt: "2026-08-01T10:00:03.300Z", Usage: llm.Usage{
			InputTokens: 50, CacheReadInputTokens: 100, OutputTokens: 10,
			CacheCreationInputTokensUnavailable: true, Requests: map[string]int{"web_search": 1},
		}},
	})
	s := p.Session
	check(t, "session", []any{s.AgentKind, s.Entrypoint, s.GitBranch, s.GitRemoteURL, s.UserPrompts, s.ToolCalls,
		s.ToolErrors, s.Compactions, s.UsageSource, s.EndedAt},
		[]any{"root", "codex-tui", "main", "git@example.com:org/repo.git", 1, 1, 1, 2, "token_usage_record",
			"2026-08-01T11:00:01.000Z"})

	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"developer text:Placeholder developer instructions. @3",
		"user text:<environment_context>\n  <cwd>/work/demo</cwd>\n</environment_context> @4",
		"user text:Run the tests. @5",
		`assistant reasoning:Plan: run go test.

Then report. call:exec_command({"cmd":"go test ./..."}) @7,8 usage`,
		"user result:call_1! @11",
		"assistant provider:ws_line_13 text:One test fails. @12,13,14 usage",
		"user  @16 summary",
		"user text:Summary: one test fails. @18 summary",
	})
	check(t, "changed", tr.ChangedBlocks, map[string]int{
		"dropped:encrypted_content": 1, "dropped:reasoning": 1, "made_up_id": 1, "dropped:compaction": 1,
	})
	check(t, "source", tr.Messages[3].Source.ResponseID, "resp_1")
	check(t, "ref", []any{tr.LineRefs[3].Model, tr.LineRefs[3].Usage.InputTokens}, []any{"gpt-5.5", 40})
	check(t, "skipped", tr.SkippedLines, map[string]int{
		"ignored:session_meta": 1, "ignored:turn_context": 1, "ignored:event_msg": 2, "ignored:token_usage_record": 2,
	})
}

// A token line without a timestamp, or with one that does not parse,
// leaves CalledAt empty.
func TestCalledAtMissing(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "token_both.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `"timestamp":"2026-08-01T10:00:01.300Z",`, "", 1)
	s = strings.Replace(s, `"timestamp":"2026-08-01T10:00:03.300Z"`, `"timestamp":"not a time"`, 1)
	c := fixture(t, 1, "", "token_both.jsonl")
	c.Open = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
	var got []string
	for _, u := range parse(t, c)[1].Usage {
		got = append(got, u.CalledAt)
	}
	check(t, "called at", got, []string{"", ""})
}

func TestTokenCountOnly(t *testing.T) {
	c := fixture(t, 1, "", "token_count_only.jsonl")
	p := parse(t, c)[1]
	// The repeat (same total) and the one lacking info are skipped.
	check(t, "usage", p.Usage, []sessionfile.Usage{
		{Line: 6, Model: "gpt-5.1-codex", CalledAt: "2026-06-01T10:00:01.100Z", Usage: llm.Usage{InputTokens: 40, CacheReadInputTokens: 10, OutputTokens: 5,
			ReasoningTokens: 1, CacheCreationInputTokensUnavailable: true}},
		{Line: 11, Model: "gpt-5.1-codex", CalledAt: "2026-06-01T10:00:03.200Z", Usage: llm.Usage{InputTokens: 50, CacheReadInputTokens: 30, OutputTokens: 7,
			ReasoningTokens: 1, CacheCreationInputTokensUnavailable: true}},
	})
	s := p.Session
	// No UserMessage items: user messages minus injected context count.
	check(t, "session", []any{s.UsageSource, s.ToolCalls, s.ToolErrors, s.UserPrompts},
		[]any{"token_count", 3, 1, 1})

	tr := convert(t, "", c)
	check(t, "messages", shape(tr), []string{
		"user text:Patch the file. image @4",
		`assistant call:apply_patch("*** Begin Patch\n*** End Patch") @5 usage`,
		"user result:call_p @8",
		`assistant call:mcp__tracker__create_issue({"title":"x"}) call:tool_search({"query":"tracker","limit":5}) @9,10 usage`,
		"user result:call_m! @12",
		"user result:call_s @13",
		"user result:call_x @14",
	})
	out := tr.Messages[6].Content[0].ToolResult.Output.Content
	check(t, "list output", []string{string(out[0].Kind), string(out[1].Kind)}, []string{"text", "image"})
	if text := tr.Messages[5].Content[0].ToolResult.Output.Content[0].Text.Text; !strings.Contains(text, "create_issue") {
		t.Fatalf("tool search output %q", text)
	}
}
