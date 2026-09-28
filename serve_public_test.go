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
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/feed"
	"github.com/deepnoodle-ai/dashi/llm"
)

// These tests run the web handler from the live-updates guide, "Serving
// live updates", against a real turn. The handler below is the guide's code.

type Server struct {
	store *dashi.MemoryRecorder
	hub   *feed.Hub // Runner.Observer = hub.Observe
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
	flush() // the browser sees the stream open before the first event

	// Steps come from the record; previews from the hub, each in its place.
	for e, err := range feed.Follow(r.Context(), s.hub, s.store, id, after) {
		switch {
		case errors.Is(err, dashi.ErrCursorAhead):
			send(w, "", "reset", nil) // the client drops its state, follows from 0
			flush()
			return
		case err != nil:
			return // the client reconnects from its last id
		case e.Step != nil:
			send(w, strconv.FormatInt(e.Seq, 10), "step", project(*e.Step))
			if e.Step.Stop != nil && e.Step.Stop.Terminal {
				send(w, "", "end", nil) // the page calls EventSource.close
			}
		case e.Gap:
			send(w, "", "gap", e.MessageID) // the page discards that preview
		default:
			send(w, "", "preview", projectPreview(e))
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

func projectPreview(e dashi.Event) string {
	if e.Delta != nil {
		return e.Delta.Text
	}
	return e.Progress.Message
}

// finalClient answers every call with one final message, streamed as three
// deltas. When set, it streams only once following has begun (a reader that
// joins mid-message rightly gets a gap), and it returns only once the client
// has read the deltas (a preview after its settling step is rightly dropped).
type finalClient struct{ following, read <-chan struct{} }

func (finalClient) Provider() string                       { return "test" }
func (finalClient) API() string                            { return "test.chat" }
func (finalClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }

func (finalClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (c finalClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	wait(ctx, c.following)
	for _, word := range []string{"do", "n", "e"} {
		if call.Sink != nil {
			call.Sink(llm.Delta{Kind: llm.DeltaText, Text: word})
		}
	}
	wait(ctx, c.read)
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("done")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}

func wait(ctx context.Context, ch <-chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
}

type sse struct{ id, event, data string }

// readEvents parses a stream until it ends, calling onEvent for each.
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
	s := &Server{store: dashi.NewMemoryRecorder(), hub: &feed.Hub{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /turns/{id}/events", s.events)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL + "/turns/t1/events"

	// The client connects before the turn starts and follows it live.
	got := make(chan []sse, 1)
	following := make(chan struct{})
	previewsRead := make(chan struct{})
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			got <- nil
			return
		}
		defer resp.Body.Close()
		steps, previews := 0, 0
		got <- readEvents(t, resp, func(e sse) {
			switch e.event {
			case "step":
				// A step means Follow has subscribed to the hub.
				if steps++; steps == 1 {
					close(following)
				}
			case "preview":
				if previews++; previews == 3 {
					close(previewsRead)
				}
			}
		})
	}()
	runner := dashi.Runner{
		Agent:    dashi.Agent{Model: "fixture"},
		Client:   finalClient{following: following, read: previewsRead},
		Recorder: s.store,
		Observer: s.hub.Observe,
	}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
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
	var seqs []string
	var text string
	for _, e := range events {
		switch e.event {
		case "step":
			seqs = append(seqs, e.id)
		case "preview":
			if e.id != "" {
				t.Fatalf("preview with id %q; only steps are resumable", e.id)
			}
			var word string
			_ = json.Unmarshal([]byte(e.data), &word)
			text += word
		}
	}
	if text != "done" {
		t.Fatalf("streamed %q; want done: %+v", text, events)
	}
	if len(seqs) != len(out.Turn.Steps) {
		t.Fatalf("sent %d steps; the turn has %d: %+v", len(seqs), len(out.Turn.Steps), events)
	}
	for i, id := range seqs {
		if id != strconv.Itoa(i+1) {
			t.Fatalf("step %d has id %q", i, id)
		}
	}
	if last := events[len(events)-1]; last.event != "end" {
		t.Fatalf("last event %+v; want end", last)
	}

	// A browser reconnecting mid-turn gets the rest.
	events = readEvents(t, get(t, url, "2"), nil)
	if len(events) != len(out.Turn.Steps)-2+1 || events[0].id != "3" {
		t.Fatalf("resume from 2 got %+v", events)
	}

	// A browser reconnecting with the terminal step's id is told to stop.
	resp := get(t, url, strconv.Itoa(len(out.Turn.Steps)))
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
	const start = "func (s *Server) events("
	extract := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		i := strings.Index(text, start)
		if i < 0 {
			t.Fatalf("%s has no %q", path, start)
		}
		j := strings.Index(text[i:], "\n}\n")
		if j < 0 {
			t.Fatalf("%s: %q has no end", path, start)
		}
		return strings.Join(strings.Fields(text[i:i+j+3]), " ")
	}
	if extract("docs/guides/live-updates.md") != extract("serve_public_test.go") {
		t.Fatal("the guide's events handler differs from serve_public_test.go; update one to match the other")
	}
}
