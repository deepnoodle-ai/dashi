package workspace

import (
	"strings"
	"testing"
)

func TestPrintableTextEscapesControls(t *testing.T) {
	in := "ok\x1b[2K\rfake\x00\x7f\u0085‮\xff\nline\ttab"
	got := PrintableText(in)
	for _, r := range got {
		if r != '\n' && r != '\t' && unsafeRune(r) {
			t.Fatalf("control %U left in %q", r, got)
		}
	}
	for _, want := range []string{"␛[2K", "␍", "<U+0085>", "<U+202E>", "<0xFF>", "\nline\ttab"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
	if PrintableText("plain é 日本\n") != "plain é 日本\n" {
		t.Fatal("clean text changed")
	}
}

func TestApprovalRequestPrintable(t *testing.T) {
	r := ApprovalRequest{
		Tool:          ToolBash,
		Summary:       "run\x1b[H",
		Preview:       "+a\r-b",
		Dir:           "/w\x1b",
		Command:       "echo hi\x1b[2J\rrm -rf x",
		Args:          []byte(`{"message":"日本\u001b[2J"}`),
		RememberScope: "Exact command\x1b",
	}.Printable()
	for _, f := range []string{r.Summary, r.Preview, r.Dir, r.Command, r.Arguments, r.RememberScope} {
		if strings.ContainsAny(f, "\x1b\r") {
			t.Fatalf("raw control in %q", f)
		}
	}
	if !strings.Contains(r.Arguments, "日本") {
		t.Fatal("full arguments lost")
	}
	if !strings.Contains(r.Command, "rm -rf x") || r.Args != nil {
		t.Fatalf("command lost or args kept: %+v", r)
	}
}
