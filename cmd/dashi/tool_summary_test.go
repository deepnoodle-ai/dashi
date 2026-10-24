package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	tl "github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/wonton/runewidth"
)

func TestToolSummary(t *testing.T) {
	for _, test := range []struct {
		name, tool, args, state, output, denied, want string
	}{
		{"answer", "ask_user", `{"question":"Favorite noodle?"}`, StateOK, "Ramen", "", "Ramen"},
		{"selections", "ask_user", `{}`, StateOK, `{"selections":["Ramen","Udon"]}`, "", "Ramen, Udon"},
		{"own words", "ask_user", `{}`, StateOK, `{"text":"Soba"}`, "", "Soba"},
		{"long answer", "ask_user", `{}`, StateOK, "first line\nsecond line", "", "first line …"},
		{"read", "read_file", `{"path":"a.go"}`, StateOK, "     1\tpackage main\n     2\t\n     3\tfunc main() {}\n", "", "3 lines"},
		{"read part", "read_file", `{}`, StateOK, "     1\ta\n     2\tb\n[Showed lines 1-2 of 90. Set offset to 3 to read more.]\n", "", "lines 1-2 of 90"},
		{"read empty", "read_file", `{}`, StateOK, "The file is empty.", "", "empty file"},
		{"list", "list_directory", `{}`, StateOK, "a.go\nb/\nc.go", "", "3 entries"},
		{"glob", "glob", `{}`, StateOK, "a.go\nb.go", "", "2 files"},
		{"glob capped", "glob", `{}`, StateOK, "a.go\nb.go\n[Showed 2 of 340 files. Use a narrower pattern or path.]", "", "340 files"},
		{"glob none", "glob", `{}`, StateOK, "No files match.", "", "no files"},
		{"grep files", "grep", `{"pattern":"x"}`, StateOK, "a.go\nb.go", "", "2 files"},
		{"grep content", "grep", `{"output_mode":"content"}`, StateOK, "a.go:3:x := 1\na.go-4-y\n--\nb.go:9:x++", "", "2 matches"},
		{"grep count", "grep", `{"output_mode":"count"}`, StateOK, "a.go:3\nb.go:1", "", "4 matches in 2 files"},
		{"grep capped", "grep", `{"output_mode":"content"}`, StateOK, "a.go:1:x\n[Showed the first 1 lines. More results exist. Narrow the search or raise head_limit.]", "", "1+ match"},
		{"grep none", "grep", `{}`, StateOK, "No matches.", "", "no matches"},
		{"write new", "write_file", `{"path":"a.go","content":"a\nb\n"}`, StateOK, "Created a.go (4 bytes).", "", "created · 2 lines"},
		{"overwrite", "write_file", `{"path":"a.go","content":"a"}`, StateOK, "Overwrote a.go (1 bytes).", "", "wrote 1 line"},
		{"edit", "edit_file", `{"old_string":"a\nb\nc","new_string":"a\nB\nB2\nc"}`, StateOK, "Edited x: replaced 1 occurrence(s).", "", "+2 -1"},
		{"edit all", "edit_file", `{"old_string":"x","new_string":"y","replace_all":true}`, StateOK, "Edited x: replaced 3 occurrence(s).", "", "+3 -3 · 3 replacements"},
		{"bash", "bash", `{"command":"go test ./..."}`, StateOK, "?   pkg [no test files]\nok  github.com/deepnoodle-ai/dashi  3.1s\n", "", "ok  github.com/deepnoodle-ai/dashi  3.1s"},
		{"bash quiet", "bash", `{}`, StateOK, "(no output)", "", "no output"},
		{"bash bracketed output", "bash", `{}`, StateOK, "building\n[done]", "", "[done]"},
		{"bash note", "bash", `{}`, StateOK, "done\n[The timeout was reduced to the maximum, 10m0s.]", "", "done"},
		{"bash exit", "bash", `{}`, StateFailed, "x.go:1: undefined: foo\n[Exit code 2]", "", "exit 2 · x.go:1: undefined: foo"},
		{"bash timeout", "bash", `{}`, StateFailed, "(no output)\n[The command ran past the timeout of 2s and was stopped. ...]", "", "timed out"},
		{"skill", "load_skill", `{"name":"pdf"}`, StateOK, "Base directory: /s/pdf\n\n# pdf\n\nUse it.", "", "loaded pdf"},
		{"skill file", "read_skill_file", `{}`, StateOK, "Skill pdf — notes.md\n\none\ntwo\n", "", "2 lines"},
		{"task", "task", `{}`, StateOK, "Started task t_1.", "", ""},
		{"mcp", "lookup", `{}`, StateOK, "\n  Found 3 results\nmore", "", "Found 3 results"},
		{"failed", "read_file", `{}`, StateFailed, "error one\nfile a.go does not exist.\n", "", "file a.go does not exist."},
		{"denied", "bash", `{}`, StateDenied, "", "decide: destructive command", "decide: destructive command"},
		{"waiting", "bash", `{}`, StateWaiting, "started", "", ""},
		{"escape", "lookup", `{}`, StateOK, "\x1b[2Jcleared", "", "␛[2Jcleared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := toolSummary(test.tool, json.RawMessage(test.args), test.state, test.output, test.denied)
			if got != test.want {
				t.Fatalf("summary=%q want %q", got, test.want)
			}
		})
	}
	long := toolSummary("lookup", nil, StateOK, strings.Repeat("word ", 60), "")
	if runewidth.StringWidth(long) != summaryWidth || !strings.HasSuffix(long, "…") {
		t.Fatalf("long summary=%q", long)
	}
}

// The renderer keeps the user's answer as the plain-output result line.
func TestAskUserPlainOutputShowsTheAnswer(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	out := tl.Succeeded("Ramen")
	step := dashi.Step{Kind: dashi.StepToolFinished, Tool: &dashi.ToolFact{
		Call:    tl.Call{ID: "q1", Name: "ask_user", Args: json.RawMessage(`{"question":"Favorite noodle?"}`)},
		Outcome: &out,
	}}
	b := onlyBlock(t, r.Event(stepEvent(step)))
	if b.Summary != "Ramen" {
		t.Fatalf("block=%+v", b)
	}
	if got := FormatBlock(b); !strings.Contains(got, "\n    └ Ramen") {
		t.Fatalf("plain=%q", got)
	}
}

func TestElapsedText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                        "<0.1s",
		99 * time.Millisecond:    "<0.1s",
		100 * time.Millisecond:   "0.1s",
		12400 * time.Millisecond: "12.4s",
	} {
		if got := elapsedText(d); got != want {
			t.Errorf("elapsedText(%v) = %q, want %q", d, got, want)
		}
	}
}
