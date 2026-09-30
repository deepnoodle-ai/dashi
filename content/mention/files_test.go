package mention

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestScanExplicitByteSpans(t *testing.T) {
	input := "雪 (@\"a b.txt\":12-15), @c\\ d.go:2! bare.png and person@example.com"
	matches := Scan(input)
	if len(matches) != 2 {
		t.Fatalf("%+v", matches)
	}
	want := []string{`@"a b.txt":12-15`, `@c\ d.go:2`}
	paths := []string{"a b.txt:12-15", "c d.go:2"}
	for i, m := range matches {
		if input[m.Start:m.End] != want[i] || m.Path != paths[i] {
			t.Fatalf("match %+v = %q", m, input[m.Start:m.End])
		}
	}
}

func TestSelectedLiteralSuffixSnapshotAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("nonblocking selection is unavailable on Windows")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"plain.txt": "original", "plain.txt:12": "literal"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	literal, err := ReadFile("plain.txt:12", Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if literal.LineSuffix != "" || string(literal.Block.Document.Source.Data) != "literal" {
		t.Fatalf("%+v", literal)
	}
	fallback, err := ReadFile("plain.txt:13", Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.LineSuffix != ":13" || string(fallback.Block.Document.Source.Data) != "original" {
		t.Fatalf("%+v", fallback)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if string(fallback.Block.Document.Source.Data) != "original" {
		t.Fatal("snapshot changed")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "plain.txt"), link); err != nil {
		t.Skip(err)
	}
	canonical, _, err := ResolvePath(link, Options{})
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if canonical != real {
		t.Fatalf("%q != %q", canonical, real)
	}
}

func TestSelectedContentAndLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("nonblocking selection is unavailable on Windows")
	}
	for _, tt := range []struct {
		name, body string
		limit      int64
		ok         bool
	}{
		{"ok.txt", "hello", 5, true}, {"large.txt", "hello!", 5, false}, {"empty.txt", "", 10, false},
		{"binary.txt", "a\x00b", 10, false}, {"false.png", "ordinary prose", 1024, false},
		{"real.png", "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32), 1024, true},
		{"real.pdf", "%PDF-1.7\n", 1024, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.name)
			os.WriteFile(path, []byte(tt.body), 0o600)
			_, err := ReadFile(path, Options{MaxTextBytes: tt.limit, MaxImageBytes: tt.limit, MaxFileBytes: tt.limit})
			if (err == nil) != tt.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if _, err := ReadFile(t.TempDir(), Options{}); err == nil {
		t.Fatal("directory accepted")
	}
}

type cancelRead struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelRead) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	copy(p, "abc")
	return 3, nil
}
func TestSelectionCancellationAndWireOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelRead{cancel: cancel}
	if _, err := readSelected(ctx, r, 100); !errors.Is(err, context.Canceled) || r.reads != 1 {
		t.Fatalf("reads=%d err=%v", r.reads, err)
	}
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := ReadFileContext(cancelled, "missing", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := WireSize(math.MaxInt64); err == nil {
		t.Fatal("overflow accepted")
	}
	var got []int64
	for _, n := range []int64{0, 1, 2, 3, 4} {
		w, err := WireSize(n)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, w)
	}
	if !reflect.DeepEqual(got, []int64{256, 260, 260, 260, 264}) {
		t.Fatal(got)
	}
}
