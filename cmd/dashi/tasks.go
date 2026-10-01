package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// taskHost retains notice delivery candidates; only the parent Engine driver
// changes the session queue. Child observer callbacks never drive a parent.
type taskHost struct {
	tasks       *delegate.Tasks
	inputs      *taskInputs
	mu          sync.Mutex
	recoverMu   sync.Mutex
	recovered   bool
	recoveryErr error
	deliveryErr error
	notices     map[string]dashi.ContextItem
	queued      map[string]bool
	rescan      bool
	recorded    map[string]taskSnapshot
	problems    map[string]string
	wake        func()
	observedSeq map[string]int64
}

func (e *Engine) newTaskHost(chat *dashi.Session, resolver *engineAttachmentResolver) *taskHost {
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
				e.cfg.Emit(taskRow(view))
			}
		},
		OnTask: func(event delegate.TaskEvent) {
			e.cfg.Emit(taskRow(event.Task))
			if event.Kind == delegate.TaskEnded {
				item := event.Task.Notice().Item()
				host.mu.Lock()
				if len(host.notices) < 128 {
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
			}
		},
	})
	chat.Runner.Tools = tool.Join(chat.Runner.Tools, host.tasks.Tools())
	return host
}
func taskNoticeReady(v dashi.TurnView) bool {
	return v.Stop.Terminal || v.Stop.Kind == dashi.StopCompleted || v.Stop.Kind == dashi.StopCanceled
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
	var turns []dashi.Turn
	for turn, err := range chat.Turns(ctx) {
		if err != nil {
			host.mu.Lock()
			host.recoveryErr = err
			host.mu.Unlock()
			return err
		}
		turns = append(turns, turn)
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
	err := host.tasks.Recover(ctx, chat.Runner, turns)
	host.mu.Lock()
	host.recoveryErr = err
	host.mu.Unlock()
	if err != nil {
		e.warn("Task recovery needs attention; affected children were not dispatched: %v", err)
	}
	return nil
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

func recordedTaskNotices(turns []dashi.Turn) map[string]bool {
	delivered := map[string]bool{}
	for _, turn := range turns {
		for _, step := range turn.Steps {
			for _, item := range step.Items() {
				if strings.HasPrefix(item.ID, "task-done:") {
					delivered[item.ID] = true
				}
			}
			if step.Kind == dashi.StepStarted {
				if list, ok := strings.CutPrefix(step.CommandID, "queue:"); ok {
					var ids []string
					if json.Unmarshal([]byte(list), &ids) == nil {
						for _, id := range ids {
							if strings.HasPrefix(id, "task-done:") {
								delivered[id] = true
							}
						}
					}
				}
			}
		}
	}
	return delivered
}

func (e *Engine) flushTaskNotices(ctx context.Context) error {
	host := e.taskHost()
	if host == nil {
		return nil
	}
	if e.HasPending() {
		return nil
	}
	chat := e.handle()
	var turns []dashi.Turn
	for turn, err := range chat.Turns(ctx) {
		if err != nil {
			return err
		}
		turns = append(turns, turn)
	}
	delivered := recordedTaskNotices(turns)
	host.mu.Lock()
	defer host.mu.Unlock()
	for id := range delivered {
		if _, pending := host.notices[id]; pending {
			e.cfg.Emit(Block{ID: "task-delivery", Kind: KindNotice, Text: "Task result delivery is recorded; reopening will not deliver it again.", Done: true})
		}
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
			state, err := dashi.Replay(turn)
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
// parent or pending admission holds notices without authorizing continuation.
func (e *Engine) DriveTaskNotices(ctx context.Context) (result Result, err error) {
	host := e.taskHost()
	if host != nil {
		defer func() {
			host.mu.Lock()
			host.deliveryErr = err
			host.mu.Unlock()
			if err != nil {
				e.warn("Task notice delivery paused; press Enter to retry its recorded admission or reopen. No continuation was authorized: %v", err)
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
	if err != nil && !errors.Is(err, dashi.ErrSessionNotFound) {
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
				if step.Kind == dashi.StepStarted {
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
// resolves an uncertain child effect, or raises any recorded limit.
func (e *Engine) QuietTasks(ctx context.Context, result Result) (Result, error) {
	if err := e.recoverTasks(ctx); err != nil {
		return result, err
	}
	host := e.taskHost()
	if host != nil {
		host.mu.Lock()
		recoveryErr := host.recoveryErr
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
	notices := recordedTaskNotices(parents)
	recorded := map[string]taskSnapshot{}
	problems := map[string]string{}
	if host.inputs.loader == nil {
		return errors.New("task recorder cannot load authoritative records")
	}
	for _, parent := range parents {
		view, err := dashi.Replay(parent)
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
			if index, ok := host.inputs.loader.(dashi.SessionIndex); ok {
				n, err := index.LastTurn(ctx, child)
				if err != nil {
					problems[id] = err.Error()
				} else if n > required {
					required = n
				}
			}
			var last taskSnapshot
			for n := 1; ; n++ {
				turn, err := host.inputs.loader.Load(ctx, dashi.TurnID(child, n))
				if errors.Is(err, dashi.ErrTurnNotFound) {
					if n <= required {
						problems[id] = fmt.Sprintf("required child Turn %s is missing; restore its journal and reopen", dashi.TurnID(child, n))
					}
					break
				}
				if err != nil {
					problems[id] = err.Error()
					break
				}
				state, err := dashi.Replay(turn)
				if err != nil {
					problems[id] = err.Error()
					break
				}
				if state.Parent == nil || *state.Parent != ref {
					problems[id] = "child parent differs"
					break
				}
				task.Turns = n
				task.TurnID = turn.ID
				task.State = delegate.TaskState(state.Stop.Kind)
				task.Reason = state.Stop.Reason
				task.Spend = task.Spend.Add(state.Spend)
				task.ModelCalls += state.ModelAttempts
				task.ToolCalls += len(state.Tools)
				if state.Stop.Kind == dashi.StopCompleted {
					task.Answer = state.Answer
				}
				last = taskSnapshot{Task: task, Turn: turn, View: state, Target: delegate.TaskTarget{ID: id, TurnID: turn.ID, Revision: state.Revision}}
			}
			if problems[id] != "" {
				// A damaged latest record cannot borrow an earlier stop as
				// completion. Keep the derived task visible with uncertainty.
				task.State = "record unavailable"
				task.Answer = ""
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
