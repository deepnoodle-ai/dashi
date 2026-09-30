package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
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
				body = "data: " + `{"type":"response.completed","response":{"object":"response","status":"completed","model":"same/model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + body + `"}]}]}}` + "\n\n"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}})
	}
	store := session.NewStore(t.TempDir())
	store.DisableSync()
	e := NewEngine(EngineConfig{Dir: t.TempDir(), Client: p.client, Model: "same/model", Store: store, Preferences: p})
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
	if err != nil || result.Outcome.Answer != "two answer" {
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
	if err != nil || result.Outcome.Answer != "new answer" || counts["one"] != 1 || counts["two"] != 1 {
		t.Fatalf("result=%+v error=%v counts=%v", result, err, counts)
	}
	first, err := e.handle().Runner.Recorder.(dashi.Loader).Load(context.Background(), dashi.TurnID(e.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	view, err := dashi.Replay(first)
	if err != nil || view.Stop.Kind != dashi.StopCanceled || view.Stop.Reason != dashi.CloseReason {
		t.Fatalf("old turn = %+v %v", view, err)
	}
}
