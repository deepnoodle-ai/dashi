package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
)

// TaskTarget identifies the exact recorded child state inspected by a host.
// Revision is the last recorded step sequence, not a task-list counter.
type TaskTarget struct {
	ID      string
	TurnID  string
	LastSeq int64
}

var (
	// ErrTaskBusy means a worker already owns the child.
	ErrTaskBusy = errors.New("delegate: task worker is running")
	// ErrTaskCommandUnknown means a prior write must be reconciled before a
	// different action. Reload and retry the exact command, or inspect the
	// authoritative changed record before choosing another action.
	ErrTaskCommandUnknown = errors.New("delegate: child command acknowledgement is unknown")
)

type pendingTaskCommand struct {
	target   TaskTarget
	id, hash string
}

func ownedCommand(cmd dashi.Command) (dashi.Command, string, string, error) {
	var owned dashi.Command
	var id string
	var err error
	switch c := cmd.(type) {
	case dashi.Continue:
		var v dashi.Continue
		v, err = copyValue(c)
		v.OnHistoryMismatch = c.OnHistoryMismatch
		owned, id = v, c.CommandID
	case dashi.Resolve:
		var v dashi.Resolve
		v, err = copyValue(c)
		owned, id = v, c.CommandID
	case dashi.Cancel:
		owned, id = c, c.CommandID
	default:
		return nil, "", "", errors.New("delegate: task control accepts Continue, Resolve, or Cancel")
	}
	if err != nil {
		return nil, "", "", err
	}
	if id == "" {
		return nil, "", "", errors.New("delegate: task command needs a CommandID")
	}
	b, err := json.Marshal(owned)
	if err != nil {
		return nil, "", "", err
	}
	return owned, id, dashi.ArgsHash(b), nil
}

func receipt(turn dashi.Turn, id, hash string) (bool, error) {
	for _, s := range turn.Steps {
		if s.CommandID == id {
			if s.CommandHash != hash {
				return false, dashi.ErrCommandIDReused
			}
			return true, nil
		}
	}
	return false, nil
}

// Run applies one explicit command to an inspected stopped child. It never
// creates a Turn or retries an unsettled effect itself. Existing command
// receipts are checked before Revision, so an acknowledged retry is safe even
// after later child work. Stop and Close own this worker as they own task starts.
// A recorder implementing Loader is required for authoritative reconciliation.
func (ts *Tasks) Run(ctx context.Context, target TaskTarget, cmd dashi.Command) (out dashi.RunResult, runErr error) {
	owned, id, hash, err := ownedCommand(cmd)
	if err != nil {
		return out, err
	}
	ts.mu.Lock()
	t := ts.tasks[target.ID]
	if t == nil {
		ts.mu.Unlock()
		return out, ErrUnknownTask
	}
	if ts.closed {
		ts.mu.Unlock()
		return out, errors.New("delegate: tasks are closed")
	}
	if t.cancel != nil {
		ts.mu.Unlock()
		return out, ErrTaskBusy
	}
	loader, ok := t.session.Runner.Recorder.(dashi.Loader)
	if !ok {
		ts.mu.Unlock()
		return out, errors.New("delegate: task control needs a recorder that implements dashi.Loader")
	}
	currentID := t.last().id
	parent := t.last().view().Parent
	pending := t.pending
	workCtx, cancel := context.WithCancel(ctx)
	t.cancel, t.done = cancel, make(chan struct{})
	t.controlling = true
	ts.wg.Add(1)
	ts.mu.Unlock()
	defer func() {
		ts.mu.Lock()
		cancel()
		t.cancel, t.stopping, t.controlling = nil, false, false
		close(t.done)
		ts.changedLocked()
		ts.mu.Unlock()
		ts.wg.Done()
	}()
	rec, err := loadRecord(workCtx, loader, t.sessionID)
	if err != nil {
		return out, fmt.Errorf("delegate: reload child: %w", err)
	}
	if len(rec.turns) == 0 {
		return out, dashi.ErrTurnNotFound
	}
	turn := rec.turns[len(rec.turns)-1]
	view := rec.views[len(rec.views)-1]
	out = dashi.RunResult{Turn: turn, TurnView: view}
	if turn.ID != currentID || target.TurnID != turn.ID ||
		!equalValue(rec.views[0].Parent, &t.parent) || !equalValue(view.Parent, parent) {
		return out, dashi.ErrStaleTurn
	}
	if pending != nil {
		known, e := receipt(turn, pending.id, pending.hash)
		if e != nil || known || view.LastSeq != pending.target.LastSeq {
			ts.mu.Lock()
			t.pending = nil
			ts.mu.Unlock()
		} else if target != pending.target || id != pending.id || hash != pending.hash {
			ts.controlResult(t, out, true)
			return out, ErrTaskCommandUnknown
		}
	}
	known, err := receipt(turn, id, hash)
	if err != nil {
		return out, err
	}
	if !known && view.LastSeq != target.LastSeq {
		return out, dashi.ErrStaleTurn
	}
	// The runner owns all command/state validation and persisted worker fences.
	// Do not promote a fresh command's rejected state to a synthetic task failure.
	if !known {
		ts.mu.Lock()
		c := t.last()
		c.steps, c.final, c.state = turn.Steps, nil, TaskRunning
		c.answer, c.reason = "", ""
		if c.suppressed {
			c.notified, c.reported, c.suppressed = false, false, false
		}
		ts.changedLocked()
		ts.mu.Unlock()
	}
	for ev, e := range t.session.Runner.Stream(workCtx, turn, owned) {
		if ev.Step != nil {
			ts.observe(t, *ev.Step)
			if ev.Step.Kind == dashi.StepTurnContinued {
				ts.mu.Lock()
				started := ts.viewLocked(t, nil)
				ts.mu.Unlock()
				ts.onTask(TaskStarted, started)
			}
		}
		if ev.Result != nil {
			out = *ev.Result
		}
		if e != nil {
			runErr = e
		}
	}
	if ts.isStopping(t) && (runErr != nil || out.Stop.Kind == dashi.StopInterrupted || out.Stop.Kind == dashi.StopWaiting) {
		// Stop is a separate explicit intent. If its write is unknown,
		// fence that cancellation receipt, not the preceding Continue.
		target.LastSeq = out.LastSeq
		_, id, hash, _ = ownedCommand(dashi.Cancel{CommandID: turn.ID + "/cancel", Reason: "stopped on request"})
		canceled, err := ts.cancelTurn(t, turn.ID)
		if canceled.Turn.ID != "" {
			out = canceled
		}
		runErr = err
	}
	var commitErr *dashi.CommitError
	unknown := errors.As(runErr, &commitErr) && !commitErr.NothingWritten
	if unknown {
		ts.mu.Lock()
		t.pending = &pendingTaskCommand{target: target, id: id, hash: hash}
		ts.mu.Unlock()
	}
	// Reconcile presentation too: a lost acknowledgement is not proof of a
	// synthetic failure or permission to deliver a final result.
	if runErr != nil {
		loaded, e := loader.Load(context.WithoutCancel(ctx), turn.ID)
		if e == nil {
			v, e := loaded.View()
			if e == nil {
				out = dashi.RunResult{Turn: loaded, TurnView: v}
			}
		}
	}
	if len(out.Turn.Steps) > 0 {
		ts.controlResult(t, out, unknown)
	}
	return out, runErr
}

func (ts *Tasks) controlResult(t *task, out dashi.RunResult, unknown bool) {
	deferred := unknown || ts.deferNotice(out.Turn)
	ts.mu.Lock()
	c := t.last()
	v := out.TurnView
	c.final, c.steps = &v, out.Turn.Steps
	c.state, c.answer, c.reason = endState(ts, out.Stop, v)
	c.deferred = deferred
	if ts.closed && out.Stop.Kind == dashi.StopInterrupted {
		c.suppressed = !c.reported
		c.notified, c.reported = true, true
	}
	ts.changedLocked()
	ts.mu.Unlock()
	if !unknown {
		ts.notifyEnded(t, nil)
	}
}
