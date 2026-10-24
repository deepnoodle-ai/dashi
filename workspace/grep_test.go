package workspace

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestGoSearchRefusesHugeBraceGlobAndStopsWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.go", "hit\n")
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := grep(context.Background(), s, "", grepArgs{Pattern: "hit", Glob: strings.Repeat("{a,b}", 30)})
	text := out.Output.Content[0].Text.Text
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "fewer {a,b} groups")

	// rg refuses a glob with bad syntax, so the Go search must too.
	out, _ = grep(context.Background(), s, "", grepArgs{Pattern: "hit", Glob: "[unclosed"})
	text = out.Output.Content[0].Text.Text
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "is not valid")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _ = grep(ctx, s, "", grepArgs{Pattern: "hit"})
	text = out.Output.Content[0].Text.Text
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "canceled")
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestSearchFileStreamsLargeFile(t *testing.T) {
	line := strings.Repeat("x", 100) + " hit\n"
	cr := &countingReader{r: strings.NewReader(strings.Repeat(line, 500_000))} // 50 MB
	var got []string
	searchFile(cr, regexp.MustCompile("hit"), "big.txt", 0, "content", func(l string) bool {
		got = append(got, l)
		return len(got) < 3
	})
	if len(got) != 3 {
		t.Fatalf("got %d lines", len(got))
	}
	if cr.n > 1<<20 {
		t.Fatalf("read %d bytes before the sink filled", cr.n)
	}
}

func TestSearchFileContext(t *testing.T) {
	text := "a\nb\nhit1\nc\nd\ne\nf\nhit2\ng\n"
	var got []string
	searchFile(strings.NewReader(text), regexp.MustCompile("hit"), "f", 1, "content", func(l string) bool {
		got = append(got, l)
		return true
	})
	want := "f-2-b|f:3:hit1|f-4-c|--|f-7-f|f:8:hit2|f-9-g"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q", strings.Join(got, "|"))
	}
	got = nil
	searchFile(strings.NewReader(text), regexp.MustCompile("hit"), "f", 0, "count", func(l string) bool {
		got = append(got, l)
		return true
	})
	if strings.Join(got, "|") != "f:2" {
		t.Fatalf("count got %v", got)
	}
}
