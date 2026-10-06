package main

import (
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/wonton/tui"
)

const (
	attachmentPasteBytes = 64 << 10
	attachmentPastePaths = 64
)

// droppedPaths recognizes only a complete path-only segment. It performs no
// filesystem work and retains at most 64 paths, even when the run overflows.
func droppedPaths(text string) (paths []string, recognized, overflow bool) {
	if len(text) > attachmentPasteBytes {
		return nil, false, false
	}
	var token strings.Builder
	var quote rune
	escaped, started := false, false
	count := 0
	flush := func() bool {
		path := token.String()
		if !localDropPath(path) {
			return false
		}
		count++
		if count <= attachmentPastePaths {
			paths = append(paths, path)
		}
		token.Reset()
		started = false
		return true
	}
	for _, r := range text {
		switch {
		case escaped:
			token.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, started = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				token.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case unicode.IsSpace(r):
			if started && !flush() {
				return nil, false, false
			}
		default:
			token.WriteRune(r)
			started = true
		}
	}
	if quote != 0 || escaped || (started && !flush()) || count == 0 {
		return nil, false, false
	}
	return paths, true, count > attachmentPastePaths
}

func localDropPath(path string) bool {
	if strings.HasPrefix(path, "file://") {
		u, err := url.Parse(path)
		return err == nil && (u.Host == "" || u.Host == "localhost") && filepath.IsAbs(u.Path) && u.RawQuery == "" && u.Fragment == ""
	}
	return filepath.IsAbs(path) || strings.HasPrefix(path, "~/")
}

// Retained callbacks cannot select into a replacement prompt. Native Wonton
// owns insertion and cursor state; an empty modification consumes only paste.
func (s *screen) inputPaste(id uint64) tui.PasteHandler {
	return func(info tui.PasteInfo) (tui.PasteHandlerDecision, string) {
		if s.promptActive || s.promptID != id {
			return tui.PasteAccept, ""
		}
		// Large text bypasses path detection without a file warning. Commands
		// stay editable, even when their spelling is also an absolute path.
		if len(info.Content) > attachmentPasteBytes || s.pastedCommand(info.Content) {
			return tui.PasteAccept, ""
		}
		paths, recognized, overflow := droppedPaths(info.Content)
		if !recognized {
			return tui.PasteAccept, ""
		}
		if s.ctrl.Mode() != ModeIdle || s.attachments.frozenBy != "" {
			s.Emit(warnBlock("Attachment changes need a fresh idle submission; the draft was kept."))
			return tui.PasteModified, ""
		}
		if overflow || len(s.attachments.entries)+len(paths) > attachmentPastePaths {
			s.Emit(warnBlock("Files were not selected. Drop at most 64 paths; use /attachments and /detach N to make room within 64 draft entries."))
			return tui.PasteModified, ""
		}
		if err := s.attachments.setText(s.input); err != nil {
			s.Emit(warnBlock("%v", err))
			return tui.PasteModified, ""
		}
		s.exitGuard.reset()
		s.stageAttachments(paths, true)
		s.updateAttachmentUnsaved()
		return tui.PasteModified, ""
	}
}

func (s *screen) pastedCommand(text string) bool {
	name, _ := splitCommand(strings.TrimSpace(text))
	if !strings.HasPrefix(name, "/") {
		return false
	}
	// Refresh synchronously for paste: the highlighting cache may still be
	// empty on the first input after startup.
	for _, command := range s.ctrl.commandNames() {
		if strings.EqualFold(command, name) {
			return true
		}
	}
	return false
}
