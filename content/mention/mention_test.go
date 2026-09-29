package mention

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

var pngBytes = append(
	[]byte("\x89PNG\r\n\x1a\n"),
	bytes.Repeat([]byte{1}, 32)...,
)

var pdfBytes = []byte("%PDF-1.4 fixture")

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExpandTextFileBecomesTitledDocument(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "wake", "wake.go"), []byte("package wake\n"))

	res := Expand("explain @wake/wake.go", Options{Root: root})

	if res.Text != "explain [file: wake/wake.go]" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.Blocks) != 1 || len(res.Warnings) != 0 {
		t.Fatalf("blocks = %d, warnings = %v", len(res.Blocks), res.Warnings)
	}
	doc := res.Blocks[0].Document
	if res.Blocks[0].Kind != content.KindDocument || doc == nil {
		t.Fatalf("block is not a document: %+v", res.Blocks[0])
	}
	if doc.Title != "wake/wake.go" {
		t.Fatalf("title = %q", doc.Title)
	}
	if doc.Source.MIMEType != "text/plain" {
		t.Fatalf("MIME type = %q", doc.Source.MIMEType)
	}
	if string(doc.Source.Data) != "package wake\n" {
		t.Fatalf("data = %q", doc.Source.Data)
	}
	if got := res.Attached[0]; got.Path != "wake/wake.go" || got.Size != 13 {
		t.Fatalf("attached = %+v", got)
	}
}

func TestExpandEmailAddressIsNotMention(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "example.com"), []byte("x"))

	res := Expand("mail me@example.com", Options{Root: root})

	if res.Text != "mail me@example.com" || len(res.Blocks) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestExpandMentionAfterOpeningBracket(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))

	res := Expand("see (@a.go) now", Options{Root: root})

	if res.Text != "see ([file: a.go]) now" || len(res.Blocks) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExpandTrailingPunctuationIsNotPath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))

	for _, input := range []string{
		"read @a.go.",
		"read @a.go?",
		"read @a.go, please",
		"read @a.go;",
	} {
		res := Expand(input, Options{Root: root})
		if len(res.Blocks) != 1 || len(res.Warnings) != 0 {
			t.Fatalf("%q: blocks = %d, warnings = %v", input, len(res.Blocks), res.Warnings)
		}
		if !strings.Contains(res.Text, "[file: a.go]") {
			t.Fatalf("%q: text = %q", input, res.Text)
		}
	}
	res := Expand("read @a.go.", Options{Root: root})
	if res.Text != "read [file: a.go]." {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestExpandQuotedMentionWithSpaces(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "my notes.md"), []byte("# notes"))

	res := Expand(`read @"my notes.md" today`, Options{Root: root})

	if res.Text != "read [file: my notes.md] today" || len(res.Blocks) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExpandDuplicateMentionAttachesOnce(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))

	res := Expand("compare @a.go with @./a.go", Options{Root: root})

	if len(res.Blocks) != 1 || len(res.Attached) != 1 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	if strings.Contains(res.Text, "@") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestExpandImagePathForms(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(root, "err.png"), pngBytes)
	abs := write(t, filepath.Join(root, "abs", "a.png"), pngBytes)
	write(t, filepath.Join(home, "Desktop", "Screen Shot 1.png"), pngBytes)
	write(t, filepath.Join(root, "sp ace", "b.png"), pngBytes)
	spaced := filepath.Join(root, "sp ace", "b.png")
	fileURL := "file://" + abs

	tests := []struct {
		name  string
		input string
	}{
		{name: "relative", input: "what is wrong in ./err.png"},
		{name: "relative without dot", input: "what is wrong in err.png"},
		{name: "absolute", input: "look at " + abs},
		{name: "home", input: `look at ~/Desktop/Screen\ Shot\ 1.png`},
		{name: "backslash escaped", input: "look at " + strings.ReplaceAll(spaced, " ", `\ `)},
		{name: "single quoted", input: "look at '" + spaced + "'"},
		{name: "double quoted", input: `look at "` + spaced + `"`},
		{name: "file URL", input: "look at " + fileURL},
		{name: "in brackets", input: "look (./err.png)."},
		{name: "mention", input: "look at @err.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Expand(tt.input, Options{Root: root})
			if len(res.Warnings) != 0 {
				t.Fatalf("warnings = %v", res.Warnings)
			}
			if len(res.Blocks) != 1 || res.Blocks[0].Image == nil {
				t.Fatalf("blocks = %+v", res.Blocks)
			}
			if res.Attached[0].MIMEType != "image/png" {
				t.Fatalf("attached = %+v", res.Attached[0])
			}
			if !bytes.Equal(res.Blocks[0].Image.Source.Data, pngBytes) {
				t.Fatal("image bytes differ")
			}
		})
	}
}

func TestExpandBarePathStaysInText(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "err.png"), pngBytes)

	res := Expand("what is wrong in ./err.png please", Options{Root: root})

	if res.Text != "what is wrong in ./err.png please" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestExpandBarePDF(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.pdf"), pdfBytes)

	res := Expand("see spec.pdf and @spec.pdf", Options{Root: root})

	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	doc := res.Blocks[0].Document
	if doc == nil || doc.Source.MIMEType != "application/pdf" {
		t.Fatalf("block = %+v", res.Blocks[0])
	}
	if doc.Title != "spec.pdf" {
		t.Fatalf("title = %q", doc.Title)
	}
}

func TestExpandBareTextPathStaysProse(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "main.go"), []byte("package main"))
	input := "panic at main.go:12 in ./main.go"

	res := Expand(input, Options{Root: root})

	if res.Text != input || len(res.Blocks) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExpandApostropheDoesNotSwallowMention(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))

	res := Expand("it doesn't work, see @a.go", Options{Root: root})

	if len(res.Blocks) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExpandWarnings(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "big.png"), append(pngBytes, make([]byte, 100)...))
	write(t, filepath.Join(root, "bin.dat"), []byte{0, 1, 2, 3})
	write(t, filepath.Join(root, "latin.txt"), []byte{0xff, 0xfe, 'a'})
	write(t, filepath.Join(root, "long.txt"), bytes.Repeat([]byte("a"), 2048))
	write(t, filepath.Join(root, "empty.txt"), nil)
	write(t, filepath.Join(root, "ok.txt"), []byte("hello"))
	write(t, filepath.Join(root, "second.txt"), []byte("world"))
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	opt := Options{
		Root:          root,
		MaxImageBytes: 100,
		MaxTextBytes:  1024,
		MaxTotalBytes: 400,
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"missing", "@nope.go", "Skipped @nope.go: no such file."},
		{"directory", "@dir", "Skipped @dir: it is a directory."},
		{"binary", "@bin.dat", "Skipped @bin.dat: not text, an image, or a PDF."},
		{"invalid UTF-8", "@latin.txt", "Skipped @latin.txt: not text, an image, or a PDF."},
		{"large image", "@big.png", "Skipped @big.png: 140 bytes is over the 100 bytes image limit."},
		{"large bare image", "big.png", "Skipped big.png: 140 bytes is over the 100 bytes image limit."},
		{"large text", "@long.txt", "Skipped @long.txt: 2 KB is over the 1 KB text limit."},
		{"empty", "@empty.txt", "Skipped @empty.txt: the file is empty."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Expand(tt.input, opt)
			if len(res.Warnings) != 1 || res.Warnings[0] != tt.want {
				t.Fatalf("warnings = %q, want %q", res.Warnings, tt.want)
			}
			if len(res.Blocks) != 0 {
				t.Fatalf("blocks = %d", len(res.Blocks))
			}
			if res.Text != tt.input {
				t.Fatalf("text = %q", res.Text)
			}
		})
	}

	res := Expand("@ok.txt @second.txt", opt)
	if len(res.Blocks) != 1 || len(res.Warnings) != 1 {
		t.Fatalf("total limit: blocks = %d, warnings = %q", len(res.Blocks), res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "limit for one message") {
		t.Fatalf("warning = %q", res.Warnings[0])
	}
	if res.Text != "[file: ok.txt] @second.txt" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestExpandNegativeLimitTurnsLimitOff(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "long.txt"), bytes.Repeat([]byte("a"), 300<<10))

	res := Expand("@long.txt", Options{Root: root})
	if len(res.Blocks) != 0 {
		t.Fatal("default text limit did not apply")
	}
	res = Expand("@long.txt", Options{Root: root, MaxTextBytes: -1})
	if len(res.Blocks) != 1 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestExpandDefaultImageLimitCountsBase64Size(t *testing.T) {
	root := t.TempDir()
	limit := 5 << 20 * 3 / 4
	write(t, filepath.Join(root, "ok.png"), append(pngBytes, make([]byte, limit-len(pngBytes))...))
	write(t, filepath.Join(root, "big.png"), append(pngBytes, make([]byte, limit-len(pngBytes)+1)...))

	res := Expand("@ok.png @big.png", Options{Root: root})

	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(res.Blocks))
	}
	if got := len(res.Blocks[0].Image.Source.Data) * 4 / 3; got > 5<<20 {
		t.Fatalf("base64 size = %d, over 5 MiB", got)
	}
	want := "Skipped @big.png: 3.75 MB is over the 3.75 MB image limit."
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "Skipped @big.png:") {
		t.Fatalf("warnings = %q, want prefix of %q", res.Warnings, want)
	}
}

func TestExpandTotalLimitCountsBase64Size(t *testing.T) {
	root := t.TempDir()
	size := 5 << 20 * 3 / 4
	var input []string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("img%d.png", i)
		write(t, filepath.Join(root, name), append(pngBytes, make([]byte, size-len(pngBytes))...))
		input = append(input, "@"+name)
	}

	// Eight images total 30 MiB raw, under the 32 MiB limit, but about 40
	// MiB as base64.
	res := Expand(strings.Join(input, " "), Options{Root: root})

	if len(res.Blocks) != 6 || len(res.Warnings) != 2 {
		t.Fatalf("blocks = %d, warnings = %q", len(res.Blocks), res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "limit for one message") {
		t.Fatalf("warning = %q", res.Warnings[0])
	}
	res = Expand(strings.Join(input, " "), Options{Root: root, MaxTotalBytes: -1})
	if len(res.Blocks) != 8 {
		t.Fatalf("unlimited: blocks = %d", len(res.Blocks))
	}
}

func TestExpandTotalLimitCountsTextAsBase64(t *testing.T) {
	root := t.TempDir()
	body := []byte(strings.Repeat("a", 250<<10))
	var input []string
	for i := 0; i < 110; i++ {
		name := fmt.Sprintf("doc%d.txt", i)
		write(t, filepath.Join(root, name), body)
		input = append(input, "@"+name)
	}

	// 110 files total about 26.8 MiB raw, under the 32 MiB limit, but about
	// 35.7 MiB as base64. Only 96 fit.
	res := Expand(strings.Join(input, " "), Options{Root: root})

	if len(res.Blocks) != 96 || len(res.Warnings) != 14 {
		t.Fatalf("blocks = %d, warnings = %d", len(res.Blocks), len(res.Warnings))
	}
	if !strings.Contains(res.Warnings[0], "limit for one message") {
		t.Fatalf("warning = %q", res.Warnings[0])
	}
}

func TestExpandLineReferenceAttachesFile(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "main.go"), []byte("package main"))

	for _, in := range []string{"see @main.go:42 now", "see @main.go:42-50 now"} {
		res := Expand(in, Options{Root: root})
		if len(res.Blocks) != 1 || len(res.Warnings) != 0 {
			t.Fatalf("%q: blocks = %d, warnings = %q", in, len(res.Blocks), res.Warnings)
		}
		if !strings.HasPrefix(res.Text, "see [file: main.go]:4") {
			t.Fatalf("%q: text = %q", in, res.Text)
		}
	}
}

func TestExpandMentionInBackticksOrQuotesAttaches(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))

	for _, in := range []string{"see `@a.go`", `see "@a.go"`, "see (`@a.go`)."} {
		res := Expand(in, Options{Root: root})
		if len(res.Blocks) != 1 || len(res.Warnings) != 0 {
			t.Fatalf("%q: blocks = %d, warnings = %q", in, len(res.Blocks), res.Warnings)
		}
	}
}

func TestExpandExtensionlessImageIsSniffed(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "shot"), pngBytes)

	res := Expand("@shot", Options{Root: root})

	if len(res.Blocks) != 1 || res.Blocks[0].Image == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestExpandBlocksPassValidate(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), []byte("package a"))
	write(t, filepath.Join(root, "a.png"), pngBytes)
	write(t, filepath.Join(root, "a.pdf"), pdfBytes)

	res := Expand("@a.go @a.png @a.pdf", Options{Root: root})

	if len(res.Blocks) != 3 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	for _, b := range res.Blocks {
		if err := b.Validate(); err != nil {
			t.Fatalf("block does not validate: %v", err)
		}
	}
}

func TestExpandNoMentionsReturnsInputUnchanged(t *testing.T) {
	input := "  just  some\ttext\n@ alone and @"

	res := Expand(input, Options{Root: t.TempDir()})

	if res.Text != input || len(res.Blocks) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}
}
