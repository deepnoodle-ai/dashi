package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLaunchctl records launchctl calls and fails the ones named in fail.
type fakeLaunchctl struct {
	calls [][]string
	fail  map[string]bool // by first argument
}

func (f *fakeLaunchctl) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.fail[args[0]] {
		return "Boot-out failed: 3: No such process", errors.New("exit status 3")
	}
	return "", nil
}

func testScheduler(t *testing.T, goos string) (scheduler, *fakeLaunchctl) {
	t.Helper()
	f := &fakeLaunchctl{fail: map[string]bool{}}
	return scheduler{
		GOOS:       goos,
		PlistPath:  filepath.Join(t.TempDir(), "LaunchAgents", scheduleLabel+".plist"),
		UID:        501,
		Executable: "/Users/me/go/bin/agent-archive",
		Launchctl:  f.run,
	}, f
}

func TestScheduleInstall(t *testing.T) {
	s, f := testScheduler(t, "darwin")
	claudeDir, _ := expandHome("~/.claude")
	archive := filepath.Join(t.TempDir(), "my archive & co")
	o := options{ArchiveDir: archive, ClaudeDir: claudeDir, CodexDir: "/elsewhere/.codex"}
	var out bytes.Buffer
	if err := s.install(o, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(archive, "logs")
	esc := func(p string) string { return strings.ReplaceAll(p, "&", "&amp;") }
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>ai.deepnoodle.agent-archive</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/me/go/bin/agent-archive</string>
		<string>copy</string>
		<string>-archive</string>
		<string>` + esc(archive) + `</string>
		<string>-codex-dir</string>
		<string>/elsewhere/.codex</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>3</integer>
		<key>Minute</key>
		<integer>0</integer>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>Umask</key>
	<integer>63</integer>
	<key>StandardOutPath</key>
	<string>` + esc(filepath.Join(logs, "copy-stdout.log")) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(filepath.Join(logs, "copy-stderr.log")) + `</string>
</dict>
</plist>
`
	if string(b) != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", b, want)
	}
	wantCalls := [][]string{
		{"bootout", "gui/501/ai.deepnoodle.agent-archive"},
		{"bootstrap", "gui/501", s.PlistPath},
	}
	if !equalCalls(f.calls, wantCalls) {
		t.Fatalf("launchctl calls %v, want %v", f.calls, wantCalls)
	}
	for _, name := range []string{"copy-stdout.log", "copy-stderr.log"} {
		info, err := os.Stat(filepath.Join(logs, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("log file %s: %v %v", name, info, err)
		}
	}
	if info, err := os.Stat(logs); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("logs folder: %v %v", info, err)
	}
}

func TestScheduleInstallDefaultsPassNoFlags(t *testing.T) {
	s, _ := testScheduler(t, "darwin")
	var o options
	for _, p := range []struct {
		dst *string
		v   string
	}{{&o.ArchiveDir, "~/agent-archive"}, {&o.ClaudeDir, "~/.claude"}, {&o.CodexDir, "~/.codex"}} {
		*p.dst, _ = expandHome(p.v)
	}
	b, err := s.plist(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<string>copy</string>\n\t</array>") {
		t.Fatalf("plist passes flags for defaults:\n%s", b)
	}
}

func TestScheduleInstallReplacesLoadedAgent(t *testing.T) {
	s, f := testScheduler(t, "darwin")
	o := options{ArchiveDir: t.TempDir()}
	if err := s.install(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	s.Executable = "/usr/local/bin/agent-archive"
	f.calls = nil
	if err := s.install(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0][0] != "bootout" || f.calls[1][0] != "bootstrap" {
		t.Fatalf("calls %v", f.calls)
	}
	if b, _ := os.ReadFile(s.PlistPath); !strings.Contains(string(b), "/usr/local/bin/agent-archive") {
		t.Fatal("plist not replaced")
	}
}

func TestScheduleInstallFails(t *testing.T) {
	s, f := testScheduler(t, "darwin")
	f.fail["bootstrap"] = true
	if err := s.install(options{ArchiveDir: t.TempDir()}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("err %v", err)
	}
	s.Executable = "/var/folders/x/go-build123/b001/exe/agent-archive"
	if err := s.install(options{ArchiveDir: t.TempDir()}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "go install") {
		t.Fatalf("go run binary: err %v", err)
	}
}

func TestScheduleRemove(t *testing.T) {
	s, f := testScheduler(t, "darwin")
	if err := s.install(options{ArchiveDir: t.TempDir()}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	var out bytes.Buffer
	if err := s.remove(&out); err != nil {
		t.Fatal(err)
	}
	if !equalCalls(f.calls, [][]string{{"bootout", "gui/501/ai.deepnoodle.agent-archive"}}) {
		t.Fatalf("calls %v", f.calls)
	}
	if _, err := os.Stat(s.PlistPath); !os.IsNotExist(err) {
		t.Fatal("plist not deleted")
	}
	// Removing again: nothing is loaded and there is no plist.
	f.fail["bootout"] = true
	out.Reset()
	if err := s.remove(&out); err != nil || !strings.Contains(out.String(), "no schedule is installed") {
		t.Fatalf("second remove: %v %q", err, out.String())
	}
}

func TestScheduleNeedsMacOS(t *testing.T) {
	s, f := testScheduler(t, "linux")
	if err := s.install(options{ArchiveDir: t.TempDir()}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("install: %v", err)
	}
	if err := s.remove(&bytes.Buffer{}); err == nil {
		t.Fatal("remove worked on linux")
	}
	if len(f.calls) != 0 {
		t.Fatal("launchctl was called")
	}
	if _, err := os.Stat(s.PlistPath); !os.IsNotExist(err) {
		t.Fatal("plist written")
	}
}

func TestScheduleCommandLine(t *testing.T) {
	s, f := testScheduler(t, "darwin")
	old := newScheduler
	newScheduler = func() (scheduler, error) { return s, nil }
	t.Cleanup(func() { newScheduler = old })
	archive := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run([]string{"schedule", "install", "-archive", archive}, &out, &errOut); code != 0 {
		t.Fatalf("install exit %d: %s", code, errOut.String())
	}
	if b, _ := os.ReadFile(s.PlistPath); !strings.Contains(string(b), "<string>"+archive+"</string>") {
		t.Fatalf("plist lacks -archive:\n%s", b)
	}
	if code := run([]string{"schedule", "remove"}, &out, &errOut); code != 0 {
		t.Fatalf("remove exit %d: %s", code, errOut.String())
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls %v", f.calls)
	}
	for _, args := range [][]string{{"schedule"}, {"schedule", "start"}, {"schedule", "remove", "-archive", archive}} {
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func equalCalls(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i], "\x00") != strings.Join(b[i], "\x00") {
			return false
		}
	}
	return true
}
