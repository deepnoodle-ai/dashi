package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// taskHost retains notice and message delivery candidates; only the parent
// Engine driver changes the session queue. Child observer callbacks never
// drive a parent.
type taskHost struct {
	tasks        *delegate.Tasks
	inputs       *taskInputs
	mu           sync.Mutex
	recoverMu    sync.Mutex
	recovered    bool
	recoveryErr  error
	inventoryErr error
	deliveryErr  error
	// notices holds the task notices and the tasks' messages that wait
	// for the parent, by item ID.
	notices     map[string]dashi.ContextItem
	queued      map[string]bool
	rescan      bool
	recorded    map[string]taskSnapshot
	problems    map[string]string
	wake        func()
	observedSeq map[string]int64
}

func (e *Engine) newTaskHost(chat *session.Session, resolver *engineAttachmentResolver) *taskHost {
	loader, _ := chat.Runner.Recorder.(dashi.Loader)
	host := &taskHost{notices: map[string]dashi.ContextItem{}, queued: map[string]bool{}, observedSeq: map[string]int64{}}
	host.inputs = &taskInputs{loader: loader, parentBind: resolver.bind, clientFor: func(ctx context.Context, agent dashi.Agent) (llm.Client, error) {
		if p := e.cfg.Preferences; p != nil {
			return p.clientFor(ctx, agent)
		}
		if agent.Provider != e.cfg.Client.Provider() || agent.API != e.cfg.Client.API() {
			return nil, errors.New("recorded task destination is unavailable")
		}
		if _, mutable := e.cfg.Client.(*turnClient); mutable {
			return nil, errors.New("task needs a recorded immutable destination")
		}
		return e.cfg.Client, nil
	}}
	host.tasks = delegate.NewTasks(delegate.Config{
		PrepareStart: host.inputs.prepareStart, BindServices: host.inputs.bindServices,
		NoticeReady: taskNoticeReady,
		OnChild: func(event delegate.ChildEvent) {
			if event.Event.Step != nil {
				host.mu.Lock()
				seq := host.observedSeq[event.Event.TurnID]
				if seq >= event.Event.Seq {
					host.mu.Unlock()
					return
				}
				host.observedSeq[event.Event.TurnID] = event.Event.Seq
				host.mu.Unlock()
				view := delegate.TaskView{ID: delegate.TaskID(delegate.ChildID(event.Parent.TurnID, event.Parent.CallID)), TurnID: event.Event.TurnID, Subagent: event.Subagent, State: delegate.TaskRunning}
				if event.Event.Step.Stop != nil {
					view.State = delegate.TaskState(event.Event.Step.Stop.Kind)
					view.Reason = event.Event.Step.Stop.Reason
				}
				for _, current := range host.tasks.List() {
					if current.TurnID == view.TurnID {
						view = current
						break
					}
				}
				if event.Event.Step.Stop != nil {
					view.State = delegate.TaskState(event.Event.Step.Stop.Kind)
					view.Reason = event.Event.Step.Stop.Reason
				}
				e.cfg.Emit(taskStatusBlock(view))
			}
		},
		OnTask: func(event delegate.TaskEvent) {
			var item dashi.ContextItem
			switch event.Kind {
			case delegate.TaskMessage:
				// A task's message waits with the notices and reaches the
				// parent the same way: in a queue turn once the parent is
				// idle, or in a task_status read before that. Steering it
				// into a running parent turn could deliver it twice, since
				// such a read also returns its text.
				if event.Message == nil {
					return
				}
				item = event.Message.Item()
			case delegate.TaskEnded:
				e.cfg.Emit(taskStatusBlock(event.Task))
				item = event.Task.Notice().Item()
			default:
				e.cfg.Emit(taskStatusBlock(event.Task))
				return
			}
			host.mu.Lock()
			// Messages are few (delegate bounds them per turn), so the cap
			// is on notices, which a rescan can rebuild.
			if len(host.notices) < 128 || event.Kind == delegate.TaskMessage {
				host.notices[item.ID] = item
			} else {
				host.rescan = true
			}
			wake := host.wake
			if wake == nil {
				wake = e.taskWake
			}
			host.mu.Unlock()
			if wake != nil {
				go wake()
			}
		},
	})
	chat.Runner.Tools = tool.Join(chat.Runner.Tools, host.tasks.Tools())
	return host
}
func taskNoticeReady(v dashi.TurnView) bool {
	return v.Stop.Ended || v.Stop.Kind == dashi.StopCompleted || v.Stop.Kind == dashi.StopCanceled
}

func (e *Engine) taskHost() *taskHost { e.mu.Lock(); defer e.mu.Unlock(); return e.tasks }
func (e *Engine) recoverTasks(ctx context.Context) error {
	host := e.taskHost()
	if host == nil {
		return nil
	}
	host.recoverMu.Lock()
	defer host.recoverMu.Unlock()
	host.mu.Lock()
	recovered := host.recovered
	if host.recoveryErr != nil {
		err := host.recoveryErr
		host.mu.Unlock()
		return err
	}
	host.recovered = true
	host.mu.Unlock()
	chat := e.handle()
	turns, err := e.taskParents(ctx)
	if err != nil {
		host.mu.Lock()
		host.recoveryErr = err
		host.mu.Unlock()
		return err
	}
	if len(turns) == 0 {
		return nil
	}
	if err := host.readRecorded(ctx, turns); err != nil {
		host.mu.Lock()
		host.recoveryErr = err
		host.mu.Unlock()
		e.warn("Recorded task inspection incomplete; restore the affected child records and reopen before work: %v", err)
		return err
	}
	// Revalidate known records before a new driver action. Recovery starts
	// workers only once; a missing journal later cannot masquerade as quiet.
	if recovered {
		return nil
	}
	err = host.tasks.Recover(ctx, chat.Runner, turns)
	host.mu.Lock()
	host.recoveryErr = err
	host.mu.Unlock()
	if err != nil {
		e.warn("Task recovery needs attention; affected children were not dispatched: %v", err)
	}
	return nil
}

// FR-4 permits skipping damaged history only when durable child storage is
// absent. A child's directory precedes its first turn and any parent notice.
func (e *Engine) taskParents(ctx context.Context) ([]dashi.Turn, error) {
	host := e.taskHost()
	var turns []dashi.Turn
	skipped := map[string]error{}
	for turn, err := range e.handle().Turns(ctx) {
		if err == nil {
			turns = append(turns, turn)
			continue
		}
		if turn.ID == "" || ctx.Err() != nil {
			return nil, err
		}
		skipped[turn.ID] = err
		e.warn("Turn %d skipped: %v.", turnNumber(turn.ID), err)
	}
	known := host.tasks.List()
	notices := delegate.DeliveredNotices(turns)
	host.mu.Lock()
	for _, snapshot := range host.recorded {
		known = append(known, snapshot.Task)
	}
	for id := range host.notices {
		notices[id] = true
	}
	var incomplete []error
	var required []error
	for parentID, err := range skipped {
		incomplete = append(incomplete, fmt.Errorf("parent %s skipped: %w", parentID, err))
		for notice := range notices {
			prefix := delegate.NoticeID(delegate.ChildID(parentID, ""))
			if !strings.HasPrefix(notice, prefix) {
				continue
			}
			childTurn := strings.TrimPrefix(notice, "task-done:")
			split := strings.LastIndexByte(childTurn, '-')
			number, parseErr := strconv.Atoi(childTurn[split+1:])
			if split < 0 || parseErr != nil || number < 1 {
				required = append(required, fmt.Errorf("malformed recorded task notice %q", notice))
				continue
			}
			child := childTurn[:split]
			call := strings.TrimPrefix(child, delegate.ChildID(parentID, ""))
			known = append(known, delegate.TaskView{ID: delegate.TaskID(child), SessionID: child, TurnID: childTurn, Parent: dashi.CallRef{TurnID: parentID, CallID: call}})
		}
		for _, task := range known {
			if task.Parent.TurnID != parentID {
				continue
			}
			problem := fmt.Sprintf("required parent %s unavailable: %v; restore its journal and reopen", parentID, err)
			task.State, task.Reply, task.Reason = "record unavailable", "", problem
			if host.recorded == nil {
				host.recorded = map[string]taskSnapshot{}
			}
			if host.problems == nil {
				host.problems = map[string]string{}
			}
			host.recorded[task.ID] = taskSnapshot{Task: task}
			host.problems[task.ID] = problem
			required = append(required, fmt.Errorf("task %s: %s", task.ID, problem))
		}
	}
	host.inventoryErr = errors.Join(incomplete...)
	host.mu.Unlock()
	if len(skipped) > 0 && !e.cfg.Store.TaskStorageAbsent(e.Session()) {
		required = append(required, fmt.Errorf("task inventory incomplete: damaged parent history may hide child work; restore its journal and reopen, or use /new for a separate session"))
	}
	return turns, errors.Join(required...)
}

func (e *Engine) closeTasks() error {
	host := e.taskHost()
	if host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.tasks.Close(ctx); err != nil {
		return fmt.Errorf("task shutdown unresolved; session stays owned: %w", err)
	}
	host.inputs.close()
	return nil
}

func (e *Engine) flushTaskNotices(ctx context.Context) error {
	host := e.taskHost()
	if host == nil {
		return nil
	}
	if e.HasPending() {
		return nil
	}
	host.mu.Lock()
	pending := len(host.notices) > 0 || host.rescan
	host.mu.Unlock()
	if !pending {
		return nil
	}
	chat := e.handle()
	turns, err := e.taskParents(ctx)
	if err != nil {
		return err
	}
	host.mu.Lock()
	incomplete := host.inventoryErr
	host.mu.Unlock()
	if incomplete != nil {
		return fmt.Errorf("task notice delivery incomplete: %w", incomplete)
	}
	delivered := delegate.DeliveredNotices(turns)
	maps.Copy(delivered, delegate.DeliveredMessages(turns))
	host.mu.Lock()
	defer host.mu.Unlock()
	for id := range delivered {
		// The record shows the delivery, by a queue turn or by a
		// task_status read, so the held notice or message is not sent.
		delete(host.notices, id)
		delete(host.queued, id)
	}
	if host.rescan {
		for _, view := range host.tasks.List() {
			if view.Running() || delivered[delegate.NoticeID(view.TurnID)] {
				continue
			}
			turn, err := host.inputs.loader.Load(ctx, view.TurnID)
			if err != nil {
				return err
			}
			state, err := turn.View()
			if err != nil {
				return err
			}
			if !taskNoticeReady(state) {
				continue
			}
			item := view.Notice().Item()
			if len(host.notices) >= 128 {
				break
			}
			host.notices[item.ID] = item
		}
		host.rescan = len(host.notices) >= 128
	}
	ids := make([]string, 0, len(host.notices))
	for id := range host.notices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if host.queued[id] {
			continue
		}
		if _, err := chat.EnqueueItem(ctx, host.notices[id]); err != nil {
			return err
		}
		host.queued[id] = true // Submission is provisional; records alone remove it.
	}
	return nil
}

// DriveTaskNotices is called only by the Controller's parent driver. A stopped
// parent or a parent Start whose recording is unconfirmed holds notices
// without authorizing continuation.
func (e *Engine) DriveTaskNotices(ctx context.Context) (result Result, err error) {
	host := e.taskHost()
	if host != nil {
		defer func() {
			host.mu.Lock()
			host.deliveryErr = err
			host.mu.Unlock()
			if err != nil {
				e.warn("Task notice delivery paused; press Enter to retry recording it or reopen. No continuation was authorized: %v", err)
			}
		}()
	}

	e.driverMu.Lock()
	defer e.driverMu.Unlock()
	if err := e.recoverTasks(ctx); err != nil {
		return Result{}, err
	}
	if e.HasPending() {
		return Result{}, nil
	}
	last := e.Last(ctx)
	if last != nil && last.Open() {
		return Result{}, nil
	}
	if err := e.flushTaskNotices(ctx); err != nil {
		return Result{}, err
	}
	chat := e.configureChat()
	state, err := chat.Open(ctx)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		return Result{}, err
	}
	if state.Queued == 0 {
		return Result{}, nil
	}
	if p := e.cfg.Preferences; p != nil {
		if err := p.bind(ctx, p.agent()); err != nil {
			return Result{}, err
		}
	}
	before := state.Turns
	out, ran, err := chat.RunQueue(ctx)
	if ran {
		n := 0
		for turn, loadErr := range chat.Turns(ctx) {
			n++
			if n <= before {
				continue
			}
			if loadErr != nil {
				return Result{Outcome: out}, loadErr
			}
			for _, step := range turn.Steps {
				if step.Kind == dashi.StepTurnStarted {
					if saveErr := e.record(&step); saveErr != nil {
						e.warn("Task queue header was not saved: %v", saveErr)
					}
				}
			}
			blocks, renderErr := RenderTurn(n, e.Model(), e.cfg.Dir, turn, nil)
			if renderErr != nil {
				return Result{Outcome: out}, renderErr
			}
			for _, block := range blocks {
				e.cfg.Emit(block)
			}
		}
	}
	if err != nil {
		return Result{Outcome: out}, err
	}
	if err := e.flushTaskNotices(ctx); err != nil {
		return Result{Outcome: out}, err
	}
	return Result{Outcome: out}, nil
}

// QuietTasks drains permitted task notices. It never resumes a stopped parent,
// resolves a child effect with an unknown result, or raises any recorded limit.
func (e *Engine) QuietTasks(ctx context.Context, result Result) (Result, error) {
	if err := e.recoverTasks(ctx); err != nil {
		return result, err
	}
	host := e.taskHost()
	if host != nil {
		host.mu.Lock()
		recoveryErr := errors.Join(host.recoveryErr, host.inventoryErr)
		host.mu.Unlock()
		if recoveryErr != nil {
			return result, recoveryErr
		}
	}
	if host == nil || len(host.tasks.List()) == 0 {
		return result, nil
	}
	for {
		if last := e.Last(ctx); last != nil && last.Open() {
			return result, fmt.Errorf("parent %s needs %s before task delivery", last.View.ID, last.Next)
		}
		if result.Outcome.ID != "" && result.Outcome.Next() != dashi.NextNone {
			return result, fmt.Errorf("parent %s needs %s before task delivery", result.Outcome.ID, result.Outcome.Next())
		}
		idle, err := host.tasks.Wait(ctx)
		if err != nil {
			return result, err
		}
		next, err := e.DriveTaskNotices(ctx)
		if err != nil {
			return result, err
		}
		if next.Outcome.ID != "" {
			result = next
		}
		host.mu.Lock()
		pending := len(host.notices) > 0 || host.rescan
		host.mu.Unlock()
		if idle && !pending && e.Queued() == 0 {
			return result, nil
		}
	}
}

func (host *taskHost) readRecorded(ctx context.Context, parents []dashi.Turn) error {
	notices := delegate.DeliveredNotices(parents)
	recorded := map[string]taskSnapshot{}
	problems := map[string]string{}
	if host.inputs.loader == nil {
		return errors.New("task recorder cannot load authoritative records")
	}
	for _, parent := range parents {
		view, err := parent.View()
		if err != nil {
			return err
		}
		for callID, call := range view.Tools {
			if call.Call.Name != delegate.ToolName {
				continue
			}
			ref := dashi.CallRef{TurnID: parent.ID, CallID: callID}
			child := delegate.ChildID(parent.ID, callID)
			id := delegate.TaskID(child)
			var args struct {
				Subagent    string `json:"subagent"`
				Prompt      string `json:"prompt"`
				Description string `json:"description"`
			}
			if json.Unmarshal(call.Call.Args, &args) != nil {
				continue
			}
			task := delegate.TaskView{ID: id, SessionID: child, Parent: ref, Subagent: args.Subagent, Description: args.Description}
			if task.Description == "" {
				task.Description = args.Prompt
			}
			required := 0
			if call.Outcome != nil && call.Outcome.State == tool.StateSucceeded {
				required = 1
			}
			for notice := range notices {
				if suffix, ok := strings.CutPrefix(notice, "task-done:"+child+"-"); ok {
					if n, err := strconv.Atoi(suffix); err == nil && n > required {
						required = n
					}
				}
			}
			if index, ok := host.inputs.loader.(session.Index); ok {
				n, err := index.LastTurn(ctx, child)
				if err != nil {
					problems[id] = err.Error()
				} else if n > required {
					required = n
				}
			}
			var last taskSnapshot
			for n := 1; ; n++ {
				turn, err := host.inputs.loader.Load(ctx, session.TurnID(child, n))
				if errors.Is(err, dashi.ErrTurnNotFound) {
					if n <= required {
						problems[id] = fmt.Sprintf("required child Turn %s is missing; restore its journal and reopen", session.TurnID(child, n))
					}
					break
				}
				if err != nil {
					problems[id] = err.Error()
					break
				}
				state, err := turn.View()
				if err != nil {
					problems[id] = err.Error()
					break
				}
				// The task call starts turn 1. A message starts a later
				// turn, with the task_send call as its parent, or with
				// none for a queue turn.
				if n == 1 && (state.Parent == nil || *state.Parent != ref) {
					problems[id] = "child parent differs"
					break
				}
				task.Turns = n
				task.TurnID = turn.ID
				task.State = delegate.TaskState(state.Stop.Kind)
				task.Reason = state.Stop.Reason
				task.Spend = task.Spend.Add(state.Spend)
				task.ModelCalls += state.ModelCalls
				task.ToolCalls += len(state.Tools)
				if state.Stop.Kind == dashi.StopCompleted {
					task.Reply = state.Reply
				}
				last = taskSnapshot{Task: task, Turn: turn, View: state, Target: delegate.TaskTarget{ID: id, TurnID: turn.ID, LastSeq: state.LastSeq}}
			}
			if problems[id] != "" {
				// A damaged latest record cannot borrow an earlier stop as
				// completion. Keep the derived task visible with its result unknown.
				task.State = "record unavailable"
				task.Reply = ""
				task.Reason = problems[id]
				last.Task = task
				recorded[id] = last
			} else if last.Turn.ID != "" {
				recorded[id] = last
			}
		}
	}
	host.mu.Lock()
	host.recorded = recorded
	host.problems = problems
	host.mu.Unlock()
	var errs []error
	ids := make([]string, 0, len(problems))
	for id := range problems {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		errs = append(errs, fmt.Errorf("task %s: %s", id, problems[id]))
	}
	err := errors.Join(errs...)
	if err != nil {
		host.mu.Lock()
		host.recoveryErr = err
		host.mu.Unlock()
	}
	return err
}

// taskConversation loads the turns of task id and returns what /tasks ID
// prints (see taskConversation).
func (e *Engine) taskConversation(ctx context.Context, id string) (string, error) {
	host := e.taskHost()
	if host == nil {
		return "", errors.New("no task host is open")
	}
	if host.inputs.loader == nil {
		return "", errors.New("task recorder cannot load records")
	}
	for _, task := range host.list() {
		if task.ID != id {
			continue
		}
		var turns []dashi.Turn
		for n := 1; n <= max(task.Turns, 1); n++ {
			turn, err := host.inputs.loader.Load(ctx, session.TurnID(task.SessionID, n))
			if errors.Is(err, dashi.ErrTurnNotFound) {
				break
			}
			if err != nil {
				return "", err
			}
			turns = append(turns, turn)
		}
		return taskConversation(task, turns), nil
	}
	return "", delegate.ErrUnknownTask
}

func (host *taskHost) list() []delegate.TaskView {
	live := host.tasks.List()
	known := map[string]bool{}
	host.mu.Lock()
	defer host.mu.Unlock()
	for i, task := range live {
		known[task.ID] = true
		if host.problems[task.ID] != "" {
			live[i] = host.recorded[task.ID].Task
		}
	}
	for id, snapshot := range host.recorded {
		if !known[id] {
			live = append(live, snapshot.Task)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live
}
