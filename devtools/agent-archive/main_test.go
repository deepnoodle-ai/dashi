package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandLine(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	dirs := []string{"-archive", e.opts.ArchiveDir}
	var out, errOut bytes.Buffer
	args := append([]string{"copy"}, dirs...)
	args = append(args, "-claude-dir", e.opts.ClaudeDir, "-codex-dir", e.opts.CodexDir)
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("copy exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "originals copied: 16") {
		t.Fatalf("copy output:\n%s", out.String())
	}

	out.Reset()
	if code := run(append([]string{"status"}, dirs...), &out, &errOut); code != 0 {
		t.Fatalf("status exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"last run:", "originals seen: 16, copied: 16", "claude_code", "session", "failed files: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := run(append([]string{"rebuild"}, dirs...), &out, &errOut); code != 0 {
		t.Fatalf("rebuild exit %d: %s", code, errOut.String())
	}

	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
	if code := run([]string{"copy", "-bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("bad flag exit %d", code)
	}
}
