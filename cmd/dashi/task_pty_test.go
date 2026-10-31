package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// This helper runs the production screen/controller in an actual 80x24 PTY.
func TestTaskPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_TASK_PTY")
	if mode == "" {
		t.Skip("real terminal evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires PTY")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	installTaskEffect(t, &effects, true)
	client := &taskFixtureClient{effect: true}
	selective := os.Getenv("DASHI_TASK_SELECTIVE") != ""
	var releaseChildren chan struct{}
	if selective {
		client.secondTask = true
		releaseChildren = make(chan struct{})
		client.childHold = releaseChildren
	}
	cfg := testConfig(t)
	cfg.Client = client
	cfg.Dir = os.Getenv("DASHI_TASK_WORKSPACE")
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	s.Emit(Block{Kind: KindNotice, Text: "Delegation acceptance: exact approval, waiting effect, details, verified finish, one result, reopen.", Done: true})
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				s.post(sessionEvidenceEvent{})
			}
		}
	}()
	released := false
	capture := func() {
		if selective && !released && len(a.eng.taskHost().tasks.List()) == 2 && a.ctrl.Mode() == ModeIdle {
			released = true
			close(releaseChildren)
			s.input = "unsent selective draft"
		}
		s.captureTaskEvidence(client, &effects)
	}
	if mode == "inline" {
		i := &sessionEvidenceInline{newInlineScreen(s, os.Stdout), capture}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&sessionEvidenceScreen{s, capture}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	s.cancelAttachmentSelections()
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if selective {
		tasks := a.eng.taskHost().tasks.List()
		if len(tasks) != 2 || effects.Load() != 0 {
			t.Fatal("selective control affected another child/effect", tasks, effects.Load())
		}
		for _, task := range tasks {
			snapshot, err := a.eng.inspectTask(t.Context(), task.ID)
			if err != nil || snapshot.View.Stop.Kind != dashi.StopCanceled {
				t.Fatal("selective stop not recorded", err)
			}
		}
	}
	after, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal attributes not restored", err)
	}
}
func (s *screen) captureTaskEvidence(client *taskFixtureClient, effects *atomic.Int32) {
	path := os.Getenv("DASHI_TASK_STATE")
	if path == "" {
		return
	}
	parent, child := client.counts()
	meta := s.eng.Session()
	state := struct {
		Input, Session, Picker, Approval, Question string
		SavedDraft, Dialog                         string
		Prompt                                     uint64
		ApprovalArguments                          string
		Mode                                       Mode
		ParentCalls, ChildCalls, Effects           int
		Blocks                                     []Block
		Tasks                                      []delegate.TaskView
		Details, Rows                              []string
	}{Input: s.input, SavedDraft: s.savedDraft, Prompt: s.promptID, Session: meta.ID, Mode: s.ctrl.Mode(), ParentCalls: parent, ChildCalls: child, Effects: int(effects.Load()), Blocks: s.main().tr.Blocks, Tasks: s.eng.taskHost().list(), Rows: attachmentRows(&s.attachments, s.ctrl.attachmentRoot)}
	if s.picker != nil {
		state.Picker = s.picker.title
		state.Details = s.picker.details
	}
	if s.dialogStatus != nil {
		state.Dialog = s.dialogStatus.text
	}
	if s.approval != nil {
		state.ApprovalArguments = s.approval.req.Arguments
		state.Approval = s.approval.req.Summary
	}
	if s.asking != nil {
		state.Question = s.asking.q.Prompt
	}
	data, _ := json.Marshal(state)
	tmp := filepath.Join(filepath.Dir(path), filepath.Base(path)+".tmp")
	if os.WriteFile(tmp, data, 0600) == nil {
		os.Rename(tmp, path)
	}
}
