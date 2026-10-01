package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

func dialogTranscriptText(s *screen) string {
	var text strings.Builder
	for _, b := range s.tr.Blocks {
		text.WriteString(b.Text)
		text.WriteByte('\n')
	}
	return text.String()
}

func TestNativeStatusKeepsApprovalAndAuthorityPending(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newScreenHarness(t, 0,
		callTool("c1", "write_note", `{"text":"one"}`), say("Noted."),
		callTool("c2", "write_note", `{"text":"two"}`), say("Done."))
	h.submit("write")
	h.waitFor(func() bool { return h.scr.approval != nil })
	var pending *approvalPrompt
	h.check(func() bool { pending = h.scr.approval; h.scr.savedDraft = "unsent draft"; return true })
	before := inspectionFiles(t, h.store.Root())
	for _, r := range "/status" {
		h.key(tui.KeyEvent{Rune: r})
		if !h.check(func() bool { return h.scr.approval == pending && len(pending.reply) == 0 }) {
			t.Fatalf("rune %q answered the approval", r)
		}
	}
	h.mu.Lock()
	h.scr.dialogStatusSubmit(h.scr.dialogStatus)("/status")
	h.mu.Unlock()
	h.waitFor(func() bool {
		return strings.Contains(dialogTranscriptText(h.scr), "this process waits for an approval or answer")
	})
	if !reflect.DeepEqual(before, inspectionFiles(t, h.store.Root())) || runs.Load() != 0 || h.app.ctrl.Mode() != ModeApproval {
		t.Fatal("status changed facts, authority, or live mode")
	}
	for _, r := range "/any" {
		h.key(tui.KeyEvent{Rune: r})
	}
	h.mu.Lock()
	h.scr.dialogStatusSubmit(h.scr.dialogStatus)("/any")
	h.mu.Unlock()
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	if !h.check(func() bool {
		return h.scr.approval == pending && len(pending.reply) == 0 && h.scr.savedDraft == "unsent draft"
	}) {
		t.Fatal("unknown slash draft changed the approval or main draft")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.key(tui.KeyEvent{Rune: 'y'})
	h.app.ctrl.Wait()
	if runs.Load() != 1 || !h.check(func() bool { return h.scr.input == "unsent draft" }) {
		t.Fatal("allow-once or draft restoration changed")
	}
	h.submit("again")
	h.waitFor(func() bool { return h.scr.approval != nil })
	if runs.Load() != 1 {
		t.Fatal("status remembered permission")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.app.ctrl.Wait()
}

func TestNativeQuestionStatusSubmissionPreservesExactPrompt(t *testing.T) {
	h := newScreenHarness(t, 0)
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "choices", true: "own-words"}[own], func(t *testing.T) {
			h.mu.Lock()
			h.scr.input = "unsent main draft"
			p := &askPrompt{q: Question{Prompt: "Which?", Multiple: true, Choices: []workspace.Choice{{Label: "one"}, {Label: "two"}}}, reply: make(chan questionAnswer, 1), own: own, text: "answer draft", selected: map[int]bool{1: true}, index: 1}
			id := h.scr.promptID + 1
			h.scr.HandleEvent(promptEvent{id: id, asking: p})
			h.app.ctrl.setMode(ModeApproval)
			before := inspectionFiles(t, h.store.Root())
			if own {
				h.scr.dialogTextKey(id)(tui.KeyEvent{Key: tui.KeyCtrlS})
				h.scr.dialogStatusSubmit(h.scr.dialogStatus)("/status ../invalid")
			} else {
				h.scr.HandleEvent(tui.KeyEvent{Rune: '/'})
				h.scr.dialogStatusSubmit(h.scr.dialogStatus)("/status ../invalid")
			}
			h.mu.Unlock()
			h.app.ctrl.Wait()
			h.waitFor(func() bool { return strings.Contains(dialogTranscriptText(h.scr), "status unavailable") })
			h.key(tui.KeyEvent{Key: tui.KeyEscape})
			h.mu.Lock()
			if h.scr.asking != p || len(p.reply) != 0 || p.text != "answer draft" || p.own != own || !p.selected[1] || p.index != 1 || h.scr.savedDraft != "unsent main draft" || h.app.ctrl.Mode() != ModeApproval {
				t.Fatal("inspection or cancellation changed the question target/drafts/choices")
			}
			if !reflect.DeepEqual(before, inspectionFiles(t, h.store.Root())) {
				t.Fatal("inspection wrote records")
			}
			if own {
				h.scr.questionSubmit(id)("/statusful intentional answer")
				if got := <-p.reply; got.text != "/statusful intentional answer" {
					t.Fatal(got)
				}
			} else {
				h.scr.HandleEvent(tui.KeyEvent{Key: tui.KeyTab})
				h.scr.HandleEvent(tui.KeyEvent{Key: tui.KeyEnter})
				if got := <-p.reply; !reflect.DeepEqual(got.choices, []int{1}) {
					t.Fatal(got)
				}
			}
			h.app.ctrl.setMode(ModeIdle)
			h.mu.Unlock()
		})
	}
}

func TestNativeStatusRetainedCallbacksCannotAffectReplacementPrompt(t *testing.T) {
	h := newScreenHarness(t, 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	p := &approvalPrompt{reply: make(chan Approval, 1), denying: true, reason: "denial draft"}
	h.scr.HandleEvent(promptEvent{id: 1, approval: p})
	h.scr.dialogTextKey(1)(tui.KeyEvent{Key: tui.KeyCtrlS})
	old := h.scr.dialogStatus
	submit, key := h.scr.dialogStatusSubmit(old), h.scr.dialogStatusKeyHook(old)
	open := h.scr.dialogTextKey(1)
	question := &askPrompt{reply: make(chan questionAnswer, 1), own: true, text: "new answer"}
	h.scr.HandleEvent(promptEvent{id: 2, asking: question})
	submit("/status")
	open(tui.KeyEvent{Key: tui.KeyCtrlS})
	key(tui.KeyEvent{Key: tui.KeyEscape})
	h.scr.HandleEvent(dialogStatusKeyEvent{input: old, key: tui.KeyEvent{Rune: 'a'}})
	h.scr.denialSubmit(1)("/status")
	if h.scr.asking != question || h.scr.dialogStatus != nil || len(p.reply) != 0 || len(question.reply) != 0 || p.reason != "denial draft" {
		t.Fatal("stale inspection callback affected the replacement prompt")
	}
	h.scr.beginDialogStatus("/status")
	old = h.scr.dialogStatus
	h.scr.generation++
	h.scr.dialogStatusSubmit(old)("/status")
	if h.scr.dialogStatus != old || old.text != "/status" {
		t.Fatal("stale screen generation dispatched inspection")
	}
	h.scr.clearPrompts()
}

func TestRuntimeNativeStatusLeavesApprovalPending(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newTranscriptRuntimeConfig(t, inspectionTestConfig(t,
		callTool("c1", "write_note", `{"text":"one"}`), say("Done.")))
	h.typeText("write")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.approval && s.mode == ModeApproval })
	h.typeText("/status")
	h.wait(func(s transcriptState) bool { return s.approval && strings.Contains(s.text, "Inspect: /status") })
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.approval && strings.Contains(s.text, "still pending") && strings.Contains(s.text, "waits for an approval or answer")
	})
	h.key(tui.KeyEvent{Key: tui.KeyHome, Ctrl: true})
	h.wait(func(s transcriptState) bool { return s.approval && strings.Contains(s.text, "Recorded snapshot") })
	if runs.Load() != 0 {
		t.Fatal("typing or reading status ran the tool")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.approval && strings.Contains(s.text, "allow once") })
	h.key(tui.KeyEvent{Rune: 'y'})
	h.app.ctrl.Wait()
	if runs.Load() != 1 {
		t.Fatal("intentional allow once stopped working")
	}
}

func TestRuntimeNativeQuestionStatusRecordsOnlyIntentionalAnswer(t *testing.T) {
	h := newTranscriptRuntimeConfig(t, inspectionTestConfig(t,
		callTool("question", workspace.ToolAskUser, `{"question":"Describe the fix"}`), say("Accepted.")))
	h.typeText("ask")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.question && s.own })
	h.typeText("/status")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.question && s.own && s.mode == ModeApproval && strings.Contains(s.text, "Question still pending") && strings.Contains(s.text, "waits for an approval or answer")
	})
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "question" && step.Tool.Outcome != nil {
			t.Fatal("status recorded an answer")
		}
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.question && s.own && strings.Contains(s.text, "Answer:") })
	h.typeText("the deliberate answer")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	var answer string
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "question" {
			answer = outputText(step.Tool.Outcome)
		}
	}
	if answer != "the deliberate answer" {
		t.Fatalf("unexpected recorded answer: %s", answer)
	}
}

func TestRuntimeStatusShortcutPreservesOwnWordsDraft(t *testing.T) {
	h := newTranscriptRuntimeConfig(t, inspectionTestConfig(t,
		callTool("question", workspace.ToolAskUser, `{"question":"Describe the fix"}`), say("Accepted.")))
	h.typeText("ask")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.question && s.own && strings.Contains(s.text, "Ctrl-S inspects")
	})
	const draft = "回答 /tmp/path\n第二行 ✓"
	h.key(tui.KeyEvent{Paste: draft})
	h.wait(func(s transcriptState) bool { return s.question && strings.Contains(s.text, "第二行 ✓") })
	pending := h.scr.asking
	if pending.text != draft {
		t.Fatalf("keyboard did not bind the exact draft: %q", pending.text)
	}
	before := inspectionFiles(t, h.app.ctrl.store.Root())
	// Queue both keys without waiting for focus or a rendered inspection field.
	h.input.events <- tui.KeyEvent{Key: tui.KeyCtrlS, Ctrl: true}
	h.input.events <- tui.KeyEvent{Key: tui.KeyEnter}
	h.wait(func(s transcriptState) bool {
		return s.question && s.mode == ModeApproval && strings.Contains(s.text, "waits for an approval or answer")
	})
	// Ordinary slash/path text edits only the separate status field.
	h.key(tui.KeyEvent{Paste: "/tmp/inspection-only"})
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Use /status [ID]") })
	if pending.text != draft || h.scr.asking != pending || len(pending.reply) != 0 ||
		!reflect.DeepEqual(before, inspectionFiles(t, h.app.ctrl.store.Root())) {
		t.Fatal("inspection edited, answered, or recorded the pending question")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool {
		return s.question && s.own && strings.Contains(s.text, "Answer:") && strings.Contains(s.text, "第二行 ✓")
	})
	if h.scr.asking != pending || pending.text != draft {
		t.Fatalf("Escape did not restore the exact answer: %q", pending.text)
	}
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	var answer string
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "question" {
			answer = outputText(step.Tool.Outcome)
		}
	}
	if answer != draft {
		t.Fatalf("intentional answer changed: %q", answer)
	}
}

func TestRuntimeStatusShortcutPreservesDenialDraft(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newTranscriptRuntimeConfig(t, inspectionTestConfig(t,
		callTool("write", "write_note", `{"text":"one"}`), say("Denied.")))
	h.typeText("write")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.approval && s.mode == ModeApproval })
	h.key(tui.KeyEvent{Rune: 'n'})
	h.wait(func(s transcriptState) bool { return s.denying && strings.Contains(s.text, "Ctrl-S inspects") })
	const draft = "拒否 /tmp/path /statusful ✓"
	h.key(tui.KeyEvent{Paste: draft})
	h.wait(func(s transcriptState) bool { return s.denying && strings.Contains(s.text, draft) })
	pending := h.scr.approval
	if pending.reason != draft {
		t.Fatalf("keyboard did not bind the exact denial: %q", pending.reason)
	}
	before := inspectionFiles(t, h.app.ctrl.store.Root())
	// Queue both keys without waiting for focus or a rendered inspection field.
	h.input.events <- tui.KeyEvent{Key: tui.KeyCtrlS, Ctrl: true}
	h.input.events <- tui.KeyEvent{Key: tui.KeyEnter}
	h.wait(func(s transcriptState) bool {
		return s.approval && s.mode == ModeApproval && strings.Contains(s.text, "waits for an approval or answer")
	})
	if pending.reason != draft || h.scr.approval != pending || len(pending.reply) != 0 || runs.Load() != 0 ||
		!reflect.DeepEqual(before, inspectionFiles(t, h.app.ctrl.store.Root())) {
		t.Fatal("inspection edited, answered, or authorized the pending approval")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool {
		return s.denying && strings.Contains(s.text, "Reason:") && strings.Contains(s.text, draft)
	})
	if pending.reason != draft || h.scr.approval != pending {
		t.Fatalf("Escape did not restore the exact denial: %q", pending.reason)
	}
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	var reason string
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "write" {
			reason = step.Tool.Reason
		}
	}
	if reason != draft || runs.Load() != 0 {
		t.Fatalf("intentional denial changed: %q; runs=%d", reason, runs.Load())
	}
}
