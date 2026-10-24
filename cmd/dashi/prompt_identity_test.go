package main

import (
	"fmt"
	"github.com/deepnoodle-ai/dashi/workspace"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func TestQueuedPromptKeysKeepOriginalRequest(t *testing.T) {
	for _, key := range []tui.KeyEvent{{Rune: 'y'}, {Rune: 'a'}, {Rune: 'n'}, {Key: tui.KeyEnter}, {Rune: ' '}} {
		t.Run(fmt.Sprintf("key=%d/rune=%c", key.Key, key.Rune), func(t *testing.T) {
			s := newScreen("fixture")
			var queued tui.Event
			s.send = func(e tui.Event) { queued = e }
			original := &approvalPrompt{reply: make(chan Approval, 1)}
			replacement := &approvalPrompt{reply: make(chan Approval, 1)}
			s.HandleEvent(promptEvent{id: 1, approval: original})
			if !s.inputKey(key) {
				t.Fatal("dialog key not captured")
			}
			s.HandleEvent(endPromptEvent{id: 1})
			s.HandleEvent(promptEvent{id: 2, approval: replacement})
			s.HandleEvent(queued)
			select {
			case a := <-replacement.reply:
				t.Fatalf("old key answered replacement: %+v", a)
			default:
			}
			if replacement.denying {
				t.Fatal("old key opened replacement denial")
			}
		})
	}
}

func TestQuestionKeysAndSubmitCallbacksKeepOriginalRequest(t *testing.T) {
	s := newScreen("fixture")
	var queued tui.Event
	s.send = func(e tui.Event) { queued = e }
	original := &askPrompt{q: Question{Choices: []workspace.Choice{{Label: "first"}}}, reply: make(chan questionAnswer, 1), own: true}
	s.HandleEvent(promptEvent{id: 1, asking: original})
	submit := s.questionSubmit(1)
	s.inputKey(tui.KeyEvent{Key: tui.KeyEscape})
	s.HandleEvent(endPromptEvent{id: 1})
	replacement := &askPrompt{q: Question{Choices: []workspace.Choice{{Label: "second"}}}, reply: make(chan questionAnswer, 1), own: true}
	s.HandleEvent(promptEvent{id: 2, asking: replacement})
	submit("stale answer")
	s.HandleEvent(queued)
	select {
	case a := <-replacement.reply:
		t.Fatalf("old callback answered replacement: %+v", a)
	default:
	}
}

func TestDenialSubmitCallbackKeepsOriginalRequest(t *testing.T) {
	s := newScreen("fixture")
	s.send = func(tui.Event) {}
	s.HandleEvent(promptEvent{id: 1, approval: &approvalPrompt{denying: true, reply: make(chan Approval, 1)}})
	submit := s.denialSubmit(1)
	s.HandleEvent(endPromptEvent{id: 1})
	replacement := &approvalPrompt{denying: true, reply: make(chan Approval, 1)}
	s.HandleEvent(promptEvent{id: 2, approval: replacement})
	submit("stale reason")
	select {
	case a := <-replacement.reply:
		t.Fatalf("old denial answered replacement: %+v", a)
	default:
	}
}

func TestQueuedComposerKeyCannotAnswerNewDialog(t *testing.T) {
	s := newScreen("fixture")
	var queued tui.Event
	s.send = func(e tui.Event) { queued = e }
	s.inputKey(tui.KeyEvent{Key: tui.KeyPageDown})
	p := &approvalPrompt{reply: make(chan Approval, 1)}
	s.HandleEvent(promptEvent{id: 1, approval: p})
	s.HandleEvent(queued)
	if _, line := p.vp.Anchor(); line != 0 {
		t.Fatal("old composer key scrolled dialog")
	}
}

func TestRuntimeNavigationAfterPromptEnds(t *testing.T) {
	h := newTranscriptRuntime(t)
	h.scr.Emit(Block{Kind: KindAssistant, Text: "```text\n" + strings.Repeat("line\n", 100) + "```", Done: true})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "line") })
	h.scr.rt.SendEvent(promptEvent{id: 1, approval: &approvalPrompt{reply: make(chan Approval, 1)}})
	h.wait(func(s transcriptState) bool { return s.approval })
	h.scr.rt.SendEvent(endPromptEvent{id: 1})
	h.wait(func(s transcriptState) bool {
		return !s.approval && strings.Contains(s.text, "Ask Dashi to do anything")
	})
	h.typeText("retained draft")
	h.key(tui.KeyEvent{Key: tui.KeyPageUp})
	h.wait(func(s transcriptState) bool {
		return !s.follow && s.input == "retained draft" && strings.Contains(s.text, "Ctrl+End returns")
	})
	h.key(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	h.wait(func(s transcriptState) bool { return s.follow && s.input == "retained draft" })
}

func TestRuntimeToolExpansionInstructionAndLongOutput(t *testing.T) {
	h := newTranscriptRuntime(t)
	source := strings.Repeat("x", 150) + "TAIL_MARKER"
	h.scr.Emit(Block{Kind: KindTool, Name: "read_file", State: StateOK, Summary: "preview", Detail: source, Done: true})
	shown := h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Read a file") })
	if strings.Contains(shown.text, "TAIL_MARKER") || strings.Contains(shown.text, "preview") {
		t.Fatalf("collapsed row shows output:\n%s", shown.text)
	}
	row := transcriptRow(t, shown.text, "Read a file")
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 1, X: 12, Y: row})
	h.wait(func(s transcriptState) bool {
		return strings.Contains(strings.Join(strings.Fields(s.text), ""), "TAIL_MARKER")
	})
}
