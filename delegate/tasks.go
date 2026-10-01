package delegate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// defaultMaxReply is the byte cap on an answer returned to the model when
// Config.MaxReply is zero. A longer answer would refill the context the
// model delegated to protect.
const defaultMaxReply = 16 << 10

// defaultMaxTaskTurns bounds the turns of one child session when
// Config.MaxTaskTurns is zero.
const defaultMaxTaskTurns = 10

// stopTimeout bounds the write that records a stop after Stop or Close.
const stopTimeout = 30 * time.Second

// TaskState is "running" or the dashi.StopKind of a child turn: completed,
// failed, limited, refused, canceled, interrupted, waiting, or uncertain. No
// other names exist.
type TaskState string

// TaskRunning is the state of a task whose child turn is working.
const TaskRunning TaskState = "running"

// TaskEventKind says what happened to a task.
type TaskEventKind string

// Task event kinds.
const (
	// TaskStarted means a child turn started, or resumed after Recover.
	TaskStarted TaskEventKind = "started"
	// TaskEnded means a child turn reached a state other than running and
	// Close did not cause it.
	TaskEnded TaskEventKind = "ended"
)

// TaskEvent is one change of a task, for Config.OnTask.
type TaskEvent struct {
	Kind TaskEventKind
	Task TaskView
}

// TaskView is a copy of the state of one task. A task runs in a child
// session, one turn at a time.
type TaskView struct {
	// ID is what the model and the user call the task, such as "t_k3f2".
	ID string
	// SessionID is the ID of the child session (ChildID of the task call).
	SessionID string
	// TurnID names the child turn that runs now, or the last one. In a
	// TaskEnded event it is the turn that ended.
	TurnID string
	// Turns counts the child turns so far.
	Turns       int
	Subagent    string
	Description string
	// Parent is the tool call of the parent turn that started the task.
	Parent dashi.CallRef
	// State is the state of the turn TurnID names.
	State TaskState
	// ModelCalls and ToolCalls count the child's work so far, over every
	// turn.
	ModelCalls int
	ToolCalls  int
	// Spend is the spend of every child turn. It is live for a running turn.
	Spend dashi.Spend
	// Reply is the answer of the turn TurnID names, cut at
	// Config.MaxReply. It is empty unless that turn completed.
	Reply string
	// Reason says why that turn stopped, when it did not complete.
	Reason string
}

// Running reports whether a turn of the task is working.
func (v TaskView) Running() bool {
	return v.State == TaskRunning
}

// TaskID returns the ID of the task for a child session. It is a fixed
// function of the session ID, so it is the same after a reopen and needs no
// counter.
func TaskID(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(sum[:])
	return "t_" + strings.ToLower(enc[:6])
}

// Errors of Tasks.Stop.
var (
	// ErrUnknownTask means Tasks holds no task with that ID.
	ErrUnknownTask = errors.New("delegate: unknown task")
	// ErrTaskEnded means no turn of the task is running.
	ErrTaskEnded = errors.New("delegate: task already ended")
)

// Tasks runs and tracks the background children of one conversation. The host
// creates it with NewTasks and keeps it for the life of the conversation.
// Use one Tasks value for one conversation. Its methods are safe for
// concurrent use.
type Tasks struct {
	cfg     Config
	catalog []Subagent
	warn    sync.Once

	mu      sync.Mutex
	tasks   map[string]*task
	order   []string
	closed  bool
	changed chan struct{} // closed and replaced when a turn ends or reports
	reports int           // reports finished
	waited  int           // reports that Wait has returned for
	wg      sync.WaitGroup
}

// task is the mutable record of one child session. Tasks.mu guards every
// field except the immutable ones in the first block.
type task struct {
	id, sessionID string
	parent        dashi.CallRef
	sub           Subagent
	description   string
	session       *session.Session

	// turns holds one entry per child turn, in order.
	turns []*childTurn
	// cancel and done belong to the goroutine that drives the session.
	// cancel is nil while none runs; done closes when it exits.
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
	// started closes when the child's record holds its first step, or when
	// the first run ends without one.
	started chan struct{}
	// pending fences an unacknowledged explicit command until record reload.
	pending     *pendingTaskCommand
	controlling bool
}

// childTurn is one turn of a child session.
type childTurn struct {
	id    string
	state TaskState
	steps []dashi.Step // observed while running
	final *dashi.TurnView
	// answer and reason are cut and ready for the model.
	answer, reason string
	// notified is set when a report starts, reported when it is done. An
	// ended turn counts as pending until reported.
	notified, reported bool
	deferred           bool
	// suppressed distinguishes Close interruption from acknowledged notices.
	suppressed bool
}

// view returns the turn's view: final, or replayed from the observed steps.
func (c *childTurn) view() dashi.TurnView {
	if c.final != nil {
		return *c.final
	}
	if len(c.steps) > 0 {
		if v, err := dashi.Replay(dashi.Turn{ID: c.id, Steps: c.steps}); err == nil {
			return v
		}
	}
	return dashi.TurnView{}
}

// NewTasks returns an empty Tasks. A Recorder in cfg.Runner that does not
// implement dashi.Loader logs one warning.
func NewTasks(cfg Config) *Tasks {
	catalog := cfg.Subagents
	if len(catalog) == 0 {
		catalog = []Subagent{Explore(), General()}
	}
	t := &Tasks{
		cfg:     cfg,
		catalog: slices.Clone(catalog),
		tasks:   map[string]*task{},
		changed: make(chan struct{}),
	}
	if cfg.Runner.Recorder != nil && !loads(cfg.Runner.Recorder) {
		t.warnNoLoader()
	}
	return t
}

// loads reports whether the recorder can load turns. A nil recorder cannot.
func loads(r dashi.Recorder) bool {
	if r == nil {
		return false
	}
	_, ok := r.(dashi.Loader)
	return ok
}

func (ts *Tasks) warnNoLoader() {
	ts.warn.Do(func() {
		slog.Warn(
			"delegate: tasks cannot survive a restart without a host " +
				"recorder that implements dashi.Loader",
		)
	})
}

// List returns every task the value knows, in the order they started.
func (ts *Tasks) List() []TaskView {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]TaskView, 0, len(ts.order))
	for _, id := range ts.order {
		out = append(out, ts.viewLocked(ts.tasks[id], nil))
	}
	return out
}

// Stop cancels the running turn of a task and records that turn as
// canceled. Recover never resumes a canceled turn. The task itself stays:
// its child session keeps its history and may run later turns. Stop returns
// after the turn has stopped, or with the error of ctx if that ends first
// (the turn keeps stopping). It returns ErrUnknownTask, or ErrTaskEnded when
// no turn runs.
func (ts *Tasks) Stop(ctx context.Context, id string) error {
	return ts.stop(ctx, id)
}

func (ts *Tasks) stop(ctx context.Context, id string) error {
	ts.mu.Lock()
	t, ok := ts.tasks[id]
	if !ok {
		ts.mu.Unlock()
		return fmt.Errorf("%w %q", ErrUnknownTask, id)
	}
	if state := t.state(); t.cancel == nil || state != TaskRunning && !t.controlling {
		ts.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrTaskEnded, state)
	}
	t.stopping = true
	cancel, done := t.cancel, t.done
	ts.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait blocks until there may be something to deliver or nothing is left to
// wait for. A child turn counts as pending from its start until OnTask has
// returned for its end, so a host that sends the notice from OnTask sees it
// in its inbox by the time Wait says there is nothing pending.
//
// Wait returns false as soon as one more turn has been reported since the
// last return. It returns true when no turn is pending. A host loop reads
// its inbox after every return. It stops when Wait returns true and the
// inbox holds nothing to deliver. Wait serves one caller at a time. It
// returns the error of ctx if that ends first.
func (ts *Tasks) Wait(ctx context.Context) (idle bool, err error) {
	for {
		ts.mu.Lock()
		if ts.reports > ts.waited {
			ts.waited = ts.reports
			ts.mu.Unlock()
			return false, nil
		}
		busy := false
		if ts.cfg.NoticeReady != nil || ts.hasPendingCommandLocked() {
			var attention []TaskView
			attention, busy = ts.attentionLocked()
			if !busy && len(attention) > 0 {
				ts.mu.Unlock()
				return false, &AttentionError{Tasks: attention}
			}
		}
		if ts.pendingLocked() == 0 && !busy {
			ts.mu.Unlock()
			return true, nil
		}
		ch := ts.changed
		ts.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// pendingLocked counts the child turns that are running or whose end
// OnTask has not yet reported. A turn that Close interrupted is not pending.
func (ts *Tasks) pendingLocked() int {
	n := 0
	for _, t := range ts.tasks {
		for _, c := range t.turns {
			if !c.reported {
				n++
			}
		}
	}
	return n
}

// reportedLocked marks a turn as reported. count says whether the report is
// news to Wait.
func (ts *Tasks) reportedLocked(c *childTurn, count bool) {
	c.notified, c.reported = true, true
	if count {
		ts.reports++
	}
	ts.changedLocked()
}

func (ts *Tasks) changedLocked() {
	close(ts.changed)
	ts.changed = make(chan struct{})
}

// Close interrupts every running child turn and waits for each to record
// that. An interrupted turn stays resumable, so the next Recover continues
// it. Close does not call OnTask for these turns. After Close, the tool
// refuses new tasks. If ctx ends first, Close returns its error and the
// children keep stopping.
func (ts *Tasks) Close(ctx context.Context) error {
	ts.mu.Lock()
	ts.closed = true
	for _, t := range ts.tasks {
		if t.cancel != nil {
			t.cancel()
		}
	}
	ts.mu.Unlock()
	done := make(chan struct{})
	go func() {
		ts.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover rebuilds the tasks of a reopened conversation from the turns the
// host holds for it. It finds each turn's task calls, loads each child
// session (ChildID) turn by turn, and adds the tasks to the list. It
// resumes a last child turn that has no recorded stop or stopped
// interrupted; it never resumes a canceled one. For each child turn that
// already ended, it calls OnTask with TaskEnded, and delivers the notice to
// Config.Session, unless the record holds that turn's notice (see NoticeID)
// or an earlier call already reported the turn. So each ended turn is
// reported once. It restores every task and reports the ended turns before
// it resumes any turn. The runner is the one the host uses for the
// conversation. It needs a recorder that implements dashi.Loader and
// returns an error that says so if it does not. It skips a child whose subagent the catalog
// no longer holds and returns an error for it after the rest.
func (ts *Tasks) Recover(
	ctx context.Context,
	r dashi.Runner,
	turns []dashi.Turn,
) error {
	recorder := cmp.Or(ts.cfg.Runner.Recorder, r.Recorder)
	loader, ok := recorder.(dashi.Loader)
	if !ok {
		return errors.New(
			"delegate: Recover needs a recorder that implements dashi.Loader",
		)
	}
	delivered := deliveredNotices(turns)
	var errs []error
	var adopted, resumed []*task
	// Restore every task before any turn runs, so a resumed turn that ends
	// early finds each delivered turn marked and never reports it again.
	for _, turn := range turns {
		view, err := dashi.Replay(turn)
		if err != nil {
			errs = append(errs, fmt.Errorf("delegate: replay %s: %w", turn.ID, err))
			continue
		}
		for _, callID := range slices.Sorted(maps.Keys(view.Tools)) {
			call := view.Tools[callID].Call
			if call.Name != ToolName {
				continue
			}
			ref := dashi.CallRef{TurnID: turn.ID, CallID: callID}
			t, resume, err := ts.adopt(ctx, r, loader, ref, call.Args, delivered)
			if err != nil {
				errs = append(errs, err)
			}
			if t != nil && !slices.Contains(adopted, t) {
				adopted = append(adopted, t)
			}
			if resume {
				resumed = append(resumed, t)
			}
		}
	}
	// Report the ended turns nobody has heard of, whether the task is new
	// here or known in this process. Then resume the unfinished turns.
	for _, t := range adopted {
		ts.notifyEnded(t, delivered)
	}
	for _, t := range resumed {
		ts.resume(ctx, t)
	}
	return errors.Join(errs...)
}

func (ts *Tasks) adopt(
	ctx context.Context,
	parent dashi.Runner,
	loader dashi.Loader,
	ref dashi.CallRef,
	rawArgs []byte,
	delivered map[string]bool,
) (*task, bool, error) {
	sessionID := ChildID(ref.TurnID, ref.CallID)
	rec, err := loadRecord(ctx, loader, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf(
			"delegate: load child session %s: %w", sessionID, err,
		)
	}
	if len(rec.turns) == 0 {
		return nil, false, nil
	}
	args, _ := parseTaskArgs(rawArgs)
	sub, ok := ts.find(args.Subagent)
	if !ok {
		return nil, false, fmt.Errorf(
			"delegate: task %s uses subagent %q, which the catalog lacks",
			TaskID(sessionID), args.Subagent,
		)
	}
	return ts.register(ctx, sub, ref, args.Description, parent, rec, delivered)
}

// register adds the task for a child session that already has turns. It
// reports whether the last turn is unfinished and needs resume. It returns
// the task that already holds the ID when there is one.
func (ts *Tasks) register(
	ctx context.Context,
	sub Subagent,
	ref dashi.CallRef,
	description string,
	parent dashi.Runner,
	rec record,
	delivered map[string]bool,
) (*task, bool, error) {
	t := newTask(sub, ref, description)
	ts.mu.Lock()
	old, existingErr := ts.existingLocked(t)
	ts.mu.Unlock()
	if old != nil || existingErr != nil {
		return old, false, existingErr
	}
	runner, err := ts.bindServices(ctx, ref, rec.views[len(rec.views)-1].Agent, ts.childRunner(sub, ref, parent))
	if err != nil {
		return nil, false, err
	}
	ts.prepareNoticePolicy(&rec)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if old, err := ts.existingLocked(t); old != nil || err != nil {
		return old, false, err
	}
	if ts.closed {
		return nil, false, errors.New("delegate: tasks are closed")
	}
	ts.addLocked(t)
	// Continue runs with the agent the child recorded, whose Tools name the
	// tools it may use, so a change to the subagent since then does not reach
	// it. startTask does the same.
	t.session = childSession(t, runner, false)
	return t, ts.restoreLocked(t, rec, delivered), nil
}

// resume continues the unfinished last turn of a restored task. When Close
// came first, the turn stays interrupted for the next Recover.
func (ts *Tasks) resume(ctx context.Context, t *task) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.closed {
		c := t.last()
		c.state = TaskState(dashi.StopInterrupted)
		c.suppressed = !c.reported
		c.notified, c.reported = true, true
		t.setStarted()
		ts.changedLocked()
		return
	}
	ts.launchLocked(ctx, t, dashi.Continue{})
}

// record is what the recorder holds for one child session: its turns, in
// order, and their views.
type record struct {
	turns    []dashi.Turn
	views    []dashi.TurnView
	deferred []bool
}

// loadRecord loads the turns of a child session from turn 1 until one is
// missing.
func loadRecord(
	ctx context.Context,
	loader dashi.Loader,
	sessionID string,
) (record, error) {
	var rec record
	for n := 1; ; n++ {
		turn, err := loader.Load(ctx, session.TurnID(sessionID, n))
		if errors.Is(err, dashi.ErrTurnNotFound) || err == nil && len(turn.Steps) == 0 {
			return rec, nil
		}
		if err != nil {
			return record{}, err
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			return record{}, fmt.Errorf("replay %s: %w", turn.ID, err)
		}
		rec.turns = append(rec.turns, turn)
		rec.views = append(rec.views, view)
	}
}

// restoreLocked fills the task's turns from its record and marks the ended
// turns whose notice delivered holds as reported. It reports whether the
// last turn is unfinished: no recorded stop, or an interruption. That turn
// stays running for the caller to resume.
func (ts *Tasks) restoreLocked(
	t *task,
	rec record,
	delivered map[string]bool,
) bool {
	resume := false
	for i, turn := range rec.turns {
		c := &childTurn{id: turn.ID}
		if len(rec.deferred) > i {
			c.deferred = rec.deferred[i]
		}
		stop, stopped := recordedStop(turn)
		if i == len(rec.turns)-1 && (!stopped || stop.Kind == dashi.StopInterrupted) {
			c.state, c.steps, resume = TaskRunning, slices.Clone(turn.Steps), true
		} else {
			v := rec.views[i]
			c.final = &v
			c.state, c.answer, c.reason = endState(ts, stop, v)
		}
		if delivered[c.id] {
			ts.reportedLocked(c, false)
		}
		t.turns = append(t.turns, c)
	}
	if !resume {
		t.setStarted()
	}
	return resume
}

func newTask(sub Subagent, ref dashi.CallRef, description string) *task {
	sessionID := ChildID(ref.TurnID, ref.CallID)
	return &task{
		id:          TaskID(sessionID),
		sessionID:   sessionID,
		parent:      ref,
		sub:         sub,
		description: description,
		started:     make(chan struct{}),
	}
}

func (t *task) setStarted() {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
}

// last returns the task's last turn, or nil before its first.
func (t *task) last() *childTurn {
	if len(t.turns) == 0 {
		return nil
	}
	return t.turns[len(t.turns)-1]
}

// state is the state of the last turn: running before the first.
func (t *task) state() TaskState {
	if c := t.last(); c != nil {
		return c.state
	}
	return TaskRunning
}

// turn returns the task's turn with id, adding it as running when the task
// has none.
func (t *task) turn(id string) *childTurn {
	for _, c := range slices.Backward(t.turns) {
		if c.id == id {
			return c
		}
	}
	c := &childTurn{id: id, state: TaskRunning}
	t.turns = append(t.turns, c)
	return c
}

// recordedStop returns the latest stop the record holds. Replay projects a
// stop for a turn that has none, so the steps are the only proof.
func recordedStop(t dashi.Turn) (dashi.Stop, bool) {
	for _, s := range slices.Backward(t.Steps) {
		if s.Stop != nil {
			return *s.Stop, true
		}
	}
	return dashi.Stop{}, false
}

// existingLocked returns the task already holding t's ID. It is an error
// when that task belongs to another child session.
func (ts *Tasks) existingLocked(t *task) (*task, error) {
	old, ok := ts.tasks[t.id]
	if !ok {
		return nil, nil
	}
	if old.sessionID != t.sessionID {
		return nil, fmt.Errorf(
			"delegate: task ID %s is already used by another child; "+
				"try the task again", t.id,
		)
	}
	return old, nil
}

func (ts *Tasks) addLocked(t *task) {
	ts.tasks[t.id] = t
	ts.order = append(ts.order, t.id)
}

// maxTaskTurns returns the turn bound of one child session; 0 means none.
func (ts *Tasks) maxTaskTurns() int {
	switch n := ts.cfg.MaxTaskTurns; {
	case n < 0:
		return 0
	case n == 0:
		return defaultMaxTaskTurns
	default:
		return n
	}
}

// checkTurnsLocked refuses a next turn once the child session has run
// Config.MaxTaskTurns turns.
func (ts *Tasks) checkTurnsLocked(t *task) error {
	if limit := ts.maxTaskTurns(); limit > 0 && len(t.turns) >= limit {
		return fmt.Errorf(
			"delegate: task %s has run %d turns; start a new task",
			t.id, len(t.turns),
		)
	}
	return nil
}

// startTurnLocked starts the next turn of a child session that runs none,
// unless checkTurnsLocked refuses it. The caller holds ts.mu and has checked
// that ts is not closed.
func (ts *Tasks) startTurnLocked(
	ctx context.Context,
	t *task,
	start dashi.Start,
) error {
	if err := ts.checkTurnsLocked(t); err != nil {
		return err
	}
	t.turns = append(t.turns, &childTurn{
		id:    session.TurnID(t.sessionID, len(t.turns)+1),
		state: TaskRunning,
	})
	ts.launchLocked(ctx, t, start)
	return nil
}

// launchLocked drives the child session in a goroutine that Tasks owns. The
// caller holds ts.mu, has checked that ts is not closed, and has made the
// turn that cmd runs the task's last. The child keeps the values of base but
// not its cancellation or deadline, so canceling the parent turn does not
// stop the task.
func (ts *Tasks) launchLocked(
	base context.Context,
	t *task,
	cmd dashi.Command,
) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(base))
	t.cancel, t.done = cancel, make(chan struct{})
	ts.wg.Add(1)
	go ts.run(ctx, t, cmd)
}

// run ranges over the child session's Stream. Each turn's Outcome event ends
// that turn; the goroutine exits when the call returns.
func (ts *Tasks) run(ctx context.Context, t *task, cmd dashi.Command) {
	defer ts.wg.Done()
	ts.mu.Lock()
	current := t.last().id
	started := ts.viewLocked(t, nil)
	ts.mu.Unlock()
	ts.onTask(TaskStarted, started)
	var runErr error
	for ev, err := range t.session.Stream(ctx, cmd) {
		switch {
		case ev.Outcome != nil:
			out := *ev.Outcome
			id := cmp.Or(out.Turn.ID, current)
			if ts.isStopping(t) {
				out, err = ts.recordCancel(t, id, out, err)
			}
			ts.endTurn(t, id, out, err)
		case err != nil:
			runErr = err
		case ev.Step != nil:
			if ev.Step.Kind == dashi.StepTurnStarted && ev.TurnID != current {
				// A later turn of the same call, such as a queue turn.
				current = ev.TurnID
				ts.mu.Lock()
				v := ts.viewLocked(t, t.turn(current))
				ts.mu.Unlock()
				ts.onTask(TaskStarted, v)
			}
			ts.observe(t, *ev.Step)
		}
	}
	ts.mu.Lock()
	unended := t.turn(current).state == TaskRunning
	ts.mu.Unlock()
	if unended {
		// The session failed before the turn recorded an outcome.
		out, err := dashi.Outcome{}, cmp.Or(runErr, errNoOutcome)
		if ts.isStopping(t) {
			out, err = ts.recordCancel(t, current, out, err)
		}
		ts.endTurn(t, current, out, err)
	}
	ts.mu.Lock()
	t.setStarted()
	t.cancel()
	t.cancel, t.stopping = nil, false
	close(t.done)
	ts.changedLocked()
	ts.mu.Unlock()
}

// errNoOutcome marks a run that ended without an outcome or an error.
var errNoOutcome = errors.New("the child session ended without an outcome")

func (ts *Tasks) isStopping(t *task) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return t.stopping
}

// recordCancel records the stop that Stop asked for on turn id. A turn that
// already ended for another reason keeps that reason.
func (ts *Tasks) recordCancel(
	t *task,
	id string,
	out dashi.Outcome,
	runErr error,
) (dashi.Outcome, error) {
	if runErr == nil && out.Stop.Kind != dashi.StopInterrupted &&
		out.Stop.Kind != dashi.StopWaiting {
		return out, nil
	}
	canceled, err := ts.cancelTurn(t, id)
	if err != nil || canceled.Turn.ID != id {
		return dashi.Outcome{}, errCanceled
	}
	return canceled, nil
}

// cancelTurn preserves the actual write acknowledgement for explicit control.
// recordCancel retains Stop's established synthetic fallback for old callers.
func (ts *Tasks) cancelTurn(t *task, id string) (dashi.Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	cmd := dashi.Cancel{CommandID: id + "/cancel", Reason: "stopped on request"}
	if loader, ok := t.session.Runner.Recorder.(dashi.Loader); ok {
		turn, err := loader.Load(ctx, id)
		if err != nil {
			return dashi.Outcome{}, err
		}
		return t.session.Runner.Run(ctx, turn, cmd)
	}
	return t.session.Run(ctx, cmd)
}

// errCanceled marks a stop that the record does not show.
var errCanceled = errors.New("stopped on request")

// endTurn records the end of turn id and reports it, unless Close caused it.
func (ts *Tasks) endTurn(t *task, id string, out dashi.Outcome, err error) {
	deferred := ts.deferNotice(out.Turn)
	ts.mu.Lock()
	c := t.turn(id)
	c.deferred = deferred
	switch {
	case errors.Is(err, errCanceled):
		c.state, c.reason = TaskState(dashi.StopCanceled), err.Error()
	case err != nil && ts.closed:
		c.state = TaskState(dashi.StopInterrupted)
	case err != nil:
		c.state = TaskState(dashi.StopFailed)
		c.reason = "the child could not run: " + err.Error()
	default:
		view := out.TurnView
		c.final = &view
		c.state, c.answer, c.reason = endState(ts, out.Stop, view)
	}
	t.setStarted()
	interrupted := c.state == TaskState(dashi.StopInterrupted)
	if interrupted {
		// Close is not an end: nothing will be reported.
		c.suppressed = !c.reported
		c.notified, c.reported = true, true
	}
	ts.changedLocked()
	ts.mu.Unlock()
	if !interrupted {
		ts.notifyEnded(t, nil)
	}
}

// endState maps a child turn's stop to its state, answer, and reason.
func endState(
	ts *Tasks,
	stop dashi.Stop,
	view dashi.TurnView,
) (TaskState, string, string) {
	state := TaskState(stop.Kind)
	if stop.Kind != dashi.StopCompleted {
		return state, "", stop.Reason
	}
	text := view.Reply
	if view.Output != nil && len(view.Output.Value) > 0 {
		text = string(view.Output.Value)
	}
	if strings.TrimSpace(text) == "" {
		text = "The subagent finished without a written answer."
	}
	return state, ts.capAnswer(text), ""
}

// notifyEnded reports each ended turn of the task that nobody was told of:
// it delivers the turn's notice to Config.Session and calls OnTask, once
// per turn, in order. A turn whose notice delivered holds counts as
// reported. A turn stays pending until OnTask has returned.
func (ts *Tasks) notifyEnded(t *task, delivered map[string]bool) {
	for {
		ts.mu.Lock()
		i := slices.IndexFunc(t.turns, func(c *childTurn) bool {
			return !c.notified && !c.deferred && c.state != TaskRunning
		})
		if i < 0 {
			ts.mu.Unlock()
			return
		}
		c := t.turns[i]
		if delivered[c.id] {
			ts.reportedLocked(c, false)
			ts.mu.Unlock()
			continue
		}
		c.notified = true
		v := ts.viewLocked(t, c)
		ts.mu.Unlock()
		ts.toParent(v.Notice().Steer())
		ts.onTask(TaskEnded, v)
		ts.mu.Lock()
		ts.reportedLocked(c, true)
		ts.mu.Unlock()
	}
}

// toParent puts a steer on Config.Session, when set: SteerItem reaches a
// running turn and queues the item when none runs. A session that refuses
// steers (while compacting, or with a host steering source) gets the item
// on its queue instead. Any other error is logged as it is.
func (ts *Tasks) toParent(steer dashi.Steer) {
	s := ts.cfg.Session
	if s == nil {
		return
	}
	ctx := context.Background()
	_, err := s.SteerItem(ctx, steer)
	if err != nil && refusesSteers(s, err) {
		_, err = s.EnqueueItem(ctx, steer.Item)
	}
	if err != nil {
		slog.Warn("delegate: could not deliver a notice to the parent session",
			"item", steer.Item.ID, "error", err)
	}
}

// refusesSteers reports whether err is the session refusing any steer, not
// this one: a compaction turn runs, or the host supplies the steering source.
func refusesSteers(s *session.Session, err error) bool {
	var busy *session.TurnBusyError
	if errors.As(err, &busy) && busy.Origin == dashi.StartCompaction {
		return true
	}
	_, inbox := s.Steering.(*dashi.Inbox)
	return s.Steering != nil && !inbox
}

func (ts *Tasks) onTask(kind TaskEventKind, v TaskView) {
	if ts.cfg.OnTask != nil {
		ts.cfg.OnTask(TaskEvent{Kind: kind, Task: v})
	}
}

// viewLocked returns the view of a task at turn c, or at its last turn when
// c is nil. Counts and spend cover every turn.
func (ts *Tasks) viewLocked(t *task, c *childTurn) TaskView {
	v := TaskView{
		ID:          t.id,
		SessionID:   t.sessionID,
		Turns:       len(t.turns),
		Subagent:    t.sub.Name,
		Description: t.description,
		Parent:      t.parent,
		State:       TaskRunning,
	}
	if c == nil {
		c = t.last()
	}
	if c != nil {
		v.TurnID, v.State, v.Reply, v.Reason = c.id, c.state, c.answer, c.reason
	}
	for _, c := range t.turns {
		tv := c.view()
		v.ModelCalls += tv.ModelAttempts
		v.ToolCalls += len(tv.Tools)
		v.Spend = v.Spend.Add(tv.Spend)
	}
	return v
}

// observe records a step of a running child turn so a status read can show
// live counts and cost.
func (ts *Tasks) observe(t *task, step dashi.Step) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t.setStarted()
	c := t.turn(step.TurnID)
	if c.final != nil {
		return
	}
	if n := len(c.steps); n > 0 && c.steps[n-1].Seq >= step.Seq {
		return
	}
	c.steps = append(c.steps, step)
}

func (ts *Tasks) find(name string) (Subagent, bool) {
	i := slices.IndexFunc(ts.catalog, func(s Subagent) bool {
		return s.Name == name
	})
	if i < 0 {
		return Subagent{}, false
	}
	return ts.catalog[i], true
}

func (ts *Tasks) names() []string {
	names := make([]string, len(ts.catalog))
	for i, s := range ts.catalog {
		names[i] = s.Name
	}
	return names
}

func (ts *Tasks) capAnswer(text string) string {
	limit := cmp.Or(ts.cfg.MaxReply, defaultMaxReply)
	if limit < 0 || len(text) <= limit {
		return text
	}
	cut := cutUTF8(text, limit)
	return fmt.Sprintf("%s\n[answer cut at %d bytes]", cut, len(cut))
}

// cutUTF8 returns at most n bytes of s, cut on a character boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// startTask starts the child session for one task call, or returns the task
// that already holds it.
func (ts *Tasks) startTask(
	ctx context.Context,
	sub Subagent,
	args taskArgs,
	ref dashi.CallRef,
	parent dashi.Runner,
	parentAgent dashi.Agent,
) (TaskView, error) {
	t := newTask(sub, ref, args.Description)
	ts.mu.Lock()
	old, existingErr := ts.existingLocked(t)
	if old != nil || existingErr != nil {
		defer ts.mu.Unlock()
		if existingErr != nil {
			return TaskView{}, existingErr
		}
		return ts.viewLocked(old, nil), nil
	}
	closed := ts.closed
	ts.mu.Unlock()
	if closed {
		return TaskView{}, errors.New("delegate: tasks are closed")
	}
	runner := ts.childRunner(sub, ref, parent)
	// A retry after a crash finds a child session the record already holds.
	var rec record
	if loader, ok := runner.Recorder.(dashi.Loader); !ok {
		ts.warnNoLoader()
	} else {
		var err error
		if rec, err = loadRecord(ctx, loader, t.sessionID); err != nil {
			return TaskView{}, fmt.Errorf("delegate: load child: %w", err)
		}
	}
	found := len(rec.turns) > 0
	var agent dashi.Agent
	var start dashi.Start
	if !found {
		var err error
		agent, err = childAgent(ctx, sub, parentAgent, parent.Tools)
		if err != nil {
			return TaskView{}, fmt.Errorf("delegate: resolve child tools: %w", err)
		}
		start, err = ts.prepareStart(ctx, ref, dashi.Start{CommandID: t.sessionID + "/start", Agent: agent, Parent: &ref, Input: []llm.Message{llm.UserMessage(llm.Text(args.Prompt))}})
		if err != nil {
			return TaskView{}, err
		}
		agent = start.Agent
	} else {
		agent = rec.views[len(rec.views)-1].Agent
		ts.prepareNoticePolicy(&rec)
	}
	var bindingErr error
	runner, bindingErr = ts.bindServices(ctx, ref, agent, runner)
	if bindingErr != nil {
		return TaskView{}, bindingErr
	}
	ts.mu.Lock()
	if old, err := ts.existingLocked(t); old != nil || err != nil {
		defer ts.mu.Unlock()
		if err != nil {
			return TaskView{}, err
		}
		return ts.viewLocked(old, nil), nil
	}
	if ts.closed {
		ts.mu.Unlock()
		return TaskView{}, errors.New("delegate: tasks are closed")
	}
	t.session = childSession(t, runner, !found)
	endedBefore := false
	switch {
	case !found:
		if err := ts.startTurnLocked(ctx, t, start); err != nil {
			ts.mu.Unlock()
			return TaskView{}, err
		}
	case ts.restoreLocked(t, rec, nil):
		ts.launchLocked(ctx, t, dashi.Continue{})
	default:
		// The child ended before the crash. Report the end here, so the
		// result does not depend on whether the host called Recover first.
		endedBefore = true
	}
	ts.addLocked(t)
	ts.mu.Unlock()
	if endedBefore {
		ts.notifyEnded(t, nil)
	}
	// The call settles only once the record holds the child, so a crash
	// after the call cannot lose a child the model was told about.
	select {
	case <-t.started:
	case <-ctx.Done():
		return TaskView{}, ctx.Err()
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if first := t.turns[0]; first.state != TaskRunning &&
		len(first.steps) == 0 && first.final == nil {
		// The run failed before it recorded the child.
		if !first.notified {
			ts.reportedLocked(first, false)
		}
		return TaskView{}, tool.Fatal(fmt.Errorf(
			"delegate: start subagent: %s", cmp.Or(first.reason, string(first.state)),
		))
	}
	return ts.viewLocked(t, nil), nil
}

// childAgent returns the child's Start.Agent. A Subagent that sets no model
// uses the parent turn's current model. Its Tools name every tool the child
// may use, resolved now from parent, the runner's tools: the Subagent's named
// tools, or else the tools the parent agent may use, less the task tools, and
// only read-only ones for a ReadOnly subagent. The record then bounds the
// child, so a resume needs no rule and a change to the Subagent cannot widen
// a running child. When nothing restricts the child (no named tools, a parent
// that may use every tool, and not ReadOnly), Tools stays empty, so the child
// may use every tool, as the parent may. A named tool that parent lacks now stays in the list, so
// the child warns that it is not offered, unless ReadOnly makes its effect
// unknown. An empty result sets NoTools, since empty Tools means every tool.
func childAgent(
	ctx context.Context,
	sub Subagent,
	parentAgent dashi.Agent,
	parent tool.Toolset,
) (dashi.Agent, error) {
	agent := sub.Agent
	if agent.Model == "" && agent.Provider == "" && agent.API == "" {
		agent.Provider = parentAgent.Provider
		agent.API = parentAgent.API
		agent.Model = parentAgent.Model
	}
	agent.ID = cmp.Or(agent.ID, sub.Name)
	if agent.NoTools {
		agent.Tools = nil
		return agent, nil
	}
	named := len(sub.Agent.Tools) > 0
	if !named && !sub.ReadOnly && !parentAgent.NoTools &&
		len(parentAgent.Tools) == 0 {
		// Nothing to freeze: like the parent, the child may use every tool,
		// and it follows a toolset that adds one later. The child's runner
		// hides the task tools.
		agent.Tools = nil
		return agent, nil
	}
	var all []tool.Tool
	if parent != nil && (named || !parentAgent.NoTools) {
		var err error
		if all, err = parent.Tools(ctx); err != nil {
			return dashi.Agent{}, err
		}
	}
	var names []string
	for _, t := range all {
		name := t.Definition.Name
		readOnly := t.Definition.Annotations.ReadOnly
		switch {
		case isTaskTool(name):
		case named && !slices.Contains(sub.Agent.Tools, name):
		case !named && len(parentAgent.Tools) > 0 &&
			!slices.Contains(parentAgent.Tools, name):
		case sub.ReadOnly && (readOnly == nil || !*readOnly):
		default:
			names = append(names, name)
		}
	}
	if !sub.ReadOnly {
		for _, name := range sub.Agent.Tools {
			if !isTaskTool(name) && !slices.ContainsFunc(all, func(t tool.Tool) bool {
				return t.Definition.Name == name
			}) {
				names = append(names, name)
			}
		}
	}
	agent.Tools = names
	agent.NoTools = len(names) == 0
	return agent, nil
}

func isTaskTool(name string) bool {
	return name == ToolName || name == StatusToolName || name == StopToolName
}

// childSession returns the handle that drives a task's child session. Tasks
// is its only driver, so it is Exclusive. create sets New, for turn 1.
func childSession(t *task, runner dashi.Runner, create bool) *session.Session {
	return &session.Session{
		ID:        t.sessionID,
		New:       create,
		Runner:    runner,
		Exclusive: true,
	}
}

// childRunner builds the runner for one child session. It starts from the
// runner that runs the task call, applies the host's overrides, and replaces
// the tools with the parent's less the task tools. The child's recorded
// Agent.Tools selects from them.
func (ts *Tasks) childRunner(
	sub Subagent,
	ref dashi.CallRef,
	parent dashi.Runner,
) dashi.Runner {
	over := ts.cfg.Runner
	runner := parent
	if over.Client != nil {
		runner.Client = over.Client
	}
	if over.Recorder != nil {
		runner.Recorder = over.Recorder
	}
	// Close interrupts and Stop cancels by ending the child's context, and a
	// resume must find an interrupted record, not a final one.
	runner.OnContextDone = dashi.ContextInterrupt
	runner.Tools = withoutTaskTools{parent.Tools}
	template := over.Observer
	onChild := ts.cfg.OnChild
	runner.Observer = nil
	if onChild != nil || template != nil {
		runner.Observer = func(e dashi.Event) {
			if onChild != nil {
				onChild(ChildEvent{Parent: ref, Subagent: sub.Name, Event: e})
			}
			if template != nil {
				template(e)
			}
		}
	}
	return runner
}
