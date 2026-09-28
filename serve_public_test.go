package dashi_test

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// These tests run the web handler from the running-turns guide, "Serving
// live updates", against a real turn. The handler below is the guide's code.

type Server struct {
	store *dashi.MemoryRecorder
	hub   *hub
}

func (s *Server) canView(*http.Request, string) bool { return true }

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Authorize first: Follow waits forever on a turn that does not exist.
	if !s.canView(r, id) {
		http.NotFound(w, r)
		return
	}
	// EventSource resends the last id it received as Last-Event-ID.
	after, _ := strconv.ParseInt(cmp.Or(
		r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"),
	), 10, 64)
	// A client reconnecting after the end gets 204, which tells
	// EventSource to stop reconnecting.
	if after > 0 {
		last, err := s.store.StepsAfter(r.Context(), id, after-1, 1)
		if err == nil && len(last) == 1 && last[0].Stop != nil &&
			last[0].Stop.Terminal {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher).Flush

	// Subscribe before following, so no preview falls between the two.
	previews, unsubscribe := s.hub.Subscribe(id) // fed by Runner.Observer
	defer unsubscribe()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	type read struct {
		step dashi.Step
		err  error
	}
	steps := make(chan read)
	go func() {
		defer close(steps)
		for step, err := range dashi.Follow(ctx, s.store, id, after) {
			select {
			case steps <- read{step, err}:
			case <-ctx.Done():
				return
			}
		}
	}()

	settled := map[string]bool{} // messages and calls a sent step settled
	for {
		select {
		case got, ok := <-steps:
			switch {
			case !ok:
				return // the client left
			case errors.Is(got.err, dashi.ErrCursorAhead):
				send(w, "", "reset", nil) // the client drops its state, follows from 0
				return
			case got.err != nil:
				return // the client reconnects from its last id
			}
			step := got.step
			if key := step.Settles(); key != "" {
				settled[key] = true
			}
			send(w, strconv.FormatInt(step.Seq, 10), "step", project(step))
			if step.Stop != nil && step.Stop.Terminal {
				send(w, "", "end", nil) // the page calls EventSource.close
				flush()
				return
			}
		case e, ok := <-previews:
			switch {
			case !ok:
				previews = nil // the hub closed; keep following steps
			case e.Gap: // this message's preview has a hole; wait for its step
				settled[e.MessageID] = true
				send(w, "", "gap", e.MessageID)
			case e.Delta != nil && !settled[e.MessageID]:
				send(w, "", "delta", projectDelta(e))
			case e.Progress != nil && !settled[e.Progress.CallID]:
				send(w, "", "progress", e.Progress)
			}
		}
		flush()
	}
}

// send writes one server-sent event.
func send(w http.ResponseWriter, id, event string, data any) {
	raw, _ := json.Marshal(data)
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

type view struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
}

func project(step dashi.Step) view {
	return view{Seq: step.Seq, Kind: string(step.Kind)}
}

func projectDelta(e dashi.Event) string {
	return e.Delta.Text
}

// hub fans the Observer's previews out to subscribers.
type hub struct {
	mu         sync.Mutex
	subs       map[string][]chan dashi.Event
	subscribed chan struct{}
}

func newHub() *hub {
	return &hub{subs: map[string][]chan dashi.Event{}, subscribed: make(chan struct{}, 8)}
}

func (h *hub) Subscribe(id string) (<-chan dashi.Event, func()) {
	ch := make(chan dashi.Event, 1024)
	h.mu.Lock()
	h.subs[id] = append(h.subs[id], ch)
	h.mu.Unlock()
	h.subscribed <- struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, c := range h.subs[id] {
			if c == ch {
				h.subs[id] = append(h.subs[id][:i], h.subs[id][i+1:]...)
				close(ch)
				return
			}
		}
	}
}

func (h *hub) observe(e dashi.Event) {
	if e.Step != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[e.TurnID] {
		select {
		case ch <- e:
		default: // a real hub sends a gap here
		}
	}
}

// deltaClient streams a reply as several text deltas. Before it returns, it
// waits until the client has read them: the handler may otherwise pick the
// settled step first and rightly drop deltas that arrive after it.
type deltaClient struct{ read <-chan struct{} }

func (deltaClient) Provider() string                       { return "test" }
func (deltaClient) API() string                            { return "test.chat" }
func (deltaClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }

func (deltaClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (c deltaClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	for _, word := range []string{"one ", "two ", "three"} {
		if call.Sink != nil {
			call.Sink(llm.Delta{Kind: llm.DeltaText, Text: word})
		}
	}
	select {
	case <-c.read:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("one two three")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}

type sse struct{ id, event, data string }

// readEvents parses a stream until it ends, calling onEvent for each event.
func readEvents(t *testing.T, resp *http.Response, onEvent func(sse)) []sse {
	t.Helper()
	var events []sse
	var cur sse
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			events = append(events, cur)
			if onEvent != nil {
				onEvent(cur)
			}
			cur = sse{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return events
}

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{store: dashi.NewMemoryRecorder(), hub: newHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /turns/{id}/events", s.events)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func get(t *testing.T, url, lastID string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestGuideHandlerServesATurn(t *testing.T) {
	s, srv := newServer(t)
	url := srv.URL + "/turns/t1/events"
	got := make(chan []sse, 1)
	deltasRead := make(chan struct{})
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			got <- nil
			return
		}
		defer resp.Body.Close()
		deltas := 0
		got <- readEvents(t, resp, func(e sse) {
			if e.event == "delta" {
				if deltas++; deltas == 3 {
					close(deltasRead)
				}
			}
		})
	}()
	select {
	case <-s.hub.subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not subscribe")
	}
	runner := dashi.Runner{
		Agent:    dashi.Agent{Model: "fixture"},
		Client:   deltaClient{read: deltasRead},
		Recorder: s.store,
		Observer: s.hub.observe,
	}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text("count"))},
	})
	if err != nil || !out.Stop.Terminal {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	var events []sse
	select {
	case events = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not end after the terminal stop")
	}
	var steps, deltas int
	for _, e := range events {
		switch e.event {
		case "step":
			steps++
		case "delta":
			deltas++
		}
	}
	if steps != len(out.Turn.Steps) {
		t.Fatalf("sent %d steps; the turn has %d: %+v", steps, len(out.Turn.Steps), events)
	}
	// The model_requested step carries the deltas' MessageID but settles
	// nothing, so the deltas that follow it still reach the client.
	if deltas != 3 {
		t.Fatalf("sent %d deltas; want 3: %+v", deltas, events)
	}
	if last := events[len(events)-1]; last.event != "end" {
		t.Fatalf("last event %+v; want end", last)
	}

	// A browser reconnecting with the terminal step's id is told to stop.
	resp := get(t, url, strconv.FormatInt(int64(len(out.Turn.Steps)), 10))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reconnect after the end: status %d; want 204", resp.StatusCode)
	}

	// A cursor from elsewhere resets the client.
	events = readEvents(t, get(t, url, "99"), nil)
	if len(events) != 1 || events[0].event != "reset" {
		t.Fatalf("foreign cursor got %+v; want one reset", events)
	}
}

// The guide shows this file's handler; keep the two the same.
func TestGuideHandlerMatchesGuide(t *testing.T) {
	const start, end = "func (s *Server) events(", "\n}\n"
	extract := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		i := strings.Index(text, start)
		if i < 0 {
			t.Fatalf("%s has no events handler", path)
		}
		j := strings.Index(text[i:], end)
		if j < 0 {
			t.Fatalf("%s: events handler has no end", path)
		}
		return strings.Join(strings.Fields(text[i:i+j+len(end)]), " ")
	}
	guide := extract("docs/guides/running-turns.md")
	test := extract("serve_public_test.go")
	if guide != test {
		t.Fatal("the guide's events handler differs from serve_public_test.go; update one to match the other")
	}
}
