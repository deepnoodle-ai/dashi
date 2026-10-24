package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	tl "github.com/deepnoodle-ai/dashi/tool"
)

func TestNewToolLabel(t *testing.T) {
	long := "/repo/" + strings.Repeat("deep/", 12) + "render.go"
	for _, test := range []struct {
		tool, args    string
		running, done string
	}{
		{"read_file", `{"path":"/repo/cmd/dashi/render.go"}`, "Reading cmd/dashi/render.go…", "Read cmd/dashi/render.go"},
		{"read_file", `{"path":"` + long + `"}`, "Reading …ep/deep/deep/deep/deep/deep/deep/deep/render.go…", "Read …ep/deep/deep/deep/deep/deep/deep/deep/render.go"},
		{"read_file", `{}`, "Reading a file…", "Read a file"},
		{"edit_file", `{"path":"render.go","old_string":"a","new_string":"b"}`, "Editing render.go…", "Edited render.go"},
		{"write_file", `{"path":"notes.md","content":"x"}`, "Writing notes.md…", "Wrote notes.md"},
		{"list_directory", `{"path":"cmd/dashi"}`, "Listing cmd/dashi…", "Listed cmd/dashi"},
		{"list_directory", `{}`, "Listing the workspace…", "Listed the workspace"},
		{"glob", `{"pattern":"**/*.go"}`, "Finding files matching “**/*.go”…", "Found files matching “**/*.go”"},
		{"glob", `{"pattern":"` + strings.Repeat("x", 30) + `"}`, "Finding files…", "Found files"},
		{"grep", `{"pattern":"foo","path":"cmd"}`, "Searching for “foo”…", "Searched for “foo”"},
		{"grep", `{"pattern":"` + strings.Repeat("a", 40) + `"}`, "Searching for “" + strings.Repeat("a", 31) + "…”…", "Searched for “" + strings.Repeat("a", 31) + "…”"},
		{"bash", `{"command":"rm -rf /tmp/x"}`, "Running a command…", "Ran a command"},
		{"ask_user", `{"question":"Which?"}`, "Asking you…", "Asked you a question"},
		{"load_skill", `{"name":"pdf"}`, "Loading the pdf skill…", "Loaded the pdf skill"},
		{"read_skill_file", `{"skill":"pdf","path":"forms.md"}`, "Reading forms.md from the pdf skill…", "Read forms.md from the pdf skill"},
		{"task", `{"subagent":"explore","prompt":"find it"}`, "Starting the explore subagent…", "Delegated a task to explore"},
		{"task", `{"prompt":"find it"}`, "Starting a subagent…", "Delegated a task"},
		{"task_status", `{"task_id":"t_k3f2"}`, "Checking task t_k3f2…", "Checked task t_k3f2"},
		{"task_stop", `{"task_id":"t_k3f2"}`, "Stopping task t_k3f2…", "Stopped task t_k3f2"},
		{"task_send", `{"task_id":"t_k3f2","message":"hi"}`, "Messaging task t_k3f2…", "Messaged task t_k3f2"},
		{"message_parent", `{"message":"hi"}`, "Messaging the parent agent…", "Messaged the parent agent"},
		{"mcp__github__create_issue", `{"title":"x"}`, "Create issue · github", "Create issue · github"},
		{"lookup_weather", `{}`, "Lookup weather", "Lookup weather"},
		{"Bash", `{}`, "Bash", "Bash"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			got := newToolLabel(test.tool, json.RawMessage(test.args), "/repo")
			if got.Running != test.running || got.Done != test.done {
				t.Fatalf("got %+v, want %q / %q", got, test.running, test.done)
			}
		})
	}
}

func TestToolLabelTextFollowsTheState(t *testing.T) {
	b := Block{Kind: KindTool, Name: "edit_file", Label: newToolLabel("edit_file", json.RawMessage(`{"path":"a.go"}`), "")}
	for state, want := range map[string]string{
		StateRunning:        "Editing a.go…",
		StateChecking:       "Editing a.go…",
		StateWaitingForYou:  "Editing a.go…",
		StateCheckingResult: "Editing a.go…",
		StateOK:             "Edited a.go",
		StateFailed:         "Edited a.go",
		StateDenied:         "Editing a.go",
	} {
		b.State = state
		if got := toolLabelText(b); got != want {
			t.Errorf("%s: got %q, want %q", state, got, want)
		}
	}
	// A block without a label, such as one a test builds, uses the tool's
	// phrase without a subject.
	if got := toolLabelText(Block{Name: "bash", State: StateOK}); got != "Ran a command" {
		t.Fatalf("bare=%q", got)
	}
}

func TestToolInput(t *testing.T) {
	if got := toolInput("bash", json.RawMessage(`{"command":"go test ./...\necho done","timeout":30}`), ""); got != "$ go test ./...\n  echo done\ntimeout: 30" {
		t.Fatalf("bash=%q", got)
	}
	got := toolInput("grep", json.RawMessage(`{"pattern":"foo","path":"/repo/cmd","output_mode":"content"}`), "/repo")
	if got != "output_mode: content\npath: cmd\npattern: foo" {
		t.Fatalf("grep=%q", got)
	}
	content := strings.Repeat("line\n", inputLineLimit+5)
	data, _ := json.Marshal(map[string]string{"path": "a.md", "content": content})
	got = toolInput("write_file", data, "")
	if !strings.HasPrefix(got, "content:\n  line\n  line") || !strings.Contains(got, "\n  … 5 more lines\npath: a.md") {
		t.Fatalf("write=%q", got)
	}
	if got := toolInput("x", json.RawMessage(`{}`), ""); got != "" {
		t.Fatalf("empty=%q", got)
	}
}

func TestRendererLabelsAToolCall(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	call := tl.Call{ID: "w1", Name: "write_file", Args: json.RawMessage(`{"path":"/repo/notes.md","content":"hi\n"}`)}
	b := onlyBlock(t, r.Event(stepEvent(dashi.Step{Kind: dashi.StepToolStarted, Tool: &dashi.ToolFact{Call: call}})))
	if toolLabelText(b) != "Writing notes.md…" || !strings.Contains(b.Input, "path: notes.md") {
		t.Fatalf("started=%+v", b)
	}
	out := tl.Succeeded("Created notes.md (1 line).")
	b = onlyBlock(t, r.Event(stepEvent(dashi.Step{Kind: dashi.StepToolFinished, Tool: &dashi.ToolFact{Call: call, Outcome: &out}})))
	if got := toolLabelText(b); got != "Created notes.md" {
		t.Fatalf("finished label=%q", got)
	}
}
