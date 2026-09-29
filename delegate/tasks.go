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
	"github.com/deepnoodle-ai/dashi/tool"
)

// defaultMaxAnswer is the byte cap on an answer returned to the model when
// Config.MaxAnswer is zero. A longer answer would refill the context the
// model delegated to protect.
const defaultMaxAnswer = 16 << 10

// stopTimeout bounds the write that records a stop after Stop or Close.
const stopTimeout = 30 * time.Second

// TaskState is "running" or the child's dashi.StopKind: completed, failed,
// limited, refused, canceled, interrupted, waiting, or uncertain. No other
// names exist.
type TaskState string

// TaskRunning is the state of a task whose child is working.
const TaskRunning TaskState = "running"

// TaskEventKind says what happened to a task.
type TaskEventKind string

// Task event kinds.
const (
	// TaskStarted means the task started, or resumed after Recover.
	TaskStarted TaskEventKind = "started"
	// TaskEnded means the task reached a state other than running and
	// Close did not cause it.
	TaskEnded TaskEventKind = "ended"
)

// TaskEvent is one change of a task, for Config.OnTask.
type TaskEvent struct {
	Kind TaskEventKind
	Task TaskView
}

// TaskView is a copy of the state of one task.
type TaskView struct {
	// ID is what the model and the user call the task, such as "t_k3f2".
	ID string
	// ChildID is the turn ID of the child.
	ChildID     string
	Subagent    string
	Description string
	// Parent is the tool call of the parent turn that started the task.
	Parent dashi.CallRef
	State  TaskState
	// ModelCalls and ToolCalls count the child's work so far.
	ModelCalls int
	ToolCalls  int
	// Spend is the child's spend. It is live for a running task.
	Spend dashi.Spend
	// Answer is the child's answer, cut at Config.MaxAnswer. It is empty
	// unless the task completed.
	Answer string
	// Reason says why a task that did not complete stopped.
	Reason string
}

// Running reports whether the task is still working.
func (v TaskView) Running() bool {
	return v.State == TaskRunning
}

// TaskID returns the ID of the task for a child turn. It is a fixed function
// of the child ID, so it is the same after a reopen and needs no counter.
func TaskID(childID string) string {
	sum := sha256.Sum256([]byte(childID))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(sum[:])
	return "t_" + strings.ToLower(enc[:6])
}

// Errors of Tasks.Stop.
var (
	// ErrUnknownTask means Tasks holds no task with that ID.
	ErrUnknownTask = errors.New("delegate: unknown task")
	// ErrTaskEnded means the task is no longer running.
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
	changed chan struct{} // closed and replaced when a task ends or reports
	reports int           // reports finished
	waited  int           // reports that Wait has returned for
	wg      sync.WaitGroup
}

// task is the mutable record of one child. Tasks.mu guards every field
// except the immutable ones in the first block.
type task struct {
	id, childID string
	parent      dashi.CallRef
	sub         Subagent
	description string

	cancel   context.CancelFunc
	state    TaskState
	stopping bool
	// notified is set when a report starts, reported when it is done. An
	// ended task counts as pending until reported.
	notified bool
	reported bool
	steps    []dashi.Step // observed while running
	final    *dashi.TurnView
	answer   string
	reason   string
	done     chan struct{}
	// started closes when the child's record holds its first step, or when
	// the run ends without one.
	started chan struct{}
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
		out = append(out, ts.viewLocked(ts.tasks[id]))
	}
	return out
}

// Stop ends a running task and records the child as canceled. A canceled
// child is final: Recover does not resume it. Stop returns after the child
// has stopped, or with the error of ctx if that ends first (the child keeps
// stopping). It returns ErrUnknownTask or ErrTaskEnded when there is nothing
// to stop.
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
	if t.state != TaskRunning {
		ts.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrTaskEnded, t.state)
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
// wait for. A task counts as pending from its start until OnTask has
// returned for its end, so a host that sends the notice from OnTask sees it
// in its inbox by the time Wait says there is nothing pending.
//
// Wait returns false as soon as one more task has been reported since the
// last return. It returns true when no task is pending. A host loop reads
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
		if ts.pendingLocked() == 0 {
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

// pendingLocked counts the tasks that are running or whose end OnTask has
// not yet reported. A task that Close interrupted is not pending.
func (ts *Tasks) pendingLocked() int {
	n := 0
	for _, t := range ts.tasks {
		if !t.reported {
			n++
		}
	}
	return n
}

// reportedLocked marks a task as reported. count says whether the report
// is news to Wait.
func (ts *Tasks) reportedLocked(t *task, count bool) {
	t.notified, t.reported = true, true
	if count {
		ts.reports++
	}
	close(ts.changed)
	ts.changed = make(chan struct{})
}

// Close interrupts every running child and waits for each to record that. An
// interrupted child stays resumable, so the next Recover continues it. Close
// does not call OnTask for these children. After Close, the tool refuses new
// tasks. If ctx ends first, Close returns its error and the children keep
// stopping.
func (ts *Tasks) Close(ctx context.Context) error {
	ts.mu.Lock()
	ts.closed = true
	for _, t := range ts.tasks {
		if t.state == TaskRunning && t.cancel != nil {
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
// host holds for it. It finds each turn's task calls, loads the children by
// ChildID, and adds them to the list. It resumes a child that has no
// recorded stop or stopped interrupted. For a child that already ended, it
// calls OnTask with TaskEnded unless the record holds the notice (see
// NoticeID and FollowUp) or an earlier call already reported the child. The
// runner is the one the host uses for the conversation. It needs a recorder
// that implements dashi.Loader and returns an error that says so if it does
// not. It skips a child whose subagent the catalog no longer holds and
// returns an error for it after the rest.
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
			if err := ts.adopt(ctx, r, loader, ref, call.Args, delivered); err != nil {
				errs = append(errs, err)
			}
		}
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
) error {
	childID := ChildID(ref.TurnID, ref.CallID)
	child, err := loader.Load(ctx, childID)
	if errors.Is(err, dashi.ErrTurnNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delegate: load child %s: %w", childID, err)
	}
	view, err := dashi.Replay(child)
	if err != nil {
		return fmt.Errorf("delegate: replay child %s: %w", childID, err)
	}
	args, _ := parseTaskArgs(rawArgs)
	sub, ok := ts.find(args.Subagent)
	if !ok {
		return fmt.Errorf(
			"delegate: task %s uses subagent %q, which the catalog lacks",
			TaskID(childID), args.Subagent,
		)
	}
	t, added, err := ts.register(ctx, sub, ref, args.Description, parent, child, view)
	if err != nil {
		return err
	}
	if !added {
		// Known in this process: report an end that nobody has heard of.
		ts.notifyEnded(t, delivered)
		return nil
	}
	if !t.resumable(child) {
		ts.notifyEnded(t, delivered)
	}
	return nil
}

// register adds the task for a child that already has a record. For a
// resumable child it also starts the goroutine. It reports whether the task
// is new. An ended child is registered as ended.
func (ts *Tasks) register(
	ctx context.Context,
	sub Subagent,
	ref dashi.CallRef,
	description string,
	parent dashi.Runner,
	child dashi.Turn,
	view dashi.TurnView,
) (*task, bool, error) {
	t := newTask(sub, ref, description)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if old, err := ts.existingLocked(t); old != nil || err != nil {
		return old, false, err
	}
	if ts.closed {
		return nil, false, errors.New("delegate: tasks are closed")
	}
	ts.addLocked(t)
	if !t.resumable(child) {
		v := view
		t.final = &v
		t.state, t.answer, t.reason = endState(ts, mustStop(child), v)
		t.setEnded()
		return t, true, nil
	}
	t.steps = slices.Clone(child.Steps)
	runner := ts.childRunner(sub, ref, parent, t)
	// Continue with the agent the child recorded, so a change to the
	// subagent since then cannot make the record and the runner disagree.
	// startTask does the same.
	runner.Agent = view.Agent
	ts.launchLocked(ctx, t, runner, child, dashi.Continue{})
	return t, true, nil
}

func newTask(sub Subagent, ref dashi.CallRef, description string) *task {
	childID := ChildID(ref.TurnID, ref.CallID)
	return &task{
		id:          TaskID(childID),
		childID:     childID,
		parent:      ref,
		sub:         sub,
		description: description,
		state:       TaskRunning,
		done:        make(chan struct{}),
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

func (t *task) setEnded() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
}

// resumable reports whether the record shows an unfinished child: no
// recorded stop, or an interruption.
func (t *task) resumable(child dashi.Turn) bool {
	stop, ok := recordedStop(child)
	return !ok || stop.Kind == dashi.StopInterrupted
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
// when that task belongs to another child.
func (ts *Tasks) existingLocked(t *task) (*task, error) {
	old, ok := ts.tasks[t.id]
	if !ok {
		return nil, nil
	}
	if old.childID != t.childID {
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

// launchLocked runs the child in a goroutine that Tasks owns. The caller
// holds ts.mu and has checked that ts is not closed. The child keeps the
// values of base but not its cancellation or deadline, so canceling the
// parent turn does not stop the task.
func (ts *Tasks) launchLocked(
	base context.Context,
	t *task,
	runner dashi.Runner,
	turn dashi.Turn,
	cmd dashi.Command,
) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(base))
	t.cancel = cancel
	ts.wg.Add(1)
	go ts.run(ctx, t, runner, turn, cmd)
}

func (ts *Tasks) run(
	ctx context.Context,
	t *task,
	runner dashi.Runner,
	turn dashi.Turn,
	cmd dashi.Command,
) {
	defer ts.wg.Done()
	if started := ts.view(t); ts.cfg.OnTask != nil {
		ts.cfg.OnTask(TaskEvent{Kind: TaskStarted, Task: started})
	}
	out, err := runner.Run(ctx, turn, cmd)
	ts.mu.Lock()
	stopping := t.stopping
	ts.mu.Unlock()
	if stopping {
		out, err = ts.recordCancel(runner, t, out, err)
	}
	ts.finish(t, out, err)
}

// recordCancel records the stop that Stop asked for. A child that already
// ended for another reason keeps that reason.
func (ts *Tasks) recordCancel(
	runner dashi.Runner,
	t *task,
	out dashi.Outcome,
	runErr error,
) (dashi.Outcome, error) {
	if runErr == nil && out.Stop.Kind != dashi.StopInterrupted &&
		out.Stop.Kind != dashi.StopWaiting {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	turn := out.Turn
	if runErr != nil || len(turn.Steps) == 0 {
		loader, ok := runner.Recorder.(dashi.Loader)
		if !ok {
			return dashi.Outcome{}, errCanceled
		}
		loaded, err := loader.Load(ctx, t.childID)
		if err != nil {
			return dashi.Outcome{}, errCanceled
		}
		turn = loaded
	}
	canceled, err := runner.Run(ctx, turn, dashi.Cancel{
		CommandID: t.childID + "/cancel",
		Reason:    "stopped on request",
	})
	if err != nil {
		return dashi.Outcome{}, errCanceled
	}
	return canceled, nil
}

// errCanceled marks a stop that the record does not show.
var errCanceled = errors.New("stopped on request")

func (ts *Tasks) finish(t *task, out dashi.Outcome, err error) {
	ts.mu.Lock()
	switch {
	case errors.Is(err, errCanceled):
		t.state, t.reason = TaskState(dashi.StopCanceled), err.Error()
	case err != nil && ts.closed:
		t.state = TaskState(dashi.StopInterrupted)
	case err != nil:
		t.state = TaskState(dashi.StopFailed)
		t.reason = "the child could not run: " + err.Error()
	default:
		view := out.TurnView
		t.final = &view
		t.state, t.answer, t.reason = endState(ts, out.Stop, view)
	}
	t.setEnded()
	t.setStarted()
	interrupted := t.state == TaskState(dashi.StopInterrupted)
	if interrupted {
		// Close is not an end: nothing will be reported.
		t.notified, t.reported = true, true
	}
	close(ts.changed)
	ts.changed = make(chan struct{})
	t.cancel()
	ts.mu.Unlock()
	if !interrupted {
		ts.notifyEnded(t, nil)
	}
}

// endState maps the child's stop to the task state, answer, and reason.
func endState(
	ts *Tasks,
	stop dashi.Stop,
	view dashi.TurnView,
) (TaskState, string, string) {
	state := TaskState(stop.Kind)
	if stop.Kind != dashi.StopCompleted {
		return state, "", stop.Reason
	}
	text := view.Answer
	if view.Output != nil && len(view.Output.Value) > 0 {
		text = string(view.Output.Value)
	}
	if strings.TrimSpace(text) == "" {
		text = "The subagent finished without a written answer."
	}
	return state, ts.capAnswer(text), ""
}

// notifyEnded calls OnTask once for an ended task, unless delivered holds
// its child. The task stays pending until OnTask has returned.
func (ts *Tasks) notifyEnded(t *task, delivered map[string]bool) {
	ts.mu.Lock()
	if t.notified || t.state == TaskRunning {
		ts.mu.Unlock()
		return
	}
	if delivered[t.childID] {
		ts.reportedLocked(t, false)
		ts.mu.Unlock()
		return
	}
	t.notified = true
	v := ts.viewLocked(t)
	ts.mu.Unlock()
	if ts.cfg.OnTask != nil {
		ts.cfg.OnTask(TaskEvent{Kind: TaskEnded, Task: v})
	}
	ts.mu.Lock()
	ts.reportedLocked(t, true)
	ts.mu.Unlock()
}

func (ts *Tasks) view(t *task) TaskView {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.viewLocked(t)
}

func (ts *Tasks) viewLocked(t *task) TaskView {
	v := TaskView{
		ID:          t.id,
		ChildID:     t.childID,
		Subagent:    t.sub.Name,
		Description: t.description,
		Parent:      t.parent,
		State:       t.state,
		Answer:      t.answer,
		Reason:      t.reason,
	}
	var tv dashi.TurnView
	switch {
	case t.final != nil:
		tv = *t.final
	case len(t.steps) > 0:
		if r, err := dashi.Replay(dashi.Turn{ID: t.childID, Steps: t.steps}); err == nil {
			tv = r
		}
	}
	v.ModelCalls = tv.ModelAttempts
	v.ToolCalls = len(tv.Tools)
	v.Spend = tv.Spend.Clone()
	return v
}

// observe records a step of a running child so a status read can show live
// counts and cost.
func (ts *Tasks) observe(t *task, e dashi.Event) {
	if e.Step == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t.setStarted()
	if t.final != nil {
		return
	}
	if n := len(t.steps); n > 0 && t.steps[n-1].Seq >= e.Step.Seq {
		return
	}
	t.steps = append(t.steps, *e.Step)
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
	limit := cmp.Or(ts.cfg.MaxAnswer, defaultMaxAnswer)
	if limit < 0 || len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%s\n[answer cut at %d bytes]", cut, len(cut))
}

// startTask starts the child for one task call, or returns the task that
// already holds it.
func (ts *Tasks) startTask(
	ctx context.Context,
	sub Subagent,
	args taskArgs,
	ref dashi.CallRef,
	parent dashi.Runner,
) (TaskView, error) {
	t := newTask(sub, ref, args.Description)
	runner := ts.childRunner(sub, ref, parent, t)
	if !loads(runner.Recorder) {
		ts.warnNoLoader()
	}
	// A retry after a crash finds a child the record already holds.
	var saved dashi.Turn
	var view dashi.TurnView
	found := false
	if loader, ok := runner.Recorder.(dashi.Loader); ok {
		turn, err := loader.Load(ctx, t.childID)
		switch {
		case errors.Is(err, dashi.ErrTurnNotFound):
		case err != nil:
			return TaskView{}, fmt.Errorf("delegate: load child: %w", err)
		default:
			view, err = dashi.Replay(turn)
			if err != nil {
				return TaskView{}, fmt.Errorf("delegate: replay child: %w", err)
			}
			saved, found = turn, true
		}
	}
	ts.mu.Lock()
	if old, err := ts.existingLocked(t); old != nil || err != nil {
		defer ts.mu.Unlock()
		if err != nil {
			return TaskView{}, err
		}
		return ts.viewLocked(old), nil
	}
	if ts.closed {
		ts.mu.Unlock()
		return TaskView{}, errors.New("delegate: tasks are closed")
	}
	ts.addLocked(t)
	endedBefore := false
	switch {
	case !found:
		start := dashi.Start{
			CommandID: t.childID + "/start",
			Parent:    &ref,
			Input:     []llm.Message{llm.UserMessage(llm.Text(args.Prompt))},
		}
		ts.launchLocked(ctx, t, runner, dashi.Turn{ID: t.childID}, start)
	case t.resumable(saved):
		t.steps = slices.Clone(saved.Steps)
		runner.Agent = view.Agent
		ts.launchLocked(ctx, t, runner, saved, dashi.Continue{})
	default:
		// The child ended before the crash. Report the end here, so the
		// result does not depend on whether the host called Recover first.
		v := view
		t.final = &v
		t.state, t.answer, t.reason = endState(ts, mustStop(saved), v)
		t.setEnded()
		t.setStarted()
		endedBefore = true
	}
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
	if t.state != TaskRunning && len(t.steps) == 0 && t.final == nil {
		// The run failed before it recorded the child.
		if !t.notified {
			ts.reportedLocked(t, false)
		}
		return TaskView{}, tool.Fatal(fmt.Errorf(
			"delegate: start subagent: %s", cmp.Or(t.reason, string(t.state)),
		))
	}
	return ts.viewLocked(t), nil
}

func mustStop(t dashi.Turn) dashi.Stop {
	s, _ := recordedStop(t)
	return s
}

// childRunner builds the runner for one child. It starts from the runner that
// runs the task call, applies the host's overrides, and replaces the agent and
// tools. A Subagent that sets no model uses the parent's current model.
func (ts *Tasks) childRunner(
	sub Subagent,
	ref dashi.CallRef,
	parent dashi.Runner,
	t *task,
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
	agent := sub.Agent
	if agent.Model == "" && agent.Provider == "" && agent.API == "" {
		agent.Provider = parent.Agent.Provider
		agent.API = parent.Agent.API
		agent.Model = parent.Agent.Model
	}
	agent.ID = cmp.Or(agent.ID, sub.Name)
	runner.Agent = agent
	runner.Tools = filtered{parent: parent.Tools, sub: sub}
	template := over.Observer
	onChild := ts.cfg.OnChild
	runner.Observer = func(e dashi.Event) {
		ts.observe(t, e)
		if onChild != nil {
			onChild(ChildEvent{Parent: ref, Subagent: sub.Name, Event: e})
		}
		if template != nil {
			template(e)
		}
	}
	return runner
}
