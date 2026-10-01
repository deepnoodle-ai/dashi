package main

import (
	"fmt"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

type idleExitGuard struct {
	at      time.Time
	draft   string
	queued  int
	pending bool
}

func (g *idleExitGuard) reset() { *g = idleExitGuard{} }
func (g *idleExitGuard) confirm(now time.Time, draft string, queued int, pending bool) bool {
	if !g.at.IsZero() && now.Sub(g.at) <= 2*time.Second && now.Sub(g.at) >= 0 && g.draft == draft && g.queued == queued && g.pending == pending {
		g.reset()
		return true
	}
	*g = idleExitGuard{at: now, draft: draft, queued: queued, pending: pending}
	return false
}
func (s *screen) protectIdleExit() bool {
	queued, pending := s.eng.Queued(), s.eng.HasPending()
	signature := fmt.Sprintf("%s|attachment-revision:%d", s.input, s.attachments.revision)
	if s.exitGuard.confirm(time.Now(), signature, queued, pending) {
		// The notice already disclosed this queue loss. Do not warn a second time.
		s.ctrl.mu.Lock()
		s.ctrl.dropping = "Ctrl-C"
		s.ctrl.mu.Unlock()
		return false
	}
	notice := ""
	if queued > 0 {
		notice += fmt.Sprintf("%d queued %s will be lost. ", queued, plural(queued, "message"))
	}
	if s.input != "" || len(s.attachments.entries) > 0 {
		notice += "The unsent draft will be lost. "
	}
	if pending {
		notice += "The pending prompt is included in that queue count. "
	}
	if queued > 0 || s.input != "" {
		notice += "This input is held only in memory. "
	}
	notice += "Press Ctrl-C again within 2 seconds to exit."
	s.Emit(warnBlock("%s", notice))
	return true
}
func (s *screen) recordSubmittedInput(value string) {
	s.exitGuard.reset()
	s.history = historyAppend(s.history, value)
	if s.historyStore != nil {
		s.historyStore.append(value)
	}
}
func (s *screen) editDraft() []tui.Cmd {
	s.exitGuard.reset()
	if s.promptActive {
		return nil
	}
	if s.ctrl.Mode() != ModeIdle {
		s.Emit(warnBlock("External editing is available after the turn ends; press Ctrl+G again then."))
		return nil
	}
	if s.handoff == nil {
		s.Emit(warnBlock("External editing requires supported terminal handoff."))
		return nil
	}
	original := s.input
	path, run, cleanup, err := prepareEditor(s.dir, original, selectedEditor())
	if err != nil {
		s.Emit(warnBlock("External editor: %v; original draft kept.", err))
		return nil
	}
	defer cleanup()
	operationErr, restoreErr := s.handoff(run)
	if restoreErr != nil {
		// Wonton stops admission and returns this error from Run for stderr.
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	}
	if operationErr != nil {
		s.Emit(warnBlock("External editor: %v; original draft kept.", operationErr))
		return []tui.Cmd{tui.Focus(inputID)}
	}
	edited, err := readEditedDraft(path)
	if err != nil {
		s.Emit(warnBlock("External editor: %v; original draft kept.", err))
	} else {
		if s.attachments.frozenBy != "" {
			s.Emit(warnBlock("The attachment request is pending; editor return cannot change it."))
		} else {
			s.input = edited
			s.attachments.setText(edited)
			s.updateAttachmentUnsaved()
		}
	}
	return []tui.Cmd{tui.Focus(inputID)}
}
