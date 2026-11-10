package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/wonton/termtest"
	"github.com/deepnoodle-ai/wonton/tui"
)

// threeTaskClient starts three tasks from the parent's first model call.
// Each child takes threeTaskSteps model calls that stream a step line and
// call list_directory, then a final call that streams its result. Every
// child model call waits for the test to release it, so the test decides
// how the three streams interleave.
type threeTaskClient struct {
	fakeClient
	mu      sync.Mutex
	parent  int
	calls   map[string]int
	release map[string]chan struct{}
	waiting map[string]chan struct{}
}

const threeTaskSteps = 3

var threeTaskNames = []string{"alpha", "beta", "gamma"}

func newThreeTaskClient() *threeTaskClient {
	c := &threeTaskClient{calls: map[string]int{}, release: map[string]chan struct{}{}, waiting: map[string]chan struct{}{}}
	for _, name := range threeTaskNames {
		c.release[name] = make(chan struct{}, threeTaskSteps+1)
		c.waiting[name] = make(chan struct{}, threeTaskSteps+1)
	}
	return c
}

func (c *threeTaskClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	in := call.Request.Snapshot().Input
	if !strings.Contains(in.System, "You are a subagent.") {
		c.mu.Lock()
		n := c.parent
		c.parent++
		c.mu.Unlock()
		if n > 0 {
			return taskModelResult(say("Parent is free.")(ctx, call))
		}
		var parts []llm.Content
		for _, name := range threeTaskNames {
			tc := tool.Call{ID: "task-" + name, Name: "task", Args: []byte(`{"subagent":"general","description":"Work on ` + name + `","prompt":"Inspect ` + name + `"}`)}
			parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &tc})
		}
		return taskModelResult(llm.Result{Message: llm.AssistantMessage(parts...), Stop: llm.Stop{Kind: llm.StopToolUse}, Usage: &llm.Usage{InputTokens: 50, OutputTokens: 10}}, nil)
	}
	name := ""
	for _, m := range in.Messages {
		for _, candidate := range threeTaskNames {
			if strings.Contains(m.Text(), "Inspect "+candidate) {
				name = candidate
			}
		}
	}
	if name == "" {
		return llm.Result{}, fmt.Errorf("unknown child")
	}
	c.mu.Lock()
	n := c.calls[name]
	c.calls[name]++
	c.mu.Unlock()
	c.waiting[name] <- struct{}{}
	select {
	case <-c.release[name]:
	case <-ctx.Done():
		return llm.Result{}, ctx.Err()
	}
	if n >= threeTaskSteps {
		return taskModelResult(say(name+" finished its work.")(ctx, call))
	}
	text := fmt.Sprintf("%s step %d of %d.", name, n+1, threeTaskSteps)
	if call.Sink != nil {
		call.Sink(llm.Delta{Kind: llm.DeltaText, Text: text[:len(text)/2]})
		call.Sink(llm.Delta{Kind: llm.DeltaText, Text: text[len(text)/2:]})
	}
	tc := tool.Call{ID: fmt.Sprintf("%s-list-%d", name, n), Name: "list_directory", Args: []byte(`{}`)}
	return taskModelResult(llm.Result{
		Message: llm.AssistantMessage(llm.Text(text), llm.Content{Kind: llm.ContentToolCall, ToolCall: &tc}),
		Stop:    llm.Stop{Kind: llm.StopToolUse},
		Usage:   &llm.Usage{InputTokens: 50, OutputTokens: 10},
	}, nil)
}

func taskModelResult(res llm.Result, err error) (llm.Result, error) {
	if res.Requested.Provider == "" {
		res.Requested = llm.ModelRef{Provider: "test", Model: "fixture"}
	}
	return res, err
}

// paneProbe runs fn on the Wonton event loop, where the screen's state is
// safe to read.
type paneProbe struct {
	fn   func(*screen)
	done chan struct{}
}

func (paneProbe) Timestamp() time.Time { return time.Now() }

type probedScreen struct {
	*screen
	rt chan *tui.Runtime
}

func (s *probedScreen) SetRuntime(rt *tui.Runtime) {
	s.screen.SetRuntime(rt)
	s.rt <- rt
}

func (s *probedScreen) HandleEvent(event tui.Event) []tui.Cmd {
	if p, ok := event.(paneProbe); ok {
		p.fn(s.screen)
		close(p.done)
		return nil
	}
	return s.screen.HandleEvent(event)
}

// Three tasks stream into three open panes while the developer types. The
// real Wonton runtime reads the keys and draws to a test terminal. Every
// step shows once in its pane, and each typed key reaches the prompt on
// screen within the plan's two-second bound while the tasks stream.
func TestThreeStreamingTaskPanesKeepTypingResponsive(t *testing.T) {
	const width, height = 160, 40
	client := newThreeTaskClient()
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ApproveAll = true
	scr := &probedScreen{screen: newScreen(cfg.Dir), rt: make(chan *tui.Runtime, 1)}
	scr.width, scr.height = width, height
	a, err := buildApp(t.Context(), cfg, scr)
	if err != nil {
		t.Fatal(err)
	}
	scr.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = false
	a.ctrl.mu.Unlock()
	a.ctrl.Open()

	display := termtest.NewScreen(width, height)
	keys := tui.NewTestInput()
	done := make(chan error, 1)
	go func() {
		done <- tui.Run(scr, tui.WithTerminal(tui.NewTestTerminal(width, height, display)), tui.WithInput(keys), tui.WithMouseDrag(true), tui.WithFPS(30))
	}()
	var rt *tui.Runtime
	runtimeDone := false
	t.Cleanup(func() {
		a.ctrl.CancelWork()
		a.eng.closeTasks()
		a.ctrl.Wait()
		keys.Close()
		if runtimeDone {
			a.eng.Close()
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runtime: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop")
		}
		a.eng.Close()
	})
	select {
	case rt = <-scr.rt:
	case err := <-done:
		runtimeDone = true
		t.Fatalf("runtime stopped before it started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not start")
	}

	probe := func(fn func(*screen)) {
		t.Helper()
		p := paneProbe{fn: fn, done: make(chan struct{})}
		rt.SendEvent(p)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the event loop did not answer a probe")
		}
	}
	// waitFor polls on the event loop until cond holds. The bound is a
	// failure deadline, not a timing assumption.
	waitFor := func(bound time.Duration, what string, cond func(*screen) bool) {
		t.Helper()
		deadline := time.Now().Add(bound)
		for {
			ok := false
			probe(func(s *screen) { ok = cond(s) })
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("not within %s: %s\nscreen:\n%s", bound, what, display.Text())
			}
			time.Sleep(time.Millisecond)
		}
	}
	// typeAndSee types text and waits for the prompt row on the terminal
	// to show everything typed so far.
	var typed string
	typeAndSee := func(text string) {
		t.Helper()
		keys.Type(text)
		typed += text
		deadline := time.Now().Add(2 * time.Second)
		for !strings.Contains(display.Text(), "❯ "+strings.TrimRight(typed, " ")) {
			if time.Now().After(deadline) {
				var in string
				probe(func(s *screen) { in = s.input })
				t.Fatalf("typed %q did not show within 2s (input %q):\n%s", typed, in, display.Text())
			}
			time.Sleep(time.Millisecond)
		}
	}
	submit := func(text string) {
		t.Helper()
		typeAndSee(text)
		keys.Send(tui.KeyEvent{Key: tui.KeyEnter})
		typed = ""
	}

	// Keys reach the prompt once the first frame has drawn its input.
	waitFor(5*time.Second, "the first frame", func(*screen) bool { return strings.Contains(display.Text(), "Ask Dashi to do anything") })
	submit("Start three tasks")
	for _, name := range threeTaskNames {
		select {
		case <-client.waiting[name]:
		case <-time.After(10 * time.Second):
			t.Fatalf("task %s did not start", name)
		}
	}
	ids := map[string]string{}
	for _, v := range a.eng.taskHost().tasks.List() {
		for _, name := range threeTaskNames {
			if v.Description == "Work on "+name {
				ids[name] = v.ID
			}
		}
	}
	if len(ids) != 3 {
		t.Fatalf("tasks=%+v", a.eng.taskHost().tasks.List())
	}
	for _, name := range threeTaskNames {
		submit("/task " + ids[name])
		pane := taskPaneID(ids[name])
		waitFor(5*time.Second, "pane "+name+" loaded", func(s *screen) bool {
			p := s.panes.get(pane)
			return p != nil && p.watch != nil && !p.watch.loading
		})
	}
	waitFor(5*time.Second, "four panes", func(s *screen) bool { return len(s.paneList()) == 4 })

	// Release one model call at a time, round robin, typing a word between
	// releases so keys arrive while the panes stream.
	for step := range threeTaskSteps + 1 {
		for _, name := range threeTaskNames {
			client.release[name] <- struct{}{}
			typeAndSee(fmt.Sprintf("%s%d ", name[:1], step))
			if step < threeTaskSteps {
				want := fmt.Sprintf("%s step %d of %d.", name, step+1, threeTaskSteps)
				waitFor(5*time.Second, want, func(s *screen) bool {
					return strings.Contains(paneText(s.panes.get(taskPaneID(ids[name])).watch), want)
				})
				select {
				case <-client.waiting[name]:
				case <-time.After(10 * time.Second):
					t.Fatalf("%s did not make its next model call", name)
				}
			}
		}
	}
	for _, name := range threeTaskNames {
		pane := taskPaneID(ids[name])
		waitFor(10*time.Second, name+" ended", func(s *screen) bool {
			p := s.panes.get(pane)
			return p.ended && strings.Contains(paneText(p.watch), name+" finished its work.")
		})
		var text string
		probe(func(s *screen) { text = paneText(s.panes.get(pane).watch) })
		for i := 1; i <= threeTaskSteps; i++ {
			if n := strings.Count(text, fmt.Sprintf("%s step %d of %d.", name, i, threeTaskSteps)); n != 1 {
				t.Errorf("%s step %d shows %d times:\n%s", name, i, n, text)
			}
		}
		if n := strings.Count(text, "list_directory"); n != threeTaskSteps {
			t.Errorf("%s shows %d list_directory steps:\n%s", name, n, text)
		}
		if n := strings.Count(text, name+" finished its work."); n != 1 {
			t.Errorf("%s result shows %d times:\n%s", name, n, text)
		}
		for _, other := range threeTaskNames {
			if other != name && strings.Contains(text, other+" step") {
				t.Errorf("%s pane shows %s's steps:\n%s", name, other, text)
			}
		}
	}
	// The draft typed while the tasks streamed is intact in the prompt.
	var draft string
	probe(func(s *screen) { draft = s.input })
	if draft != typed {
		t.Fatalf("draft=%q, typed %q", draft, typed)
	}
}
