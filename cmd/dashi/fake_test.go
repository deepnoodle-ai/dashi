package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// reply is one scripted model answer.
type reply func(ctx context.Context, call llm.Call) (llm.Result, error)

// fakeClient answers model calls from a script. It records each input, so a
// test can check the history a turn sent.
type fakeClient struct {
	mu     sync.Mutex
	script []reply
	inputs []llm.Input
	sends  int
}

func (*fakeClient) Provider() string { return "test" }

func (*fakeClient) API() string { return "test.chat" }

func (*fakeClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c *fakeClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	c.mu.Unlock()
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

func (c *fakeClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.mu.Lock()
	i := c.sends
	c.sends++
	var r reply
	if i < len(c.script) {
		r = c.script[i]
	}
	c.mu.Unlock()
	if r == nil {
		return llm.Result{}, errors.New("unexpected model call")
	}
	res, err := r(ctx, call)
	if res.Requested.Provider == "" {
		res.Requested = llm.ModelRef{Provider: "test", Model: "fixture"}
	}
	return res, err
}

func (c *fakeClient) inputAt(i int) llm.Input {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inputs[i]
}

// say answers with text, streamed in two pieces.
func say(text string) reply {
	return func(_ context.Context, call llm.Call) (llm.Result, error) {
		half := len(text) / 2
		if call.Sink != nil {
			call.Sink(llm.Delta{Kind: llm.DeltaText, Text: text[:half]})
			call.Sink(llm.Delta{Kind: llm.DeltaText, Text: text[half:]})
		}
		return llm.Result{
			Message: llm.AssistantMessage(llm.Text(text)),
			Stop:    llm.Stop{Kind: llm.StopFinished},
			Usage:   &llm.Usage{InputTokens: 100, OutputTokens: 20},
		}, nil
	}
}

// callTool answers with one tool call.
func callTool(id, name, args string) reply {
	return func(context.Context, llm.Call) (llm.Result, error) {
		call := tool.Call{ID: id, Name: name, Args: []byte(args)}
		return llm.Result{
			Message: llm.AssistantMessage(llm.Content{
				Kind:     llm.ContentToolCall,
				ToolCall: &call,
			}),
			Stop:  llm.Stop{Kind: llm.StopToolUse},
			Usage: &llm.Usage{InputTokens: 50, OutputTokens: 10},
		}, nil
	}
}

// blockUntilDone waits for the request context to end, like a model call
// that the user cuts short.
func blockUntilDone(started chan<- struct{}) reply {
	return func(ctx context.Context, _ llm.Call) (llm.Result, error) {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return llm.Result{}, ctx.Err()
	}
}

// recorderUI is a UI that keeps every block and answers approvals from a
// script.
type recorderUI struct {
	mu        sync.Mutex
	tr        Transcript
	log       []Block
	approvals []ApprovalRequest
	answer    func(ctx context.Context, r ApprovalRequest) (Approval, error)
	quit      chan struct{}
	quitOnce  sync.Once
	resets    int
}

func newRecorderUI() *recorderUI {
	return &recorderUI{quit: make(chan struct{})}
}

func (u *recorderUI) Emit(b Block) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.log = append(u.log, b)
	u.tr.Apply(b)
}

func (u *recorderUI) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tr = Transcript{}
	u.resets++
}

func (u *recorderUI) Approve(ctx context.Context, r ApprovalRequest) (Approval, error) {
	u.mu.Lock()
	u.approvals = append(u.approvals, r)
	answer := u.answer
	u.mu.Unlock()
	if answer == nil {
		return Approval{Allow: true}, nil
	}
	return answer(ctx, r)
}

func (u *recorderUI) Ask(context.Context, Question) (string, error) {
	return "", errors.New("not used")
}

func (u *recorderUI) Quit() {
	u.quitOnce.Do(func() { close(u.quit) })
}

// text returns the plain text of the transcript, one block per line.
func (u *recorderUI) text() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var s string
	for _, b := range u.tr.Blocks {
		s += FormatBlock(b) + "\n"
	}
	return s
}

func (u *recorderUI) blocks() []Block {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Block(nil), u.tr.Blocks...)
}

// harness builds an app on a fake client in a temp directory.
type harness struct {
	t      *testing.T
	dir    string
	store  *session.Store
	client *fakeClient
	ui     *recorderUI
	app    *app
	limit  int
	// recorder, when set, wraps the recorder of the next build.
	recorder func(*filerecorder.Recorder) dashi.Recorder
	// notes counts runs of the write_note tool.
	notes atomic.Int32
}

func newHarness(t *testing.T, script ...reply) *harness {
	t.Helper()
	return newHarnessLimit(t, 0, script...)
}

// newHarnessLimit is newHarness with a model-call limit per turn.
func newHarnessLimit(t *testing.T, attempts int, script ...reply) *harness {
	t.Helper()
	store := session.NewStore(t.TempDir())
	store.DisableSync()
	h := &harness{
		t:      t,
		dir:    t.TempDir(),
		store:  store,
		client: &fakeClient{script: script},
		ui:     newRecorderUI(),
		limit:  attempts,
	}
	h.build(Options{})
	return h
}

// build creates the app and opens a session per o. It can run twice to
// model a second process.
func (h *harness) build(o Options) {
	h.t.Helper()
	a, err := buildApp(h.t.Context(), appConfig{
		Dir:    h.dir,
		Model:  "fixture",
		Client: h.client,
		Store:  h.store,

		ModelAttempts: h.limit,
		recorder:      h.recorder,
	}, h.ui)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := a.openSession(h.store, h.dir, o); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(a.eng.Close)
	h.app = a
}

// turns returns the number of turns of the open session.
func (h *harness) turns() int {
	return h.app.eng.Turns(h.t.Context())
}

// turn loads turn n of the open session from its record.
func (h *harness) turn(n int) dashi.Turn {
	h.t.Helper()
	s := h.app.eng.Session()
	rec, err := h.store.Recorder(s)
	if err != nil {
		h.t.Fatal(err)
	}
	turn, err := rec.Load(h.t.Context(), dashi.TurnID(s.ID, n))
	if err != nil {
		h.t.Fatal(err)
	}
	return turn
}

// turnsOf returns the number of turns a stored session has.
func turnsOf(t *testing.T, st *session.Store, s *session.Session) int {
	t.Helper()
	rec, err := st.Recorder(s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := rec.LastTurn(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) input(line string) {
	h.t.Helper()
	h.app.ctrl.Input(line)
	h.app.ctrl.Wait()
}

func mustHave(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

// atomicCounter counts events across goroutines.
type atomicCounter = atomic.Int32

func slicesClone[T any](s []T) []T {
	return append([]T(nil), s...)
}

// inputText joins the text of every message the model was sent.
func inputText(in llm.Input) string {
	var b strings.Builder
	for _, m := range in.Messages {
		b.WriteString(m.Text())
		for _, c := range m.Content {
			if c.ToolResult != nil {
				for _, blk := range c.ToolResult.Output.Content {
					if blk.Text != nil {
						b.WriteString(blk.Text.Text)
					}
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
