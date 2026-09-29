package delegate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	parentTurn = "parent-turn"
	callID     = "call-1"
)

// modelFunc answers one model request. The system text tells parent
// requests from child requests.
type modelFunc func(ctx context.Context, in llm.Input) (llm.Result, error)

type fakeClient struct {
	handle modelFunc
	mu     sync.Mutex
	inputs []llm.Input
}

func (*fakeClient) Provider() string {
	return "test"
}

func (*fakeClient) API() string {
	return "test.chat"
}

func (*fakeClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c *fakeClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{"ok":true}`),
	})
}

func (c *fakeClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	in := call.Request.Snapshot().Input
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	c.mu.Unlock()
	result, err := c.handle(ctx, in)
	result.Requested = llm.ModelRef{
		Provider: "test",
		Model:    call.Request.Snapshot().Origin.Model,
	}
	return result, err
}

// sends counts model requests whose system text is system.
func (c *fakeClient) sends(system string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, in := range c.inputs {
		if role(in) == system {
			n++
		}
	}
	return n
}

func (c *fakeClient) last(system string) llm.Input {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.inputs) - 1; i >= 0; i-- {
		if role(c.inputs[i]) == system {
			return c.inputs[i]
		}
	}
	return llm.Input{}
}

// role names the agent that made a request: its instructions come first in
// the system text.
func role(in llm.Input) string {
	first, _, _ := strings.Cut(in.System, "\n")
	return first
}

func text(s string) llm.Result {
	return llm.Result{
		Message: llm.AssistantMessage(llm.Text(s)),
		Stop:    llm.Stop{Kind: llm.StopFinished},
	}
}

func costed(r llm.Result, total string) llm.Result {
	r.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 5}
	r.Cost = &llm.Cost{
		Total:    money.MustParseAmount(total),
		Currency: "USD",
		Source:   llm.CostSourceProviderReported,
	}
	return r
}

func toolCall(id, name, args string) llm.Result {
	call := tool.Call{ID: id, Name: name, Args: json.RawMessage(args)}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop: llm.Stop{Kind: llm.StopToolUse},
	}
}

func taskCall(prompt string) llm.Result {
	args, _ := json.Marshal(map[string]string{
		"subagent":    "scout",
		"prompt":      prompt,
		"description": "look",
	})
	return toolCall(callID, delegate.ToolName, string(args))
}

// gate holds a child model call until the test releases it. The call ends
// early when its context ends, as a real provider call would.
type gate struct {
	started chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (g *gate) open() {
	close(g.release)
}

func (g *gate) wait(ctx context.Context) error {
	select {
	case g.started <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the gated call never started")
	}
}

func inputText(in llm.Input) string {
	var sb strings.Builder
	for _, m := range in.Messages {
		sb.WriteString(m.Text() + "\n")
	}
	return sb.String()
}

// parentAfterTask answers parent calls after the task call settled.
func parentAfterTask(_ context.Context, in llm.Input) (llm.Result, error) {
	if strings.Contains(inputText(in), "[task t_") {
		return costed(text("parent read the notice"), "1"), nil
	}
	return costed(text("parent done"), "1"), nil
}

// scripted is the default model: the parent delegates once and then
// finishes, and child answers each child request.
func scripted(child modelFunc) modelFunc {
	return func(ctx context.Context, in llm.Input) (llm.Result, error) {
		switch {
		case role(in) != "parent":
			return child(ctx, in)
		case len(in.Messages) == 1 && !strings.Contains(inputText(in), "[task t_"):
			return costed(taskCall("find the parser"), "1"), nil
		}
		return parentAfterTask(ctx, in)
	}
}

func answers(a string) modelFunc {
	return func(context.Context, llm.Input) (llm.Result, error) {
		return costed(text(a), "0.25"), nil
	}
}

// script is the default model with an immediate child answer.
func script(childAnswer string) modelFunc {
	return scripted(answers(childAnswer))
}

// gated makes the child wait at g, then answer.
func gated(g *gate, answer string) modelFunc {
	return func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return answers(answer)(ctx, in)
	}
}

func agent(system string) dashi.Agent {
	return dashi.Agent{
		ID:           system,
		Revision:     "r1",
		Provider:     "test",
		API:          "test.chat",
		Model:        "fixture",
		Instructions: system,
	}
}

func scout() delegate.Subagent {
	return delegate.Subagent{
		Name:        "scout",
		Description: "Looks around.",
		Agent:       agent("child"),
	}
}

func yes() *bool {
	v := true
	return &v
}

func probe(name string, readOnly *bool, ran *atomic.Int32) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:        name,
			Description: "A probe.",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Annotations: tool.Annotations{ReadOnly: readOnly},
		},
		Executor: executor(func(
			context.Context,
			tool.Call,
		) (tool.Outcome, error) {
			ran.Add(1)
			return tool.Succeeded("probed"), nil
		}),
	}
}

type executor func(context.Context, tool.Call) (tool.Outcome, error)

func (f executor) Execute(
	ctx context.Context,
	c tool.Call,
) (tool.Outcome, error) {
	return f(ctx, c)
}

// crashRecorder fails every commit once the trigger matches, as a process
// that died at that point would. Revive lets a restarted worker commit.
type crashRecorder struct {
	dashi.Recorder
	trigger func(dashi.CommitRequest) bool
	dead    atomic.Bool
	starts  atomic.Int32
}

func (c *crashRecorder) Load(
	ctx context.Context,
	id string,
) (dashi.Turn, error) {
	return c.Recorder.(dashi.Loader).Load(ctx, id)
}

func (c *crashRecorder) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	if c.dead.Load() || c.trigger != nil && c.trigger(req) {
		c.dead.Store(true)
		return &dashi.CommitError{
			Persistence: dashi.PersistenceRejected,
			Err:         errors.New("crashed"),
		}
	}
	if req.Step.Kind == dashi.StepStarted &&
		strings.HasPrefix(req.Step.TurnID, "child-") {
		c.starts.Add(1)
	}
	return c.Recorder.Commit(ctx, req)
}

// bareRecorder hides Load, so it is not a dashi.Loader.
type bareRecorder struct {
	dashi.Recorder
}

// countingRecorder counts the child turns it starts.
type countingRecorder struct {
	dashi.Recorder
	starts atomic.Int32
}

func (c *countingRecorder) Load(
	ctx context.Context,
	id string,
) (dashi.Turn, error) {
	return c.Recorder.(dashi.Loader).Load(ctx, id)
}

func (c *countingRecorder) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	if req.Step.Kind == dashi.StepStarted &&
		strings.HasPrefix(req.Step.TurnID, "child-") {
		c.starts.Add(1)
	}
	return c.Recorder.Commit(ctx, req)
}

type harness struct {
	client     *fakeClient
	rec        *dashi.MemoryRecorder
	base       tool.Toolset
	cfg        delegate.Config
	tasks      *delegate.Tasks
	runner     dashi.Runner
	inbox      *dashi.Inbox
	events     chan delegate.ChildEvent
	taskEvents chan delegate.TaskEvent
	observed   atomic.Int32
	notify     atomic.Bool
}

// checkLeaks fails the test if a task goroutine or a gated model call is
// still alive after the test's cleanups ran. Register it first: cleanups
// run in reverse order.
func checkLeaks(t *testing.T) {
	t.Cleanup(func() {
		var dump string
		for range 100 {
			buf := make([]byte, 1<<20)
			dump = string(buf[:runtime.Stack(buf, true)])
			leaked := ""
			for _, g := range strings.Split(dump, "\n\n") {
				if strings.Contains(g, "delegate.(*Tasks).run") ||
					strings.Contains(g, "delegate_test.(*gate).wait") {
					leaked = g
				}
			}
			if leaked == "" {
				return
			}
			dump = leaked
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("goroutine leaked:\n%s", dump)
	})
}

// newHarness wires a parent runner whose toolset holds the task tools. store
// replaces the recorder when a test wraps it.
func newHarness(
	t *testing.T,
	handle modelFunc,
	cfg delegate.Config,
	store func(*dashi.MemoryRecorder) dashi.Recorder,
) *harness {
	t.Helper()
	checkLeaks(t)
	h := &harness{
		client:     &fakeClient{handle: handle},
		rec:        dashi.NewMemoryRecorder(),
		inbox:      dashi.NewInbox(),
		events:     make(chan delegate.ChildEvent, 1024),
		taskEvents: make(chan delegate.TaskEvent, 1024),
	}
	var recorder dashi.Recorder = h.rec
	if store != nil {
		recorder = store(h.rec)
	}
	var ran atomic.Int32
	h.base = tool.List{
		probe("read", yes(), &ran),
		probe("write", nil, &ran),
	}
	cfg.Subagents = cmpSubagents(cfg.Subagents)
	cfg.Runner.Observer = func(dashi.Event) {
		h.observed.Add(1)
	}
	cfg.OnChild = func(e delegate.ChildEvent) {
		h.events <- e
	}
	cfg.OnTask = func(e delegate.TaskEvent) {
		if e.Kind == delegate.TaskEnded && h.notify.Load() {
			_, _ = h.inbox.Send(e.Task.Notice().Steer())
		}
		h.taskEvents <- e
	}
	h.cfg = cfg
	h.runner = dashi.Runner{
		Agent:    agent("parent"),
		Client:   h.client,
		Recorder: recorder,
	}
	h.newTasks(t)
	return h
}

// newTasks gives the harness a fresh Tasks value, as a new process would
// have. It closes the old one first.
func (h *harness) newTasks(t *testing.T) {
	t.Helper()
	if h.tasks != nil {
		h.close(t)
	}
	h.tasks = delegate.NewTasks(h.cfg)
	h.runner.Tools = tool.Join(h.base, h.tasks.Tools())
	tasks := h.tasks
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tasks.Close(ctx); err != nil {
			t.Errorf("close tasks: %v", err)
		}
	})
}

func (h *harness) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.tasks.Close(ctx); err != nil {
		t.Fatalf("close tasks: %v", err)
	}
}

func cmpSubagents(in []delegate.Subagent) []delegate.Subagent {
	if len(in) > 0 {
		return in
	}
	return []delegate.Subagent{scout()}
}

func (h *harness) start(ctx context.Context) (dashi.Outcome, error) {
	return h.runner.Run(
		ctx,
		dashi.Turn{ID: parentTurn},
		dashi.Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		dashi.WithSteering(h.inbox),
	)
}

func (h *harness) resume(ctx context.Context) (dashi.Outcome, error) {
	turn, err := h.rec.Load(ctx, parentTurn)
	if err != nil {
		return dashi.Outcome{}, err
	}
	return h.runner.Run(ctx, turn, dashi.Continue{}, dashi.WithSteering(h.inbox))
}

// recover runs Tasks.Recover over the parent turn.
func (h *harness) recoverTasks(t *testing.T) error {
	t.Helper()
	turn, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	return h.tasks.Recover(t.Context(), h.runner, []dashi.Turn{turn})
}

func (h *harness) view(t *testing.T, id string) dashi.TurnView {
	t.Helper()
	turn, err := h.rec.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// waitStop waits for a turn's record to show a stop of kind.
func (h *harness) waitStop(t *testing.T, id string, kind dashi.StopKind) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if turn, err := h.rec.Load(t.Context(), id); err == nil {
			view, err := dashi.Replay(turn)
			if err == nil && view.Stop.Kind == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn %s never showed stop %s", id, kind)
}

// next returns the next task event of kind, skipping others.
func (h *harness) next(t *testing.T, kind delegate.TaskEventKind) delegate.TaskEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-h.taskEvents:
			if e.Kind == kind {
				return e
			}
		case <-timeout:
			t.Fatalf("no %s task event", kind)
		}
	}
}

// none fails if a task event of kind arrives within a short time.
func (h *harness) none(t *testing.T, kind delegate.TaskEventKind) {
	t.Helper()
	timeout := time.After(150 * time.Millisecond)
	for {
		select {
		case e := <-h.taskEvents:
			if e.Kind == kind {
				t.Fatalf("unexpected %s event for %+v", kind, e.Task)
			}
		case <-timeout:
			return
		}
	}
}

// call runs one task tool directly, outside a runner.
func (h *harness) call(t *testing.T, name, args string) (string, error) {
	t.Helper()
	return h.callCtx(t.Context(), name, args)
}

func (h *harness) callCtx(ctx context.Context, name, args string) (string, error) {
	list, err := h.tasks.Tools().Tools(ctx)
	if err != nil {
		return "", err
	}
	for _, tl := range list {
		if tl.Definition.Name != name {
			continue
		}
		out, err := tl.Executor.Execute(ctx, tool.Call{
			ID: "direct", Name: name, Args: json.RawMessage(args),
		})
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, b := range out.Output.Content {
			if b.Text != nil {
				sb.WriteString(b.Text.Text)
			}
		}
		return sb.String(), nil
	}
	return "", errors.New("no tool " + name)
}

func childID() string {
	return delegate.ChildID(parentTurn, callID)
}

func taskID() string {
	return delegate.TaskID(childID())
}

func toolResult(t *testing.T, v dashi.TurnView) string {
	t.Helper()
	view, ok := v.Tools[callID]
	if !ok || view.Status != dashi.ToolSettled {
		t.Fatalf("task call not settled: %+v", view.Status)
	}
	var sb strings.Builder
	for _, b := range view.Result().Output.Content {
		if b.Text != nil {
			sb.WriteString(b.Text.Text)
		}
	}
	return sb.String()
}

// countItems counts the context items with id that a turn records.
func countItems(turn dashi.Turn, id string) int {
	n := 0
	for _, s := range turn.Steps {
		for _, item := range s.Items() {
			if item.ID == id {
				n++
			}
		}
	}
	return n
}

// twoTasks makes the parent start two tasks in one message. Each child
// waits at its own gate, keyed by prompt.
func twoTasks(first, second *gate) modelFunc {
	return func(ctx context.Context, in llm.Input) (llm.Result, error) {
		switch {
		case role(in) != "parent":
			g := first
			if in.Messages[0].Text() == "second" {
				g = second
			}
			return gated(g, "answer to "+in.Messages[0].Text())(ctx, in)
		case len(in.Messages) == 1 && !strings.Contains(inputText(in), "[task t_"):
			r := taskCall("first")
			b := toolCall(
				"call-2",
				delegate.ToolName,
				`{"subagent":"scout","prompt":"second","description":"b"}`,
			)
			r.Message.Content = append(r.Message.Content, b.Message.Content...)
			return costed(r, "1"), nil
		}
		return parentAfterTask(ctx, in)
	}
}

func TestTaskReturnsBeforeChildEnds(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "found")), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The parent finished while the child still waits at the gate.
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	got := toolResult(t, out.TurnView)
	if !strings.Contains(got, "Started task "+taskID()+" (scout)") ||
		!strings.Contains(got, "Do not poll") {
		t.Fatalf("task result=%q", got)
	}
	list := h.tasks.List()
	if len(list) != 1 || !list[0].Running() || list[0].ID != taskID() {
		t.Fatalf("list=%+v", list)
	}
	g.waitStarted(t)
	g.open()
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "completed" || ended.Task.Answer != "found" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	child := h.view(t, childID())
	want := dashi.CallRef{TurnID: parentTurn, CallID: callID}
	if child.Parent == nil || *child.Parent != want {
		t.Fatalf("child parent=%+v, want %+v", child.Parent, want)
	}
	if child.Agent.ID != "child" || child.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("child=%+v %+v", child.Agent.ID, child.Stop)
	}
}

func TestNoticeReachesRunningTurnOnce(t *testing.T) {
	g := newGate()
	parentHeld := newGate()
	handle := scripted(gated(g, "it is in config/load.go"))
	h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		// The second parent call waits, so the turn is still running when
		// the child ends.
		if role(in) == "parent" && len(in.Messages) == 3 {
			if err := parentHeld.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return handle(ctx, in)
	}, delegate.Config{}, nil)
	h.notify.Store(true)

	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(t.Context())
		done <- out
	}()
	parentHeld.waitStarted(t)
	g.open()
	ended := h.next(t, delegate.TaskEnded)
	parentHeld.open()
	out := <-done

	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	if out.Answer != "parent read the notice" {
		t.Fatalf("answer=%q: the turn did not read the notice", out.Answer)
	}
	last := h.client.last("parent")
	if !strings.Contains(inputText(last), "[task "+taskID()+" completed]") ||
		!strings.Contains(inputText(last), "it is in config/load.go") {
		t.Fatalf("last parent input=%q", inputText(last))
	}
	if n := countItems(out.Turn, delegate.NoticeID(childID())); n != 1 {
		t.Fatalf("notice recorded %d times, want 1", n)
	}
	item := recordedNotice(t, out.Turn)
	if item.Origin != dashi.ContextTool {
		t.Fatalf("notice origin=%q, want %q", item.Origin, dashi.ContextTool)
	}
	// Recorded means delivered: a reopened host sends nothing.
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
	if ended.Task.ID != taskID() {
		t.Fatalf("ended task=%+v", ended.Task)
	}
}

func recordedNotice(t *testing.T, turn dashi.Turn) dashi.ContextItem {
	t.Helper()
	for _, s := range turn.Steps {
		for _, item := range s.Items() {
			if item.ID == delegate.NoticeID(childID()) {
				return item
			}
		}
	}
	t.Fatal("no recorded notice")
	return dashi.ContextItem{}
}

func TestNoticesWithNoRunningTurnMakeOneFollowUp(t *testing.T) {
	g1, g2 := newGate(), newGate()
	h := newHarness(t, twoTasks(g1, g2), delegate.Config{}, nil)
	h.notify.Store(true)
	h.runner.Agent.Limits.ToolConcurrency = 2
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	g1.open()
	g2.open()
	for range 2 {
		h.next(t, delegate.TaskEnded)
	}
	// The user also typed a line while no turn ran.
	if _, err := h.inbox.SendText("also fix the docs"); err != nil {
		t.Fatal(err)
	}
	left, err := h.inbox.Leftovers(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	var user, notices []dashi.Steer
	for _, s := range left {
		if delegate.IsNotice(s) {
			notices = append(notices, s)
		} else {
			user = append(user, s)
		}
	}
	if len(left) != 3 || len(user) != 1 ||
		user[0].Item.Origin != dashi.ContextUser {
		t.Fatalf("leftovers=%d user=%d", len(left), len(user))
	}

	// Either order of the notices names the same follow-up turn.
	id := delegate.FollowUpTurnID(notices)
	if id != delegate.FollowUpTurnID([]dashi.Steer{notices[1], notices[0]}) {
		t.Fatal("follow-up turn ID depends on notice order")
	}
	start := delegate.FollowUp(notices)
	if start.Origin != dashi.StartScheduled {
		t.Fatalf("origin=%q, want scheduled", start.Origin)
	}
	followUp, err := h.runner.Run(t.Context(), dashi.Turn{ID: id}, start)
	if err != nil {
		t.Fatal(err)
	}
	if followUp.Stop.Kind != dashi.StopCompleted ||
		followUp.Answer != "parent read the notice" {
		t.Fatalf("follow-up stop=%+v answer=%q", followUp.Stop, followUp.Answer)
	}
	in := inputText(h.client.last("parent"))
	for _, want := range []string{"answer to first", "answer to second"} {
		if !strings.Contains(in, want) {
			t.Fatalf("follow-up input lacks %q: %q", want, in)
		}
	}
	// The user's line was never turned into a notice, and the notices were
	// never turned into user text.
	if strings.Contains(in, "also fix the docs") {
		t.Fatalf("user line leaked into the follow-up: %q", in)
	}
	// The follow-up records the delivery, so a reopened host sends nothing.
	parent, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent, followUp.Turn}); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
	// Without the follow-up turn, both notices are still owed.
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent}); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	h.next(t, delegate.TaskEnded)
}

func TestResultAfterKillReachesTheAgentOnce(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "answer")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.open()
	h.next(t, delegate.TaskEnded) // the process dies before anyone reads it

	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.ID != taskID() || ended.Task.Answer != "answer" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	// Recover again in the same process: the host was already told.
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)

	// The host delivers the notice in a follow-up turn.
	notices := []dashi.Steer{ended.Task.Notice().Steer()}
	followUp, err := h.runner.Run(t.Context(),
		dashi.Turn{ID: delegate.FollowUpTurnID(notices)},
		delegate.FollowUp(notices))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent, followUp.Turn}); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
}

func TestStatusReadDoesNotSuppressTheNotice(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "the answer")), delegate.Config{}, nil)
	h.notify.Store(true)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.open()
	h.next(t, delegate.TaskEnded)
	got, err := h.call(t, delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || !strings.Contains(got, "the answer") {
		t.Fatalf("status=%q err=%v", got, err)
	}
	// The read is not delivery: the steer still waits, and a reopened host
	// is told again because the record holds no notice.
	parent, _ := h.rec.Load(t.Context(), parentTurn)
	left, err := h.inbox.Leftovers(parent)
	if err != nil || len(left) != 1 || !delegate.IsNotice(left[0]) {
		t.Fatalf("leftovers=%+v err=%v", left, err)
	}
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
}

func TestCloseInterruptsAndRecoverResumesTheSameChild(t *testing.T) {
	g := newGate()
	first := &countingRecorder{}
	h := newHarness(t, scripted(gated(g, "finished after resume")),
		delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			first.Recorder = m
			return first
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	before := taskID()
	h.close(t)
	h.waitStop(t, childID(), dashi.StopInterrupted)
	// Close is not an end: nobody is told to read a result.
	h.none(t, delegate.TaskEnded)
	if got := h.tasks.List()[0].State; got != "interrupted" {
		t.Fatalf("state after Close=%q", got)
	}
	if _, err := h.call(t, delegate.ToolName,
		`{"subagent":"scout","prompt":"x","description":"x"}`); err == nil {
		t.Fatal("a closed Tasks started a task")
	}

	// Reopen: a new recorder object over the same store, a new Tasks.
	second := &countingRecorder{Recorder: h.rec}
	h.runner.Recorder = second
	g2 := newGate()
	g2.open()
	h.client.handle = scripted(gated(g2, "finished after resume"))
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	started := h.next(t, delegate.TaskStarted)
	if started.Task.ID != before {
		t.Fatalf("task ID after reopen=%q, want %q", started.Task.ID, before)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.Answer != "finished after resume" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	if n := second.starts.Load(); n != 0 {
		t.Fatalf("reopen started %d new children, want 0", n)
	}
	child := h.view(t, childID())
	if child.ModelAttempts != 1 || child.Interruptions != 1 {
		t.Fatalf("child attempts=%d interruptions=%d",
			child.ModelAttempts, child.Interruptions)
	}
}

func TestKilledInsideTaskCallHoldsOneChild(t *testing.T) {
	isChild := func(r dashi.CommitRequest) bool {
		return strings.HasPrefix(r.Step.TurnID, "child-")
	}
	for _, tc := range []struct {
		name    string
		trigger func(dashi.CommitRequest) bool
	}{
		{"before the child is saved", func(r dashi.CommitRequest) bool {
			return isChild(r) && r.Step.Kind == dashi.StepStarted
		}},
		{"after the child started", func(r dashi.CommitRequest) bool {
			return isChild(r) && r.Step.Kind == dashi.StepModelIntent
		}},
		{"before the call settled", func(r dashi.CommitRequest) bool {
			return !isChild(r) && r.Step.Kind == dashi.StepToolSettled
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var crash *crashRecorder
			h := newHarness(t, script("answer"), delegate.Config{},
				func(m *dashi.MemoryRecorder) dashi.Recorder {
					crash = &crashRecorder{Recorder: m, trigger: tc.trigger}
					return crash
				})
			if _, err := h.start(t.Context()); err == nil {
				t.Fatal("the crash did not stop the run")
			}
			crash.trigger = nil
			crash.dead.Store(false)
			h.newTasks(t)
			if err := h.recoverTasks(t); err != nil {
				t.Fatal(err)
			}
			out, err := h.resume(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("parent stop=%+v", out.Stop)
			}
			if !strings.Contains(toolResult(t, out.TurnView), taskID()) {
				t.Fatalf("task result=%q", toolResult(t, out.TurnView))
			}
			if n := len(h.tasks.List()); n != 1 {
				t.Fatalf("tasks=%d, want 1", n)
			}
			h.next(t, delegate.TaskEnded)
			// The rerun call added no second child.
			if got := h.client.sends("child"); got != 1 {
				t.Fatalf("child model calls=%d, want 1", got)
			}
			if crash.starts.Load() > 1 {
				t.Fatalf("child turns started=%d", crash.starts.Load())
			}
		})
	}
}

func TestStopCancelsTheChildForGood(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "never")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	got, err := h.call(t, delegate.StopToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || got != "Stopped task "+taskID()+"." {
		t.Fatalf("stop=%q err=%v", got, err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "canceled" {
		t.Fatalf("state=%q", ended.Task.State)
	}
	if h.view(t, childID()).Stop.Kind != dashi.StopCanceled {
		t.Fatal("the record does not show canceled")
	}
	got, err = h.call(t, delegate.StopToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || got != "Task "+taskID()+" already canceled." {
		t.Fatalf("second stop=%q err=%v", got, err)
	}
	if err := h.tasks.Stop(t.Context(), "t_nope"); !errors.Is(err, delegate.ErrUnknownTask) {
		t.Fatalf("Stop unknown=%v", err)
	}
	if err := h.tasks.Stop(t.Context(), taskID()); !errors.Is(err, delegate.ErrTaskEnded) {
		t.Fatalf("Stop ended=%v", err)
	}
	// A reopened host does not resume a canceled child.
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	if h.next(t, delegate.TaskEnded).Task.State != "canceled" {
		t.Fatal("the reopened task is not canceled")
	}
	if h.client.sends("child") != sends {
		t.Fatal("Recover ran a canceled child")
	}
}

func TestStopWithHostAPI(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "never")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
		t.Fatal(err)
	}
	if got := h.tasks.List()[0].State; got != "canceled" {
		t.Fatalf("state=%q", got)
	}
	h.next(t, delegate.TaskEnded)
	h.none(t, delegate.TaskEnded)
}

func TestCtrlCOnTheParentLeavesTasksRunning(t *testing.T) {
	g := newGate()
	parentHeld := newGate()
	handle := scripted(gated(g, "still done"))
	h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "parent" && len(in.Messages) == 3 {
			if err := parentHeld.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return handle(ctx, in)
	}, delegate.Config{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
	go func() {
		out, _ := h.start(ctx)
		done <- out
	}()
	parentHeld.waitStarted(t)
	cancel() // Ctrl-C
	if out := <-done; out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	if !h.tasks.List()[0].Running() {
		t.Fatal("Ctrl-C stopped the task")
	}
	g.open()
	if h.next(t, delegate.TaskEnded).Task.Answer != "still done" {
		t.Fatal("the task did not finish after Ctrl-C")
	}
}

func TestStatusShowsProgressAndWaits(t *testing.T) {
	g := newGate()
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "child" {
			return scripted(nil)(ctx, in)
		}
		if len(in.Messages) == 1 {
			return costed(toolCall("c1", "read", `{}`), "0.25"), nil
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return costed(text("done"), "0.25"), nil
	}
	h := newHarness(t, handle, delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	var got string
	for range 200 {
		var err error
		got, err = h.call(t, delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "1 tool calls") &&
			strings.Contains(got, "0.25 USD") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(got, "running") || !strings.Contains(got, "1 tool calls") ||
		!strings.Contains(got, "0.25 USD") {
		t.Fatalf("running status=%q", got)
	}

	// A short wait returns while the task still runs.
	start := time.Now()
	got, _ = h.call(t, delegate.StatusToolName,
		`{"task_id":"`+taskID()+`","wait_seconds":1}`)
	if time.Since(start) < 900*time.Millisecond || !strings.Contains(got, "running") {
		t.Fatalf("wait returned early or wrong: %v %q", time.Since(start), got)
	}
	// The context of the call ends a wait early.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := h.callCtx(ctx, delegate.StatusToolName,
		`{"task_id":"`+taskID()+`","wait_seconds":60}`); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a canceled wait took %v", time.Since(start))
	}
	// A wait without an ID returns when the next task ends.
	go func() {
		time.Sleep(50 * time.Millisecond)
		g.open()
	}()
	got, _ = h.call(t, delegate.StatusToolName, `{"wait_seconds":30}`)
	if !strings.Contains(got, taskID()+" scout") || !strings.Contains(got, "completed") {
		t.Fatalf("list after wait=%q", got)
	}
	got, _ = h.call(t, delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if !strings.Contains(got, "completed") || !strings.HasSuffix(got, "done") {
		t.Fatalf("ended status=%q", got)
	}
}

func TestStatusErrors(t *testing.T) {
	h := newHarness(t, script("x"), delegate.Config{}, nil)
	if got, _ := h.call(t, delegate.StatusToolName, `{}`); got != "No tasks." {
		t.Fatalf("empty list=%q", got)
	}
	if _, err := h.call(t, delegate.StatusToolName,
		`{"task_id":"t_nope"}`); err == nil ||
		!strings.Contains(err.Error(), "no tasks") {
		t.Fatalf("unknown ID error=%v", err)
	}
	if _, err := h.call(t, delegate.StatusToolName,
		`{"wait_seconds":61}`); err == nil {
		t.Fatal("wait_seconds above the cap was accepted")
	}
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if _, err := h.call(t, delegate.StatusToolName,
		`{"task_id":"t_nope"}`); err == nil ||
		!strings.Contains(err.Error(), taskID()) {
		t.Fatalf("error does not list known IDs: %v", err)
	}
	if _, err := h.call(t, delegate.StopToolName,
		`{"task_id":"t_nope"}`); err == nil {
		t.Fatal("stopping an unknown task succeeded")
	}
}

func TestSpendCountsOnceAcrossReopen(t *testing.T) {
	h := newHarness(t, script("done"), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	usd := money.MustParseAmount
	if got := out.Spend.Cost["USD"]; got != usd("2") {
		t.Fatalf("parent view spend=%v, want its own 2 only", got)
	}
	check := func(recorder dashi.Loader) {
		t.Helper()
		report, err := delegate.Report(t.Context(), recorder, out.TurnView)
		if err != nil {
			t.Fatal(err)
		}
		if report.Parent.Cost["USD"] != usd("2") ||
			report.Children.Cost["USD"] != usd("0.25") ||
			report.Inclusive.Cost["USD"] != usd("2.25") {
			t.Fatalf("report=%+v", report)
		}
		if len(report.Inclusive.ToolCost) != 0 {
			t.Fatalf("child model cost leaked into tool cost: %+v", report)
		}
	}
	check(h.rec)
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	check(h.rec)
	if got := h.tasks.List()[0].Spend.Cost["USD"]; got != usd("0.25") {
		t.Fatalf("task spend=%v", got)
	}
}

func TestReportKeepsUnknownPriceFlags(t *testing.T) {
	child := func(context.Context, llm.Input) (llm.Result, error) {
		r := text("done")
		r.Usage = &llm.Usage{InputTokens: 1, OutputTokens: 1}
		return r, nil // no cost: the price is unknown
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	report, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil {
		t.Fatal(err)
	}
	if report.Children.CostUnknown != 1 || report.Inclusive.CostUnknown != 1 ||
		report.Parent.CostUnknown != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestChildEventsReachOnChildAndTemplateObserver(t *testing.T) {
	h := newHarness(t, script("done"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	deadline := time.Now().Add(5 * time.Second)
	for h.observed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.observed.Load() == 0 {
		t.Fatal("template observer saw no child event")
	}
	sawStep := false
	for len(h.events) > 0 {
		e := <-h.events
		want := dashi.CallRef{TurnID: parentTurn, CallID: callID}
		if e.Parent != want || e.Subagent != "scout" ||
			e.Event.TurnID != childID() {
			t.Fatalf("event=%+v", e)
		}
		sawStep = sawStep || e.Event.Step != nil
	}
	if !sawStep {
		t.Fatal("OnChild saw no committed child step")
	}
}

func TestTaskWithoutLoaderWarnsAndIsNotIdempotent(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// An override Recorder that is not a Loader is known at NewTasks.
	tasks := delegate.NewTasks(delegate.Config{
		Runner: dashi.Runner{Recorder: bareRecorder{dashi.NewMemoryRecorder()}},
	})
	list, err := tasks.Tools().Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list {
		if tl.Definition.Name == delegate.ToolName &&
			tl.Definition.Annotations.Idempotent != nil {
			t.Fatal("task is marked idempotent without a Loader")
		}
	}
	// Recover names the missing Loader.
	err = tasks.Recover(t.Context(), dashi.Runner{}, nil)
	if err == nil || !strings.Contains(err.Error(), "dashi.Loader") {
		t.Fatalf("Recover error=%v", err)
	}
	// An inherited Recorder that is not a Loader is known at the first call.
	h := newHarness(t, script("x"), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			return bareRecorder{m}
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if n := strings.Count(logs.String(), "without a host recorder"); n != 2 {
		t.Fatalf("warnings=%d, want 2 (one from NewTasks, one from the call): %q",
			n, logs.String())
	}
}

func TestTaskIsIdempotentWithLoader(t *testing.T) {
	h := newHarness(t, script("x"), delegate.Config{}, nil)
	tools, err := h.runner.Tools.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]tool.Definition{}
	for _, tl := range tools {
		found[tl.Definition.Name] = tl.Definition
	}
	if i := found[delegate.ToolName].Annotations.Idempotent; i == nil || !*i {
		t.Fatal("task is not marked idempotent")
	}
	if r := found[delegate.StatusToolName].Annotations.ReadOnly; r == nil || !*r {
		t.Fatal("task_status is not marked read-only")
	}
	if _, ok := found[delegate.StopToolName]; !ok {
		t.Fatal("no task_stop tool")
	}
}

// A nil Recorder is a supported mode. The child runs, and the task ends. A
// resumed parent reports the task call as uncertain.
func TestTaskWithoutHostRecorder(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "found it")), delegate.Config{}, nil)
	h.runner.Recorder = nil
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	g.open()
	if h.next(t, delegate.TaskEnded).Task.Answer != "found it" {
		t.Fatal("the task lost its answer")
	}
	if err := h.tasks.Recover(t.Context(), h.runner, nil); err == nil {
		t.Fatal("Recover accepted a runner with no Loader")
	}
}

// childCallsProbe has the child call the write probe once and then answer.
func childCallsProbe(_ context.Context, in llm.Input) (llm.Result, error) {
	if len(in.Messages) == 1 {
		return toolCall("c1", "write", `{}`), nil
	}
	return text("child finished"), nil
}

// The host hooks cover a child tool call while no turn runs.
func TestChildToolCallsPassHostHooksAfterTheTurnEnded(t *testing.T) {
	var mu sync.Mutex
	var seen []dashi.ToolPlan
	g := newGate()
	hook := dashi.ToolHook{
		Name: "host-approval",
		Func: func(
			_ context.Context,
			p dashi.ToolPlan,
		) (dashi.ToolDecision, error) {
			mu.Lock()
			seen = append(seen, p)
			mu.Unlock()
			if p.Call.Name == "write" {
				return dashi.Deny("host said no"), nil
			}
			return dashi.ToolDecision{}, nil
		},
	}
	h := newHarness(t, scripted(func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return childCallsProbe(ctx, in)
	}), delegate.Config{}, nil)
	h.runner.Hooks = hook.Hooks()
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	g.open() // the child's tool call happens after the parent turn ended
	h.next(t, delegate.TaskEnded)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range seen {
		if p.Call.Name != "write" {
			continue
		}
		if p.Turn.Parent == nil || p.Turn.Agent.ID != "child" {
			t.Fatalf("hook saw turn parent=%+v agent=%q",
				p.Turn.Parent, p.Turn.Agent.ID)
		}
		child := h.view(t, childID())
		if v := child.Tools["c1"]; v.Status != dashi.ToolSettled ||
			!strings.Contains(v.Verdict.Reason, "host said no") {
			t.Fatalf("child write call=%+v", v.Status)
		}
		return
	}
	t.Fatal("the host hook never saw the child's write call")
}

func TestDeferredApprovalInChildEndsTheTaskWaiting(t *testing.T) {
	hook := dashi.ToolHook{
		Name: "defer-writes",
		Func: func(
			_ context.Context,
			p dashi.ToolPlan,
		) (dashi.ToolDecision, error) {
			if p.Call.Name == "write" {
				return dashi.Defer("needs a person"), nil
			}
			return dashi.ToolDecision{}, nil
		},
	}
	h := newHarness(t, scripted(childCallsProbe), delegate.Config{}, nil)
	h.runner.Hooks = hook.Hooks()
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "waiting" {
		t.Fatalf("state=%q, want waiting", ended.Task.State)
	}
	if h.view(t, childID()).Stop.Kind != dashi.StopWaiting {
		t.Fatal("the child is not resumable by its ID")
	}
}

func toolNames(in llm.Input) []string {
	var names []string
	for _, d := range in.Tools {
		names = append(names, d.Name)
	}
	return names
}

func TestChildToolsAreFilteredAndNeverIncludeTaskTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  delegate.Subagent
		want []string
	}{
		{
			name: "all tools except the task tools",
			sub:  scout(),
			want: []string{"read", "write"},
		},
		{
			name: "read-only",
			sub: func() delegate.Subagent {
				s := scout()
				s.ReadOnly = true
				return s
			}(),
			want: []string{"read"},
		},
		{
			name: "named tools",
			sub: func() delegate.Subagent {
				s := scout()
				s.Tools = []string{"write", "task", "task_status", "task_stop"}
				return s
			}(),
			want: []string{"write"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := delegate.Config{Subagents: []delegate.Subagent{tc.sub}}
			h := newHarness(t, script("x"), cfg, nil)
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.next(t, delegate.TaskEnded)
			got := toolNames(h.client.last("child"))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("child tools=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestChildSeesOnlyItsPrompt(t *testing.T) {
	h := newHarness(t, script("x"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	in := h.client.last("child")
	if len(in.Messages) != 1 || in.Messages[0].Text() != "find the parser" {
		t.Fatalf("child input=%+v", in.Messages)
	}
}

func TestNoticeCutsLongAnswer(t *testing.T) {
	long := strings.Repeat("é", 100)
	h := newHarness(t, script(long), delegate.Config{MaxAnswer: 51}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	got := ended.Task.Notice().Text()
	if !strings.Contains(got, "[answer cut at 50 bytes]") {
		t.Fatalf("notice=%q", got)
	}
	if !strings.Contains(got, strings.Repeat("é", 25)+"\n[answer") {
		t.Fatalf("cut split a character: %q", got)
	}
}

func TestFailedChildEndsTheTaskAndIsNotRerun(t *testing.T) {
	child := func(context.Context, llm.Input) (llm.Result, error) {
		return text("  "), nil // an empty answer fails the child
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "failed" || ended.Task.Reason == "" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	if text := ended.Task.Notice().Text(); !strings.Contains(text, "failed") ||
		!strings.Contains(text, "Reason:") {
		t.Fatalf("notice=%q", text)
	}
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	if h.next(t, delegate.TaskEnded).Task.State != "failed" {
		t.Fatal("reopened task is not failed")
	}
	if h.client.sends("child") != sends {
		t.Fatal("Recover ran the failed child again")
	}
}

func TestTaskRejectsUnknownSubagent(t *testing.T) {
	handle := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "parent" && len(in.Messages) == 1 {
			return toolCall(
				callID,
				delegate.ToolName,
				`{"subagent":"nope","prompt":"x","description":"x"}`,
			), nil
		}
		return script("")(ctx, in)
	}
	h := newHarness(t, handle, delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The enum makes the runner refuse the arguments before the tool runs.
	view := out.Tools[callID]
	if view.Status != dashi.ToolSettled || h.client.sends("child") != 0 ||
		len(h.tasks.List()) != 0 {
		t.Fatalf("status=%v child sends=%d", view.Status,
			h.client.sends("child"))
	}
}

func TestParallelTaskCallsGetOwnChildren(t *testing.T) {
	g1, g2 := newGate(), newGate()
	h := newHarness(t, twoTasks(g1, g2), delegate.Config{}, nil)
	h.runner.Agent.Limits.ToolConcurrency = 2
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stop=%+v", out.Stop)
	}
	g1.open()
	g2.open()
	h.next(t, delegate.TaskEnded)
	h.next(t, delegate.TaskEnded)
	for id, want := range map[string]string{
		"call-1": "answer to first",
		"call-2": "answer to second",
	} {
		v := h.view(t, delegate.ChildID(parentTurn, id))
		if v.Answer != want || v.Parent.CallID != id {
			t.Fatalf("child %s: answer=%q parent=%+v", id, v.Answer, v.Parent)
		}
	}
	if n := len(h.tasks.List()); n != 2 {
		t.Fatalf("tasks=%d, want 2", n)
	}
	report, err := delegate.Report(t.Context(), h.rec, out.TurnView)
	if err != nil || report.Children.Cost["USD"] != money.MustParseAmount("0.5") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestChildIDIsPureAndCollisionFree(t *testing.T) {
	if delegate.ChildID("a", "b") != delegate.ChildID("a", "b") {
		t.Fatal("ChildID is not deterministic")
	}
	pairs := [][2]string{
		{"a", "b:c"}, {"a:b", "c"}, {"a", "bc"}, {"ab", "c"},
		{"", "abc"}, {"abc", ""}, {"a", "b"},
	}
	seen := map[string][2]string{}
	for _, p := range pairs {
		id := delegate.ChildID(p[0], p[1])
		if prev, dup := seen[id]; dup {
			t.Fatalf("%v and %v share ID %q", prev, p, id)
		}
		seen[id] = p
	}
}

func TestTaskIDIsAFixedFunctionOfTheChild(t *testing.T) {
	id := delegate.TaskID("child-1:p:c")
	if id != delegate.TaskID("child-1:p:c") || id == delegate.TaskID("child-1:p:d") {
		t.Fatal("TaskID is not a fixed, distinguishing function")
	}
	if len(id) != 8 || !strings.HasPrefix(id, "t_") {
		t.Fatalf("task ID %q", id)
	}
}

func TestDefaultCatalogHasExploreAndGeneral(t *testing.T) {
	list, err := delegate.NewTasks(delegate.Config{}).Tools().Tools(t.Context())
	if err != nil || len(list) != 3 {
		t.Fatalf("tools=%d err=%v", len(list), err)
	}
	schema := string(list[0].Definition.Schema)
	if !strings.Contains(schema, `"explore","general"`) {
		t.Fatalf("schema=%s", schema)
	}
}

// A child uses the parent's current model unless its Subagent sets one.
func TestChildInheritsParentModel(t *testing.T) {
	sub := scout()
	sub.Agent.Provider, sub.Agent.API, sub.Agent.Model = "", "", ""
	h := newHarness(t, script("x"),
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	h.runner.Agent.Model = "chosen-by-user"
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if got := h.view(t, childID()).Agent.Model; got != "chosen-by-user" {
		t.Fatalf("recorded child model=%q", got)
	}
}

func TestSubagentModelOverridesParent(t *testing.T) {
	sub := scout()
	sub.Agent.Model = "cheap"
	h := newHarness(t, script("x"),
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	h.runner.Agent.Model = "chosen-by-user"
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if got := h.view(t, childID()).Agent.Model; got != "cheap" {
		t.Fatalf("child model=%q, want cheap", got)
	}
}

// A crash during a child tool call, after the child's first model call
// settled, must not repeat that model call after Recover.
func TestCrashDuringChildToolCallRepeatsNoModelCall(t *testing.T) {
	child := func(_ context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			return costed(toolCall("c1", "read", `{}`), "0.25"), nil
		}
		return costed(text("child finished"), "0.25"), nil
	}
	var crash *crashRecorder
	h := newHarness(t, scripted(child), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			crash = &crashRecorder{Recorder: m, trigger: func(
				r dashi.CommitRequest,
			) bool {
				return strings.HasPrefix(r.Step.TurnID, "child-") &&
					r.Step.Kind == dashi.StepToolSettled
			}}
			return crash
		})
	_, _ = h.start(t.Context()) // the crash may stop the parent too
	if h.next(t, delegate.TaskEnded).Task.State != "failed" {
		t.Fatal("the crashed child did not fail its task")
	}
	crash.trigger = nil
	crash.dead.Store(false)
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskStarted)
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.Answer != "child finished" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	// One call asked for the tool and one answered. Neither ran twice.
	if got := h.client.sends("child"); got != 2 {
		t.Fatalf("child model calls=%d, want 2", got)
	}
	if got := crash.starts.Load(); got != 1 {
		t.Fatalf("child turns started=%d, want 1", got)
	}
}

// A child with an output schema returns its structured value.
func TestTaskReturnsStructuredChildOutput(t *testing.T) {
	sub := scout()
	sub.Agent.OutputSchema = json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"integer"}},` +
			`"required":["n"],"additionalProperties":false}`)
	child := func(context.Context, llm.Input) (llm.Result, error) {
		return costed(
			toolCall("s1", dashi.SubmitOutputTool, `{"n":7}`), "0.25"), nil
	}
	h := newHarness(t, scripted(child),
		delegate.Config{Subagents: []delegate.Subagent{sub}}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.next(t, delegate.TaskEnded).Task.Answer; got != `{"n":7}` {
		t.Fatalf("answer=%q", got)
	}
}

func TestRecoverSkipsAnUnknownSubagentAndReportsIt(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "x")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.open()
	h.next(t, delegate.TaskEnded)
	h.cfg.Subagents = []delegate.Subagent{{Name: "other", Agent: agent("child")}}
	h.newTasks(t)
	err := h.recoverTasks(t)
	if err == nil || !strings.Contains(err.Error(), `"scout"`) {
		t.Fatalf("Recover error=%v", err)
	}
	if len(h.tasks.List()) != 0 {
		t.Fatal("Recover adopted a child it could not run")
	}
}

// A notice steer that a run claimed but could not record stays owed: it comes
// back once from Leftovers, and once delivered it never comes back.
func TestUnrecordedNoticeComesBackOnceFromLeftovers(t *testing.T) {
	var crash *crashRecorder
	h := newHarness(t, func(_ context.Context, in llm.Input) (llm.Result, error) {
		return costed(text("plain answer"), "1"), nil
	}, delegate.Config{}, func(m *dashi.MemoryRecorder) dashi.Recorder {
		crash = &crashRecorder{Recorder: m, trigger: func(r dashi.CommitRequest) bool {
			return r.Step.Context != nil && r.Step.Context.Steering
		}}
		return crash
	})
	notice := delegate.TaskView{
		ID: "t_abcdef", ChildID: "child-x", Subagent: "scout",
		Description: "look", State: "completed", Answer: "found",
	}.Notice()
	if _, err := h.inbox.Send(notice.Steer()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.inbox.Send(notice.Steer()); err == nil {
		t.Fatal("the inbox accepted the same notice twice")
	}
	_, _ = h.start(t.Context()) // the steer write is rejected
	turn, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	left, err := h.inbox.Leftovers(turn)
	if err != nil || len(left) != 1 || !delegate.IsNotice(left[0]) {
		t.Fatalf("leftovers=%+v err=%v", left, err)
	}
	again, err := h.inbox.Leftovers(turn)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Leftovers=%+v err=%v", again, err)
	}
}

// A child that ended before the parent crashed inside the task call must
// reach OnTask whether the host resumes the parent or calls Recover first.
func TestEndedChildIsReportedInEitherReopenOrder(t *testing.T) {
	for _, recoverFirst := range []bool{false, true} {
		name := "resume then recover"
		if recoverFirst {
			name = "recover then resume"
		}
		t.Run(name, func(t *testing.T) {
			var h *harness
			var crash *crashRecorder
			h = newHarness(t, script("the answer"), delegate.Config{},
				func(m *dashi.MemoryRecorder) dashi.Recorder {
					crash = &crashRecorder{Recorder: m,
						trigger: func(r dashi.CommitRequest) bool {
							if r.Step.Kind != dashi.StepToolSettled ||
								strings.HasPrefix(r.Step.TurnID, "child-") {
								return false
							}
							// Crash only after the child has ended.
							h.waitStop(t, childID(), dashi.StopCompleted)
							return true
						}}
					return crash
				})
			if _, err := h.start(t.Context()); err == nil {
				t.Fatal("the crash did not stop the run")
			}
			h.next(t, delegate.TaskEnded) // the first process reports it
			crash.trigger = nil
			crash.dead.Store(false)
			h.newTasks(t)
			if recoverFirst {
				if err := h.recoverTasks(t); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.resume(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(toolResult(t, out.TurnView), "already ended") {
				t.Fatalf("task result=%q", toolResult(t, out.TurnView))
			}
			if !recoverFirst {
				if err := h.recoverTasks(t); err != nil {
					t.Fatal(err)
				}
			}
			ended := h.next(t, delegate.TaskEnded)
			if ended.Task.Answer != "the answer" {
				t.Fatalf("ended=%+v", ended.Task)
			}
			h.none(t, delegate.TaskEnded)
			if got := h.client.sends("child"); got != 1 {
				t.Fatalf("child model calls=%d, want 1", got)
			}
		})
	}
}

// Wait must not say "idle" between a task ending and its notice reaching the
// host, even when OnTask is slow.
func TestWaitCoversTheGapBeforeTheNotice(t *testing.T) {
	g := newGate()
	release := make(chan struct{})
	h := newHarness(t, scripted(gated(g, "late")), delegate.Config{}, nil)
	h.cfg.OnTask = func(e delegate.TaskEvent) {
		if e.Kind == delegate.TaskEnded {
			<-release
			_, _ = h.inbox.Send(e.Task.Notice().Steer())
		}
	}
	h.newTasks(t)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	g.open()
	h.waitStop(t, childID(), dashi.StopCompleted)
	// The child ended. OnTask has not returned. Nothing is idle yet.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if idle, err := h.tasks.Wait(ctx); err == nil {
		t.Fatalf("Wait returned idle=%v before the notice was sent", idle)
	}
	close(release)
	idle, err := h.tasks.Wait(t.Context())
	if err != nil || idle {
		t.Fatalf("Wait idle=%v err=%v, want a report", idle, err)
	}
	turn, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	left, err := h.inbox.Leftovers(turn)
	if err != nil || len(left) != 1 {
		t.Fatalf("leftovers=%d err=%v, want the notice", len(left), err)
	}
	if idle, err = h.tasks.Wait(t.Context()); err != nil || !idle {
		t.Fatalf("second Wait idle=%v err=%v, want idle", idle, err)
	}
}

func TestStopReturnsWhenContextEnds(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(func(ctx context.Context, in llm.Input) (llm.Result, error) {
		// A child that ignores cancellation until the test ends.
		g.started <- struct{}{}
		<-g.release
		return text("late"), nil
	}), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := h.tasks.Stop(ctx, taskID()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop=%v, want the context error", err)
	}
	g.open()
}

// Recover continues a child with the agent its record holds, even when the
// catalog changed since.
func TestRecoverResumesWithTheRecordedAgent(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "x")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	h.close(t)
	h.waitStop(t, childID(), dashi.StopInterrupted)

	changed := scout()
	changed.Agent = agent("changed")
	h.cfg.Subagents = []delegate.Subagent{changed}
	g2 := newGate()
	g2.open()
	h.client.handle = scripted(gated(g2, "done"))
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	if got := h.client.sends("changed"); got != 0 {
		t.Fatalf("resumed child used the changed agent %d times", got)
	}
	if got := h.client.sends("child"); got == 0 {
		t.Fatal("resumed child did not run with the recorded agent")
	}
}
