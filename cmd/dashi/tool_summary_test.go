package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	tl "github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
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

// The renderer puts the user's answer on the finished ask_user row, and the
// row draws it in the normal color rather than muted.
func TestAskUserRowShowsTheAnswer(t *testing.T) {
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
	styled := tui.Sprint(toolView(b), tui.WithWidth(80), tui.WithColor(true))
	if !strings.Contains(styled, "\x1b[0mRamen") || strings.Contains(styled, "248mRamen") {
		t.Fatalf("answer style=%q", styled)
	}
}

func TestFailedToolRowShowsTheError(t *testing.T) {
	b := Block{Kind: KindTool, Name: "bash", Arg: "make lint", State: StateFailed, Elapsed: 3200 * time.Millisecond,
		Summary: "exit 2 · undefined: foo", Detail: "undefined: foo\n[Exit code 2]", Done: true}
	plain := tui.Sprint(toolView(b), tui.WithWidth(80), tui.WithColor(false))
	if !strings.Contains(plain, "✗ bash  make lint") || !strings.Contains(plain, "failed  3.2s") ||
		!strings.Contains(plain, "  └ exit 2 · undefined: foo") {
		t.Fatalf("row=%q", plain)
	}
	styled := tui.Sprint(toolView(b), tui.WithWidth(80), tui.WithColor(true))
	if !strings.Contains(styled, "\x1b[0;31mexit 2 · undefined: foo") {
		t.Fatalf("error style=%q", styled)
	}
}

func TestToolRowOffersExpansionOnlyWhenThereIsMore(t *testing.T) {
	s := newScreen("fixture")
	s.width = 120
	s.tr.Apply(Block{ID: "one", Kind: KindTool, Name: "ask_user", State: StateOK, Summary: "Ramen", Detail: "Ramen", Done: true})
	s.tr.Apply(Block{ID: "many", Kind: KindTool, Name: "read_file", State: StateOK, Summary: "2 lines", Detail: "a\nb\n", Done: true})
	s.tr.Apply(Block{ID: "none", Kind: KindTool, Name: "task", State: StateOK, Done: true})
	row := func(i int) string { return tui.Sprint(s.Item(i), tui.WithWidth(118), tui.WithColor(false)) }
	if got := row(0); strings.Contains(got, "click to expand") || !strings.Contains(got, "└ Ramen") {
		t.Fatalf("single line=%q", got)
	}
	if got := row(1); !strings.Contains(got, "└ 2 lines · click to expand") {
		t.Fatalf("more=%q", got)
	}
	if got := row(2); strings.Contains(got, "└") {
		t.Fatalf("no result=%q", got)
	}
	// The hint is not part of what a selection copies, and plain output
	// never shows it.
	if src, _ := s.Source(1); src != "read_file\n2 lines" {
		t.Fatalf("source=%q", src)
	}
	if got := FormatBlock(s.tr.Blocks[1]); strings.Contains(got, "click") {
		t.Fatalf("plain=%q", got)
	}
	// Expanded, the row shows the whole output under the connector.
	s.expanded = map[int]bool{1: true}
	if got := row(1); !strings.Contains(got, "  └ a\n    b") || strings.Contains(got, "click") {
		t.Fatalf("expanded=%q", got)
	}
	if src, _ := s.Source(1); src != "read_file\na\nb" {
		t.Fatalf("expanded source=%q", src)
	}
}

func TestReadingWidthCapsToolRowsNotTheFooter(t *testing.T) {
	s := newScreen("fixture")
	s.width = 160
	s.tr.Apply(Block{ID: "t", Kind: KindTool, Name: "bash", Arg: "go test ./...", State: StateFailed, Elapsed: time.Second, Summary: "exit 1", Done: true})
	s.tr.Apply(Block{ID: "f", Kind: KindFooter, Footer: &footerFacts{Turn: 1, Calls: 1}, Done: true})
	s.tr.Apply(Block{ID: "a", Kind: KindAssistant, Text: strings.Repeat("word ", 60), Done: true})
	lines := func(i int) []string {
		return strings.Split(tui.Sprint(s.Item(i), tui.WithWidth(160), tui.WithColor(false)), "\n")
	}
	head := strings.TrimRight(lines(0)[0], " ")
	if runewidth.StringWidth(head) != readingWidth || !strings.HasSuffix(head, "failed  1.0s") {
		t.Fatalf("tool head is %d columns: %q", runewidth.StringWidth(head), head)
	}
	if footer := strings.TrimRight(lines(1)[0], " "); runewidth.StringWidth(footer) != 160 {
		t.Fatalf("footer is %d columns: %q", runewidth.StringWidth(footer), footer)
	}
	for _, line := range lines(2) {
		if w := runewidth.StringWidth(strings.TrimRight(line, " ")); w > readingWidth {
			t.Fatalf("reply line is %d columns: %q", w, line)
		}
	}
	native, _ := nativeBlockText(s.tr.Blocks[0], 160, false)
	if head := strings.Split(native, "\n")[0]; runewidth.StringWidth(strings.TrimRight(head, " ")) != readingWidth {
		t.Fatalf("native head=%q", head)
	}
}

func TestRunningToolRowSpins(t *testing.T) {
	s := newScreen("fixture")
	s.width = 80
	s.tr.Apply(Block{ID: "t", Kind: KindTool, Name: "grep", Arg: "TaskBudget", State: StateRunning})
	draw := func() string { return tui.Sprint(s.Item(0), tui.WithWidth(78), tui.WithColor(false)) }
	first := draw()
	if !strings.HasPrefix(first, spinnerFrames[0]+" grep") || !strings.Contains(first, "running") {
		t.Fatalf("first=%q", first)
	}
	s.HandleEvent(tui.TickEvent{Frame: 3})
	if second := draw(); !strings.HasPrefix(second, spinnerFrames[1]+" grep") {
		t.Fatalf("second=%q", second)
	}
	// Outside the live screen, a running row has a still glyph.
	if got := tui.Sprint(toolView(s.tr.Blocks[0]), tui.WithWidth(78), tui.WithColor(false)); !strings.HasPrefix(got, "● grep") {
		t.Fatalf("still=%q", got)
	}
}
