package main

import (
	"strings"
	"testing"
)

func rowsText(rows []approvalRow) string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.String()
	}
	return strings.Join(lines, "\n")
}

func TestLayoutApprovalShowsArgumentsAsKeyValueLines(t *testing.T) {
	a := toolApproval{ApprovalRequest: ApprovalRequest{
		Tool:      "mcp__docs__search",
		Summary:   "mcp__docs__search",
		Arguments: `{"query":"retry policy","limit":5,"exact":false,"cursor":null,"filters":{"lang":"go","tags":["http","retry"],"empty":{}},"notes":"line one\nline two\u001b[2J","none":[]}`,
	}}
	l := layoutApproval(a, 0, false)
	if l.summary != "retry policy" {
		t.Fatalf("summary=%q", l.summary)
	}
	want := strings.Join([]string{
		"limit: 5",
		"exact: false",
		"cursor: null",
		"filters:",
		"  lang: go",
		"  tags:",
		"    - http",
		"    - retry",
		"  empty: {}",
		"notes:",
		"  line one",
		"  line two␛[2J",
		"none: []",
	}, "\n")
	if got := rowsText(l.rows); got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
}

func TestLayoutApprovalClipsLongValuesUntilExpanded(t *testing.T) {
	prompt := strings.Repeat("a paragraph line\n", 19) + "LAST LINE"
	a := toolApproval{ApprovalRequest: ApprovalRequest{
		Tool:      "task",
		Summary:   "task",
		Arguments: `{"subagent":"explore","description":"wait for a message","prompt":` + jsonString(prompt) + `}`,
	}}
	l := layoutApproval(a, 60, false)
	if l.summary != "explore subagent: wait for a message" || !l.clipped {
		t.Fatalf("summary=%q clipped=%v", l.summary, l.clipped)
	}
	got := rowsText(l.rows)
	if len(l.rows) != clippedValueRows || !strings.HasSuffix(got, "… +16 more lines") || strings.Contains(got, "LAST LINE") {
		t.Fatalf("clipped rows:\n%s", got)
	}
	if strings.Contains(got, "description") || strings.Contains(got, "subagent") {
		t.Fatalf("summary arguments repeated:\n%s", got)
	}
	full := layoutApproval(a, 60, true)
	if full.clipped || len(full.rows) != 21 || !strings.Contains(rowsText(full.rows), "LAST LINE") {
		t.Fatalf("expanded rows:\n%s", rowsText(full.rows))
	}
	// A long single-line value wraps below its key, within the width.
	long := toolApproval{ApprovalRequest: ApprovalRequest{Tool: "x", Arguments: `{"text":"` + strings.Repeat("word ", 30) + `"}`}}
	for _, r := range layoutApproval(long, 40, false).rows {
		if w := len(r.String()); w > 40 {
			t.Fatalf("row %q is %d wide", r.String(), w)
		}
	}
}

func TestLayoutApprovalHidesInternalIDsAndRepeats(t *testing.T) {
	bash := layoutApproval(toolApproval{ApprovalRequest: ApprovalRequest{
		Tool: "bash", Summary: "run: go test ./...", Command: "go test ./...\necho done", Dir: "/work",
		Arguments: `{"command":"go test ./...\necho done","timeout":1000}`, RememberScope: "This exact Bash command in this workspace",
	}, Task: "t_abc123"}, 80, false)
	want := strings.Join([]string{
		"From task t_abc123 · /task t_abc123 shows it",
		"$ go test ./...",
		"  echo done",
		"in /work",
		"timeout: 1000",
		"Allow for this Dashi run covers this exact Bash command in this workspace",
	}, "\n")
	if bash.summary != "" || rowsText(bash.rows) != want {
		t.Fatalf("summary=%q rows:\n%s", bash.summary, rowsText(bash.rows))
	}
	edit := layoutApproval(toolApproval{ApprovalRequest: ApprovalRequest{
		Tool: "edit_file", Summary: "edit main.go", Preview: "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-a\n+b\n",
		Arguments: `{"path":"main.go","old_string":"a","new_string":"b","replace_all":true}`,
	}}, 80, false)
	if edit.summary != "edit main.go" || rowsText(edit.rows) != "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-a\n+b\nreplace_all: true" {
		t.Fatalf("summary=%q rows:\n%s", edit.summary, rowsText(edit.rows))
	}
	// A summary too long for the title moves to the details whole.
	long := strings.Repeat("read the supporting file ", 6)
	l := layoutApproval(toolApproval{ApprovalRequest: ApprovalRequest{Tool: "read_skill_file", Summary: long}}, 60, false)
	if l.summary != "" || !strings.HasPrefix(rowsText(l.rows), "read the supporting file") {
		t.Fatalf("summary=%q rows:\n%s", l.summary, rowsText(l.rows))
	}
}

func TestApprovalTextForLineMode(t *testing.T) {
	got := approvalText(toolApproval{ApprovalRequest: ApprovalRequest{
		Tool: "task", Summary: "task", Arguments: `{"subagent":"explore","prompt":"first\nsecond"}`,
	}})
	want := "Allow task?  explore subagent\n  prompt:\n    first\n    second\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
