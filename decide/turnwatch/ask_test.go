package turnwatch

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutMiddle(t *testing.T) {
	if got := cutMiddle("short", 10); got != "short" {
		t.Fatalf("cutMiddle kept %q", got)
	}
	text := "--- FAIL: TestParse\n" + strings.Repeat("log line\n", 100) +
		"parser_test.go:41: got 4 tokens, want 3"
	got := cutMiddle(text, 100)
	if !strings.HasPrefix(got, "--- FAIL: TestParse") ||
		!strings.HasSuffix(got, "got 4 tokens, want 3") ||
		!strings.Contains(got, elided) || len(got) > 100+len(elided) {
		t.Fatalf("cutMiddle = %q", got)
	}
	// Both cuts end on whole characters.
	wide := strings.Repeat("é", 100)
	if got := cutMiddle(wide, 51); !utf8.ValidString(got) {
		t.Fatalf("cutMiddle split a character: %q", got)
	}
}
