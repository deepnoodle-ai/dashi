package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// scheduleLabel names the LaunchAgent; its plist is <label>.plist.
const scheduleLabel = "ai.deepnoodle.agent-archive"

// Log files the scheduled copy writes, in <archive>/logs/. logs/ is not an
// app folder, so recovery never takes these for copies.
const (
	scheduleStdoutLog = "copy-stdout.log"
	scheduleStderrLog = "copy-stderr.log"
)

// scheduler installs and removes the daily LaunchAgent. Tests set every
// field, so they never touch the real LaunchAgents folder or launchctl.
type scheduler struct {
	GOOS       string
	PlistPath  string
	UID        int
	Executable string // absolute path of the agent-archive binary to run
	// Launchctl runs launchctl with args and returns its combined output.
	Launchctl func(args ...string) (string, error)
}

// defaultScheduler is the real one: this user's LaunchAgents folder,
// launchctl, and the running executable.
func defaultScheduler() (scheduler, error) {
	s := scheduler{GOOS: runtime.GOOS, UID: os.Getuid(), Launchctl: runLaunchctl}
	home, err := os.UserHomeDir()
	if err != nil {
		return s, err
	}
	s.PlistPath = filepath.Join(home, "Library", "LaunchAgents", scheduleLabel+".plist")
	exe, err := os.Executable()
	if err != nil {
		return s, err
	}
	if s.Executable, err = filepath.EvalSymlinks(exe); err != nil {
		return s, err
	}
	return s, nil
}

func runLaunchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s scheduler) checkOS() error {
	if s.GOOS != "darwin" {
		return fmt.Errorf("schedule uses macOS launchd and does not work on %s; run agent-archive copy daily from cron or a systemd timer instead", s.GOOS)
	}
	return nil
}

// domain is the launchd domain of this user's GUI session.
func (s scheduler) domain() string { return "gui/" + strconv.Itoa(s.UID) }

// install writes the plist and loads it, unloading an older one first.
// launchd runs the copy at load (RunAtLoad), daily at 03:00, and at wake
// for a 03:00 missed while asleep.
func (s scheduler) install(o options, w io.Writer) error {
	if err := s.checkOS(); err != nil {
		return err
	}
	if strings.Contains(s.Executable, "go-build") {
		return fmt.Errorf("%s is a temporary go run binary; install agent-archive with go install and run schedule install from it", s.Executable)
	}
	logDir := filepath.Join(o.ArchiveDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	// launchd would create the log files with its own looser mode.
	for _, name := range []string{scheduleStdoutLog, scheduleStderrLog} {
		f, err := os.OpenFile(filepath.Join(logDir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	plist, err := s.plist(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.PlistPath), 0o755); err != nil {
		return err
	}
	tmp := s.PlistPath + ".tmp"
	if err := os.WriteFile(tmp, plist, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.PlistPath); err != nil {
		os.Remove(tmp)
		return err
	}
	// Unload an older version; this fails harmlessly when none is loaded.
	_, _ = s.Launchctl("bootout", s.domain()+"/"+scheduleLabel)
	if out, err := s.Launchctl("bootstrap", s.domain(), s.PlistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Fprintf(w, "installed %s\n", s.PlistPath)
	fmt.Fprintf(w, "agent-archive copy runs now, then daily at 03:00; output goes to %s\n", logDir)
	return nil
}

// remove unloads the LaunchAgent and deletes its plist.
func (s scheduler) remove(w io.Writer) error {
	if err := s.checkOS(); err != nil {
		return err
	}
	out, bootoutErr := s.Launchctl("bootout", s.domain()+"/"+scheduleLabel)
	err := os.Remove(s.PlistPath)
	switch {
	case errors.Is(err, fs.ErrNotExist) && bootoutErr != nil:
		fmt.Fprintln(w, "no schedule is installed")
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if bootoutErr != nil {
		fmt.Fprintf(w, "launchctl bootout: %v: %s (it was probably not loaded)\n", bootoutErr, out)
	}
	fmt.Fprintf(w, "removed %s\n", s.PlistPath)
	return nil
}

// plist renders the LaunchAgent. It passes --archive, --claude-dir, and
// --codex-dir only when they differ from the defaults, so directories set
// by environment variables reach launchd, which does not see the shell's.
func (s scheduler) plist(o options) ([]byte, error) {
	args := []string{s.Executable, "copy"}
	for _, f := range []struct{ name, value, dflt string }{
		{"--archive", o.ArchiveDir, defaultArchiveDir},
		{"--claude-dir", o.ClaudeDir, defaultClaudeDir},
		{"--codex-dir", o.CodexDir, defaultCodexDir},
	} {
		dflt, err := expandHome(f.dflt)
		if err != nil {
			return nil, err
		}
		if f.value != dflt {
			args = append(args, f.name, f.value)
		}
	}
	logDir := filepath.Join(o.ArchiveDir, "logs")
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", k) }
	str := func(indent, v string) {
		b.WriteString(indent + "<string>")
		xml.EscapeText(&b, []byte(v))
		b.WriteString("</string>\n")
	}
	key("Label")
	str("\t", scheduleLabel)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	key("StartCalendarInterval")
	b.WriteString("\t<dict>\n\t\t<key>Hour</key>\n\t\t<integer>3</integer>\n\t\t<key>Minute</key>\n\t\t<integer>0</integer>\n\t</dict>\n")
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Background")
	key("Umask")
	b.WriteString("\t<integer>63</integer>\n") // 0o077: owner-only files
	key("StandardOutPath")
	str("\t", filepath.Join(logDir, scheduleStdoutLog))
	key("StandardErrorPath")
	str("\t", filepath.Join(logDir, scheduleStderrLog))
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}
