package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

func TestInputHistoryRelaunchExactPrivateAndConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dashi", "history.json")
	var mu sync.Mutex
	var warnings []error
	warn := func(err error) { mu.Lock(); warnings = append(warnings, err); mu.Unlock() }
	first, entries := openInputHistory(path, warn)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
	first.append(" \t世界\nsecond line  ")
	first.append(" \t世界\nsecond line  ")
	first.append("  ")
	first.close()
	first.close()
	second, entries := openInputHistory(path, warn)
	if !reflect.DeepEqual(entries, []string{" \t世界\nsecond line  "}) {
		t.Fatal(entries)
	}
	third, _ := openInputHistory(path, warn)
	// Both have stale startup snapshots; sequential persistence still keeps both additions.
	second.append("from second")
	second.close()
	third.append("from third")
	third.close()
	got, _, err := readInputHistory(path)
	if err != nil || !reflect.DeepEqual(got, []string{" \t世界\nsecond line  ", "from second", "from third"}) {
		t.Fatalf("%q %v", got, err)
	}
	for file, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600, path + ".lock": 0600} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", file, info, err)
		}
	}
	if len(warnings) > 0 {
		t.Fatal(warnings)
	}
}
func TestInputHistoryFailureKeepsPreviousFileAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	initial := []byte(`{"entries":["previous"]}`)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockPreferences(lock); err != nil {
		t.Fatal(err)
	}
	var warned int
	h := &inputHistory{path: path, pending: []string{"new"}, warn: func(error) { warned++ }}
	h.flush()
	got, _ := os.ReadFile(path)
	if string(got) != string(initial) || len(h.pending) != 1 || warned != 1 {
		t.Fatalf("%s %q %d", got, h.pending, warned)
	}
	lock.Close()
	h.flush()
	entries, _, err := readInputHistory(path)
	if err != nil || !reflect.DeepEqual(entries, []string{"previous", "new"}) || len(h.pending) != 0 {
		t.Fatalf("%q %v", entries, err)
	}
	// A valid-file read failure must not turn into empty-history overwrite.
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("keep"), 0600)
	os.Remove(path)
	os.Symlink(target, path)
	h.pending = []string{"later"}
	h.flush()
	secret, _ := os.ReadFile(target)
	if string(secret) != "keep" || len(h.pending) != 1 {
		t.Fatal("symlink overwrote target")
	}
}
func TestInputHistoryCorruptLoadAndBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte("broken PRIVATE CONTENT"), 0600)
	var warnings []error
	h, entries := openInputHistory(path, func(err error) { warnings = append(warnings, err) })
	if len(entries) != 0 || len(warnings) != 1 || strings.Contains(warnings[0].Error(), "PRIVATE") {
		t.Fatal(warnings)
	}
	h.append("fresh")
	h.close()
	entries, _, err := readInputHistory(path)
	if err != nil || !reflect.DeepEqual(entries, []string{"fresh"}) {
		t.Fatal(entries, err)
	}
	for i := range 1100 {
		entries = historyAppend(entries, string(rune('a'+i)))
	}
	if len(entries) != historyLimit {
		t.Fatal(len(entries))
	}
}
func TestCompletionSuffixAndWorkspaceBoundary(t *testing.T) {
	h := newScreenHarness(t, 0)
	for _, path := range []string{"世界 with spaces.txt", "other.txt", "node_modules/hidden.txt", ".git/config"} {
		full := filepath.Join(h.dir, path)
		os.MkdirAll(filepath.Dir(full), 0700)
		os.WriteFile(full, []byte("not read"), 0600)
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	os.WriteFile(outside, []byte("private"), 0600)
	os.Symlink(outside, filepath.Join(h.dir, "escape.txt"))
	h.scr.pathCompletion = indexWorkspace(h.dir)
	got := h.scr.completeDraft("Keep this\n@世界 with")
	if !reflect.DeepEqual(got, []string{"Keep this\n@世界 with spaces.txt"}) {
		t.Fatal(got)
	}
	got = h.scr.completeDraft("/queue inspect @世界 with")
	if !reflect.DeepEqual(got, []string{"/queue inspect @世界 with spaces.txt"}) {
		t.Fatal(got)
	}
	got = h.scr.completeDraft("  /he arg\nnext line")
	if !reflect.DeepEqual(got, []string{"  /help arg\nnext line", "  /attachments arg\nnext line"}) {
		t.Fatal(got)
	}
	for _, entry := range h.scr.pathCompletion.paths {
		if strings.Contains(entry, "hidden") || strings.Contains(entry, ".git") || strings.Contains(entry, "escape") {
			t.Fatal(entry)
		}
	}
	if got = h.scr.completeDraft("@../"); len(got) != 0 {
		t.Fatal(got)
	}
	rank := rankCompletion("/m", []string{"/max-tokens", "/model", "/compact", "/m"})
	if !reflect.DeepEqual(rank, []string{"/m", "/max-tokens", "/model", "/compact"}) {
		t.Fatal(rank)
	}
}
func TestEditorArgumentsAndDraftReturnPaths(t *testing.T) {
	args, err := editorArgv(`"/path with space/editor" --wait 'literal $HOME' "$(literal)"`)
	if err != nil || !reflect.DeepEqual(args, []string{"/path with space/editor", "--wait", "literal $HOME", "$(literal)"}) {
		t.Fatal(args, err)
	}
	for _, bad := range []string{`vi | other`, `vi "unfinished`, ""} {
		if _, err := editorArgv(bad); err == nil {
			t.Fatal(bad)
		}
	}
	for _, scenario := range []struct {
		name, edited       string
		operation, restore error
		want               string
	}{
		{name: "save", edited: "世界\nchanged", want: "世界\nchanged"}, {name: "cancel", edited: "original", want: "original"}, {name: "clear", want: ""},
		{name: "invalid", edited: string([]byte{255}), want: "original"}, {name: "failed", edited: "changed", operation: errors.New("editor failed"), want: "original"},
		{name: "unsafe", edited: "changed", restore: errors.New("restore failed"), want: "original"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newScreenHarness(t, 0)
			editor := filepath.Join(t.TempDir(), "editor")
			script := "#!/bin/sh\nprintf '%s' " + shellTestQuote(scenario.edited) + " > \"$1\"\n"
			os.WriteFile(editor, []byte(script), 0700)
			t.Setenv("VISUAL", editor)
			h.scr.input = "original"
			h.scr.handoff = func(fn func() error) (error, error) {
				if err := fn(); err != nil {
					t.Fatal(err)
				}
				return scenario.operation, scenario.restore
			}
			h.scr.editDraft()
			if h.scr.input != scenario.want || len(h.scr.history) != 0 || h.client.sends != 0 {
				t.Fatalf("input=%q", h.scr.input)
			}
			if h.scr.quitting != (scenario.restore != nil) {
				t.Fatal("restoration policy")
			}
		})
	}
}
func shellTestQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func TestExitGuardExpiryDraftAndQueueRevision(t *testing.T) {
	var guard idleExitGuard
	now := time.Now()
	if guard.confirm(now, "draft", 1, true) || guard.confirm(now.Add(time.Second), "changed", 1, true) {
		t.Fatal("premature confirmation")
	}
	if guard.confirm(now.Add(2*time.Second), "changed", 2, true) {
		t.Fatal("queue change did not reset")
	}
	if guard.confirm(now.Add(5*time.Second), "changed", 2, true) {
		t.Fatal("expired confirmation")
	}
	if !guard.confirm(now.Add(6*time.Second), "changed", 2, true) {
		t.Fatal("repeat not accepted")
	}
	guard.reset()
	if guard.confirm(now, "draft", 0, false) {
		t.Fatal("reset")
	}
}
func TestComposerRuntimeCompletionEscapeAndCtrlD(t *testing.T) {
	h := newTranscriptRuntime(t)
	h.typeText("/m")
	h.key(tui.KeyEvent{Key: tui.KeyTab})
	h.wait(func(s transcriptState) bool { return s.input != "/m" })
	h.key(tui.KeyEvent{Key: tui.KeyTab, Shift: true})
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.input == "/m" })
	h.key(tui.KeyEvent{Key: tui.KeyHome})
	h.key(tui.KeyEvent{Key: tui.KeyCtrlD})
	h.wait(func(s transcriptState) bool { return s.input == "m" })
	h.key(tui.KeyEvent{Key: tui.KeyEnd})
	h.key(tui.KeyEvent{Key: tui.KeyCtrlD})
	h.wait(func(s transcriptState) bool { return s.input == "m" })
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "unsent draft will be lost") })
	h.typeText(" edited")
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool { return s.input == "m edited" })
}
func TestComposerRuntimeHistoryPreservesRawUnicodeDraft(t *testing.T) {
	h := newTranscriptRuntime(t, say("accepted"))
	raw := " \t世界\nsecond line  "
	h.scr.rt.SendEvent(tui.KeyEvent{Paste: raw})
	h.wait(func(s transcriptState) bool { return s.input == raw })
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "accepted") })
	h.typeText("unfinished")
	h.key(tui.KeyEvent{Key: tui.KeyArrowUp})
	h.wait(func(s transcriptState) bool { return s.input == raw })
	// Recalled multiline input must reach its final visual row before history Down.
	h.key(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	h.key(tui.KeyEvent{Key: tui.KeyArrowDown})
	h.wait(func(s transcriptState) bool { return s.input == "unfinished" })
}
func TestComposerRunningHelpUnknownAndEditorRetry(t *testing.T) {
	started := make(chan struct{})
	h := newTranscriptRuntime(t, blockUntilDone(started))
	h.typeText("start")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	<-started
	h.typeText("/unknown")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Unknown command /unknown") })
	h.typeText("/?")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Ctrl+D") })
	h.typeText("世界 draft")
	h.key(tui.KeyEvent{Key: tui.KeyCtrlG})
	h.wait(func(s transcriptState) bool {
		return s.input == "世界 draft" && strings.Contains(s.text, "press Ctrl+G again then")
	})
	if h.app.eng.Queued() != 0 {
		t.Fatal("running editor or slash command queued input")
	}
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle })
}

func TestComposerCommandHelpMatchesAvailableHandlers(t *testing.T) {
	h := newHarness(t)
	text := h.app.ctrl.commandHelp()
	for _, want := range []string{"/help", "/?", "/quit", "/q", "/exit", "/tasks", "/task", "/task-stop", "Enter / Shift+Enter", "Up / Down", "Tab/Down", "Shift+Tab/Up", "Escape", "Ctrl+G", "Ctrl+C", "Ctrl+D", "pending/queued-loss", "supported fullscreen/inline terminal handoff"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help missing %s", want)
		}
	}
	for _, unavailable := range []string{"/copy", "/mouse", "/scrollback", "/attach"} {
		if strings.Contains(text, unavailable) {
			t.Fatalf("advertised unavailable %s", unavailable)
		}
	}
	h.input("/?")
	if !strings.Contains(h.ui.text(), "Commands:") {
		t.Fatal("help alias")
	}
}

func TestComposerRuntimeNoMatchesAndHistoryFailure(t *testing.T) {
	h := newTranscriptRuntime(t)
	h.typeText("/zzzzzz")
	h.key(tui.KeyEvent{Key: tui.KeyTab})
	h.wait(func(s transcriptState) bool {
		return s.input == "/zzzzzz" && strings.Contains(s.text, "No completion exists")
	})
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte("invalid"), 0600)
	history, _ := openInputHistory(path, func(err error) { h.scr.Emit(warnBlock("%v", err)) })
	history.close()
	h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "input history is invalid") && s.input == "/zzzzzz"
	})
}

func TestComposerPrintCommandsNeverStartProviderWork(t *testing.T) {
	for _, scenario := range []struct {
		input  string
		code   int
		output string
	}{{"/?", 0, "Commands:"}, {"/q", 0, ""}, {"/exit", 0, ""}, {"/unknown", 2, "unknown command"}, {"/model other", 2, "requires --plain"}} {
		t.Run(scenario.input, func(t *testing.T) {
			cfg := testConfig(t)
			var out, diag bytes.Buffer
			code := runPrint(t.Context(), cfg, Options{Print: scenario.input, PrintSet: true}, &out, &diag)
			if code != scenario.code || !strings.Contains(out.String()+diag.String(), scenario.output) || cfg.Client.(*fakeClient).sends != 0 {
				t.Fatalf("code=%d out=%s diag=%s", code, &out, &diag)
			}
		})
	}
}

func TestComposerPrintSlashControlsStayPrintable(t *testing.T) {
	cfg := testConfig(t)
	var out, diag bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Print: "/unknown\x1b]52;c;Y2xpcGJvYXJk\x07\x1b[2J", PrintSet: true}, &out, &diag)
	if code != 2 || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.ContainsAny(out.String()+diag.String(), "\x1b\x07") {
		t.Fatalf("terminal controls reached direct output: %q", diag.String())
	}
	if !strings.Contains(diag.String(), "unknown command") {
		t.Fatal(diag.String())
	}
}

func TestInputHistoryLargeEntriesSurviveBoundedSaveAndRelaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte(`{"entries":["previous"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, letter := range []string{"a", "b", "c", "d", "e"} {
		entries = append(entries, strings.Repeat(letter, 14<<20)+"\n世界\t  ")
	}
	var warning error
	history := &inputHistory{path: path, pending: entries, warn: func(err error) { warning = err }}
	history.flush()
	if warning != nil || len(history.pending) != 0 {
		t.Fatalf("save failed: %v", warning)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > historyByteLimit {
		t.Fatalf("unloadable file: %v %v", info, err)
	}
	reopened, recalled := openInputHistory(path, func(err error) { warning = err })
	if warning != nil || !reflect.DeepEqual(recalled, entries[1:]) {
		reopened.close()
		t.Fatalf("relaunch lost exact latest entries: count=%d warning=%v", len(recalled), warning)
	}
	reopened.append("after relaunch")
	reopened.close()
	loaded, _, err := readInputHistory(path)
	if err != nil || len(loaded) != 5 || loaded[4] != "after relaunch" || !reflect.DeepEqual(loaded[:4], entries[1:]) {
		t.Fatalf("subsequent save failed: count=%d err=%v", len(loaded), err)
	}
}

func TestInputHistoryOversizedEncodedEntryPreservesPreviousFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	previous := []byte(`{"entries":["previous exact text"]}`)
	if err := os.WriteFile(path, previous, 0600); err != nil {
		t.Fatal(err)
	}
	// This 11 MiB draft fits the editor's raw-text limit. JSON HTML escaping
	// requires more than 64 MiB, so encoded size must decide acceptance.
	oversized := strings.Repeat("&", 11<<20)
	var warnings []error
	history := &inputHistory{path: path, warn: func(err error) { warnings = append(warnings, err) }}
	composer := &screen{historyStore: history}
	composer.recordSubmittedInput(oversized)
	history.flush()
	if len(warnings) != 1 || !errors.Is(warnings[0], errHistoryEntryTooLarge) || len(history.pending) != 0 || !reflect.DeepEqual(composer.history, []string{oversized}) {
		t.Fatalf("permanent size rejection damaged recall or retries: warnings=%v", warnings)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, previous) {
		t.Fatal("failed save replaced previous history")
	}
	// Unsupported text remains in local recall, but does not poison later saves.
	composer.recordSubmittedInput("later supported input")
	history.flush()
	if len(warnings) != 1 || len(history.pending) != 0 || !reflect.DeepEqual(composer.history, []string{oversized, "later supported input"}) {
		t.Fatal("rejected input poisoned supported save")
	}
	reopened, entries := openInputHistory(path, func(err error) { t.Error(err) })
	reopened.close()
	if !reflect.DeepEqual(entries, []string{"previous exact text", "later supported input"}) {
		t.Fatal(entries)
	}
}
