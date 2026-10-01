package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/session"
)

func routedTestEngine(t *testing.T, dispatch func(string) (int, string)) (*Engine, *cliPreferences) {
	t.Helper()
	registry := map[string]configuredDestination{}
	for _, id := range []string{"one", "two"} {
		registry[id] = configuredDestination{ID: id, Definition: destinationDefinition{API: "openai.responses", Endpoint: "https://" + id + ".test/responses", Models: []string{"same/model"}, DefaultModel: "same/model"}}
	}
	selected := resolvedSelection{Destination: registry["one"], Model: "same/model", Source: "fixture"}
	p := &cliPreferences{launch: launchPreferences{Selection: selected}, routes: newRouteClients(registry, nil, func(string) (string, bool) { return "", false }), client: newTurnClient(nil), writer: userPreferenceWriter{path: filepath.Join(t.TempDir(), "settings.json")}}
	p.routes.factory = func(d configuredDestination, lookup func(string) (string, bool), trusted bool) (llm.Client, error) {
		return responses.New(responses.Config{Provider: d.ID, Endpoint: d.Definition.Endpoint, Client: &http.Client{Transport: routeTransport(func(r *http.Request) (*http.Response, error) {
			status, body := dispatch(d.ID)
			kind := "application/json"
			if status == 200 {
				kind = "text/event-stream"
				encoded, _ := json.Marshal(body)
				body = "data: " + `{"type":"response.completed","response":{"object":"response","status":"completed","model":"same/model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]}}` + "\n\n"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}})
	}
	store := sessionstore.NewStore(t.TempDir())
	store.DisableSync()
	dir, err := canonicalWorkspace(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineConfig{Dir: dir, Client: p.client, Model: "same/model", Store: store, Preferences: p})
	saved, err := store.Create(e.cfg.Dir, e.Model())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Open(saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e, p
}

func TestContinueThenQueueUsesRecordedAndNextDestinations(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	e, p := routedTestEngine(t, func(provider string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		counts[provider]++
		if provider == "one" && counts[provider] == 1 {
			return 429, `{"error":{"message":"fixture rate limited","type":"rate_limit_error"}}`
		}
		return 200, provider + " answer"
	})
	if first, err := e.Prompt(t.Context(), "old route"); err != nil || first.Outcome.Stop.Kind != dashi.StopFailed {
		t.Fatalf("expected recorded model failure: %+v %v", first, err)
	}
	last := e.Last(t.Context())
	if !last.CanGoOn() {
		t.Fatalf("cannot continue: %+v", last)
	}
	p.mu.Lock()
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "session change"}
	p.mu.Unlock()
	if err := e.Queue("new route"); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resume(t.Context())
	if err != nil || result.Outcome.Reply != "two answer" {
		t.Fatalf("continue/queue result=%+v error=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["one"] != 2 || counts["two"] != 1 {
		t.Fatalf("route counts=%v", counts)
	}
	n := 0
	for turn, err := range e.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			t.Fatal(err)
		}
		n++
		want := "one"
		if n == 2 {
			want = "two"
		}
		if view.Agent.Provider != want || !strings.HasPrefix(view.Agent.Revision, cliRouteRevision) {
			t.Fatalf("turn %d agent=%+v", n, view.Agent)
		}
	}
	if n != 2 {
		t.Fatalf("turns=%d", n)
	}
}

func TestNewDestinationClosesInterruptedOldTurnFirst(t *testing.T) {
	counts := map[string]int{}
	e, p := routedTestEngine(t, func(provider string) (int, string) {
		counts[provider]++
		if provider == "one" {
			return 429, `{"error":{"message":"fixture rate limited","type":"rate_limit_error"}}`
		}
		return 200, "new answer"
	})
	_, _ = e.Prompt(t.Context(), "old")
	p.mu.Lock()
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "session change"}
	p.mu.Unlock()
	result, err := e.Prompt(t.Context(), "new")
	if err != nil || result.Outcome.Reply != "new answer" || counts["one"] != 1 || counts["two"] != 1 {
		t.Fatalf("result=%+v error=%v counts=%v", result, err, counts)
	}
	first, err := e.handle().Runner.Recorder.(dashi.Loader).Load(context.Background(), session.TurnID(e.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	view, err := dashi.Replay(first)
	if err != nil || view.Stop.Kind != dashi.StopCanceled || view.Stop.Reason != session.CloseReason {
		t.Fatalf("old turn = %+v %v", view, err)
	}
}

func TestNewDestinationCompletesRecordedOutputBeforeQueuedWork(t *testing.T) {
	counts := map[string]int{}
	e, p := routedTestEngine(t, func(provider string) (int, string) {
		counts[provider]++
		if provider == "one" {
			return 200, `{"answer":"42"}`
		}
		return 200, "new answer"
	})
	start := dashi.Say("old structured answer")
	start.CommandID = dashi.NewID()
	e.mu.Lock()
	start.Agent = e.agent()
	e.mu.Unlock()
	start.Agent.Limits = dashi.Limits{ModelAttempts: 1}
	start.OutputSchema = json.RawMessage(`{"type":"object"}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.handle().Runner.Recorder = &cancelRouteAfterStart{Recorder: e.handle().Runner.Recorder.(*filerecorder.Recorder), cancel: cancel, kind: dashi.StepOutputRecorded}
	first, err := e.run(ctx, start)
	if err != nil || first.Outcome.Output == nil || first.Outcome.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("fixture: %+v %v", first, err)
	}
	p.mu.Lock()
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "session change"}
	p.mu.Unlock()
	if err := e.Queue("queued new route"); err != nil {
		t.Fatal(err)
	}
	result, err := e.Prompt(t.Context(), "new route")
	if err != nil || result.Outcome.Reply != "new answer" || counts["one"] != 1 || counts["two"] != 1 {
		t.Fatalf("result=%+v err=%v counts=%v", result, err, counts)
	}
	firstTurn, err := e.handle().Runner.Recorder.(dashi.Loader).Load(t.Context(), session.TurnID(e.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	view, err := dashi.Replay(firstTurn)
	if err != nil || view.Stop.Kind != dashi.StopCompleted || view.Agent.Provider != "one" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

type cancelRouteAfterStart struct {
	*filerecorder.Recorder
	cancel context.CancelFunc
	kind   dashi.StepKind
}

func (r *cancelRouteAfterStart) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == r.kind {
		r.cancel()
	}
	return nil
}
func TestReopenedTurnKeepsRouteBeforeFirstModelAttempt(t *testing.T) {
	calls := 0
	e, p := routedTestEngine(t, func(string) (int, string) { calls++; return 200, "continued answer" })
	p.guidance, p.guidanceSet = "Recorded custom guidance", true
	old := e.Session()
	e.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := e.cfg
	cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
		return &cancelRouteAfterStart{Recorder: r, cancel: cancel, kind: dashi.StepTurnStarted}
	}
	e = NewEngine(cfg)
	defer e.Close()
	if err := e.Open(old); err != nil {
		t.Fatal(err)
	}
	_, _ = e.Prompt(ctx, "recorded before attempt")
	last := e.Last(t.Context())
	if last == nil || calls != 0 || last.View.ModelAttempts != 0 {
		t.Fatalf("last=%+v calls=%d", last, calls)
	}
	p.mu.Lock()
	p.launch.Selection = resolvedSelection{Destination: p.routes.registry["two"], Model: "same/model", Source: "session change"}
	p.mu.Unlock()
	if err := e.saveNextPreferences(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	saved, err := e.cfg.Store.Load(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := reopenedPreferences(saved.NextTurn, "")
	if err != nil || next.Provider != "two" {
		t.Fatalf("saved next=%+v %v", next, err)
	}
	reopened := NewEngine(e.cfg)
	reopened.cfg.recorder = nil
	if err := reopened.Open(saved); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	changed := p.routes.registry["one"]
	changed.Definition.Endpoint = "https://changed.test/responses"
	p.routes.registry["one"] = changed
	if _, err := reopened.Resume(t.Context()); err == nil || calls != 0 {
		t.Fatalf("changed route err=%v calls=%d", err, calls)
	}
	changed.Definition.Endpoint = "https://one.test/responses"
	p.routes.registry["one"] = changed
	p.guidance = "Changed launch guidance"
	result, err := reopened.Resume(t.Context())
	if err != nil || result.Outcome.Reply != "continued answer" || result.Outcome.Agent.Provider != "one" || !strings.Contains(result.Outcome.Agent.Instructions, "Recorded custom guidance") || strings.Contains(result.Outcome.Agent.Instructions, "Changed launch guidance") || calls != 1 {
		t.Fatalf("resume=%+v %v calls=%d", result, err, calls)
	}
}

func TestSummaryStreamingAndPhysicalReopenUseOnlyPublicText(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("wrong factory"); return 0, "" })
	p.launch.Controls.Settings.ReasoningSummary = new(true)
	p.launch.Controls.ShowThinking = true
	p.routes.factory = func(d configuredDestination, _ func(string) (string, bool), _ bool) (llm.Client, error) {
		return responses.New(responses.Config{Provider: d.ID, Endpoint: d.Definition.Endpoint, Client: &http.Client{Transport: routeTransport(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"summary":"auto"`) {
				t.Error("summary request was not encoded")
			}
			events := []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}`,
				`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
				`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"preview"}`,
				`{"type":"response.completed","response":{"object":"response","status":"completed","output":[{"type":"reasoning","encrypted_content":"NEVER_DISPLAY_PRIVATE","summary":[{"type":"summary_text","text":"public settled summary"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`,
			}
			body := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}})
	}
	var live Transcript
	e.cfg.Emit = func(b Block) { live.Apply(b) }
	if result, err := e.Prompt(t.Context(), "show public summary"); err != nil || result.Outcome.Reply != "answer" {
		t.Fatalf("result=%+v %v", result, err)
	}
	assertSummary := func(blocks []Block) {
		t.Helper()
		n := 0
		for _, b := range blocks {
			if strings.Contains(FormatBlock(b), "NEVER_DISPLAY_PRIVATE") {
				t.Fatal("private artifact displayed")
			}
			if b.Kind == KindReasoning {
				n++
				if b.Text != "public settled summary" || !b.Done {
					t.Fatalf("summary=%+v", b)
				}
			}
		}
		if n != 1 {
			t.Fatalf("summary blocks=%d", n)
		}
	}
	assertSummary(live.Blocks)
	if got := e.inspectSettings(t.Context()); got.Recorded == nil || got.Recorded.Summary != "public summary returned" {
		t.Fatalf("inspection=%+v", got)
	}
	id := e.Session().ID
	e.Close()
	saved, err := e.cfg.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	p.launch.Controls.Settings.ReasoningSummary = new(false)
	p.launch.Controls.ShowThinking = false
	reopened := NewEngine(e.cfg)
	if err := reopened.Open(saved); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertSummary(reopened.Scrollback(t.Context()))
}
