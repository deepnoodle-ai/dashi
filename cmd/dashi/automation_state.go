package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	dashisession "github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// Private execution decisions never cross the stdout boundary.
type automationAttention struct {
	TurnID, TaskID, CallID string
	Stop                   dashi.Stop
	Next                   dashi.Next
	Ready                  bool
}
type automationSnapshot struct {
	latest, answer dashi.TurnView
	request        int
	affected       []automationAttention
	missingNotice  bool
}

func automationRank(kind dashi.StopKind) int {
	switch kind {
	case dashi.StopCompleted:
		return 0
	case dashi.StopFailed:
		return 1
	case dashi.StopLimited:
		return 3
	case dashi.StopRefused:
		return 4
	case dashi.StopCanceled:
		return 5
	default:
		return 2
	}
}
func automationCode(signal int, failed, preflight bool, affected []automationAttention) int {
	if signal != 0 {
		return signal
	}
	if failed {
		return 1
	}
	if preflight {
		return 2
	}
	best := 0
	for _, a := range affected {
		if rank := automationRank(a.Stop.Kind); rank > 0 && (best == 0 || rank < best) {
			best = rank
		}
	}
	switch {
	case best == 1:
		return 1
	case best == 2:
		return 3
	case best > 2:
		return 4
	default:
		return 0
	}
}
func printAutomationAttention(stderr io.Writer, affected []automationAttention, original string) {
	affected = slices.Clone(affected)
	slices.SortFunc(affected, func(a, b automationAttention) int {
		if d := automationRank(a.Stop.Kind) - automationRank(b.Stop.Kind); d != 0 {
			return d
		}
		if (a.TurnID == original) != (b.TurnID == original) {
			if a.TurnID == original {
				return -1
			}
			return 1
		}
		if d := strings.Compare(a.TurnID, b.TurnID); d != 0 {
			return d
		}
		if d := strings.Compare(a.TaskID, b.TaskID); d != 0 {
			return d
		}
		return strings.Compare(a.CallID, b.CallID)
	})
	for _, a := range affected {
		if automationRank(a.Stop.Kind) == 0 {
			continue
		}
		text := fmt.Sprintf("dashi: Turn %s", a.TurnID)
		if a.TaskID != "" {
			text += "; task " + a.TaskID
		}
		if a.CallID != "" {
			text += "; call " + a.CallID
		}
		text += fmt.Sprintf(" stopped: %s; next: %s", a.Stop.Kind, a.Next)
		if a.Stop.Reason != "" {
			text += "; " + sanitizeAutomationDiagnostic(a.Stop.Reason)
		}
		fmt.Fprintln(stderr, workspace.PrintableText(text+". Reopen and inspect this exact target before recovery."))
	}
}

var automationCredentialHeader = regexp.MustCompile(`(?i)(authorization|proxy-authorization|api[-_]key|access[-_]token|password|secret)[ \t]*[:=][^\r\n]*`)
var automationDiagnosticURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func sanitizeAutomationDiagnostic(value string) string {
	value = automationCredentialHeader.ReplaceAllString(value, "$1: [redacted]")
	return automationDiagnosticURL.ReplaceAllStringFunc(value, func(value string) string {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			return "[endpoint]"
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	})
}
func loadAutomationSnapshot(ctx context.Context, loader interface {
	dashi.Loader
	dashisession.Index
}, id string) (automationSnapshot, error) {
	var snap automationSnapshot
	n, err := loader.LastTurn(ctx, id)
	if err != nil {
		return snap, errors.New("cannot read required parent index")
	}
	var views []dashi.TurnView
	var parents []dashi.Turn
	for i := 1; i <= n; i++ {
		turn, err := loader.Load(ctx, dashisession.TurnID(id, i))
		if err != nil {
			return snap, fmt.Errorf("required parent Turn %s is unreadable", dashisession.TurnID(id, i))
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			return snap, fmt.Errorf("required parent Turn %s cannot be replayed", turn.ID)
		}
		parents = append(parents, turn)
		views = append(views, view)
		snap.latest = view
		if view.Origin == dashi.StartUser || view.Origin == "" {
			snap.request = i
		}
		if view.Origin != dashi.StartCompaction {
			snap.answer = view
		}
	}
	if snap.request == 0 && n > 0 {
		snap.request = 1
	}
	for i, view := range views {
		if i+1 >= snap.request && view.Origin != dashi.StartCompaction || i+1 == n {
			if automationRank(view.Stop.Kind) != 0 {
				snap.affected = append(snap.affected, automationViewAttention(view, "", "")...)
			}
		}
	}
	// This record-only host uses the existing task parent-link validation. It has
	// no tools, client, resolver, or worker and cannot dispatch anything.
	host := &taskHost{inputs: &taskInputs{loader: loader}}
	if err := host.readRecorded(ctx, parents); err != nil {
		return snap, errors.New("cannot inspect required child records")
	}
	if len(host.problems) > 0 {
		return snap, errors.New("required child record or parent link is unreadable; inspect /tasks after reopening")
	}
	delivered := recordedTaskNotices(parents)
	for _, child := range host.recorded {
		relevant := turnNumber(child.Task.Parent.TurnID) >= snap.request
		if !taskNoticeReady(child.View) || relevant && child.View.Stop.Kind != dashi.StopCompleted {
			snap.affected = append(snap.affected, automationViewAttention(child.View, child.Task.ID, child.Task.Parent.CallID)...)
		}
		if taskNoticeReady(child.View) && !delivered[delegate.NoticeID(child.View.ID)] {
			snap.missingNotice = true
		}
	}
	for _, view := range views {
		for callID, call := range view.Tools {
			if call.Call.Name != delegate.ToolName || call.Status == dashi.ToolDenied || call.Status == dashi.ToolRequested || call.Status == dashi.ToolApprovalPending {
				continue
			}
			taskID := delegate.TaskID(delegate.ChildID(view.ID, callID))
			if _, ok := host.recorded[taskID]; !ok && (call.Outcome == nil || call.Outcome.State == "succeeded") {
				return snap, fmt.Errorf("required child record for Turn %s call %s is unavailable", view.ID, callID)
			}
		}
	}
	if snap.missingNotice && len(snap.affected) == 0 {
		snap.affected = append(snap.affected, automationAttention{TurnID: snap.latest.ID, Stop: dashi.Stop{Kind: dashi.StopWaiting, Reason: "recorded child notice still needs foreground delivery"}, Next: dashi.NextContinue, Ready: true})
	}
	return snap, nil
}
func openAutomationSaved(ctx context.Context, store *session.Store, dir string, o Options) (*session.Session, *filerecorder.Reader, func(), error) {
	id := o.ResumeID
	if id == "" {
		result, err := store.Search(ctx, dir, "")
		if err != nil {
			return nil, nil, nil, err
		}
		if len(result.Matches) == 0 {
			return nil, nil, nil, session.ErrNotFound
		}
		id = result.Matches[0].Card.Metadata.ID
	}
	s, err := store.LoadScoped(ctx, dir, id)
	if err != nil {
		return nil, nil, nil, err
	}
	path := filepath.Join(store.Root(), id)
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() {
		return nil, nil, nil, errors.New("saved session directory is unavailable")
	}
	if lock, err := os.Lstat(filepath.Join(path, "lock")); err == nil && !lock.Mode().IsRegular() {
		return nil, nil, nil, errors.New("saved session lock is unsafe")
	}
	unlock, err := store.Lock(s)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (*session.Session, *filerecorder.Reader, func(), error) {
		unlock()
		return nil, nil, nil, err
	}
	s, err = store.LoadScoped(ctx, dir, id)
	if err != nil {
		return fail(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return fail(errors.New("saved session directory changed"))
	}
	reader, err := filerecorder.OpenReader(filepath.Join(path, "turns"))
	if err != nil {
		return fail(errors.New("required saved records are unavailable"))
	}
	return s, reader, unlock, nil
}

func (s automationSnapshot) canContinue() bool {
	for _, a := range s.affected {
		if a.TaskID == "" && s.missingNotice && a.Stop.Kind == dashi.StopWaiting && a.Next == dashi.NextContinue {
			continue
		}
		if a.Next == dashi.NextContinue || a.Next == dashi.NextResume && a.Ready {
			continue
		}
		return false
	}
	return true
}
func (s automationSnapshot) blocksNew() bool {
	if s.latest.Next() != dashi.NextNone || s.missingNotice {
		return true
	}
	for _, a := range s.affected {
		if a.TaskID != "" && a.Next != dashi.NextNone {
			return true
		}
	}
	return false
}

func automationViewAttention(view dashi.TurnView, taskID, fallbackCall string) []automationAttention {
	var affected []automationAttention
	for _, pending := range view.Pending {
		if pending.Kind != dashi.PendingTool && pending.Kind != dashi.PendingApproval {
			continue
		}
		affected = append(affected, automationAttention{TurnID: view.ID, TaskID: taskID, CallID: pending.ID, Stop: view.Stop, Next: view.Next(), Ready: view.ReadyToContinue})
	}
	if len(affected) == 0 {
		affected = append(affected, automationAttention{TurnID: view.ID, TaskID: taskID, CallID: fallbackCall, Stop: view.Stop, Next: view.Next(), Ready: view.ReadyToContinue})
	}
	return affected
}
