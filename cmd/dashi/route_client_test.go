package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

type routeTransport func(*http.Request) (*http.Response, error)

func (f routeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTurnClientSendsFrozenRequestOnOriginalDestination(t *testing.T) {
	counts := map[string]int{}
	client := func(provider string) llm.Client {
		c, err := responses.New(responses.Config{Provider: provider, Endpoint: "https://" + provider + ".test/responses", Client: &http.Client{Transport: routeTransport(func(r *http.Request) (*http.Response, error) {
			counts[provider]++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)), Request: r}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := client("first"), client("second")
	forwarder := newTurnClient(first)
	input := llm.Input{Model: "same/model", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}}
	request, err := forwarder.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	forwarder.bind(second, nil)
	later, err := forwarder.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if forwarder.Provider() != "second" {
		t.Fatal("next destination not bound")
	}
	if _, err := forwarder.Send(t.Context(), llm.Call{Request: request}); err != nil {
		t.Fatal(err)
	}
	if _, err := forwarder.Send(t.Context(), llm.Call{Request: later}); err != nil {
		t.Fatal(err)
	}
	// A reused frozen request retains the same route.
	if _, err := forwarder.Send(t.Context(), llm.Call{Request: request}); err != nil {
		t.Fatal(err)
	}
	if counts["first"] != 2 || counts["second"] != 1 {
		t.Fatalf("wrong route counts=%v", counts)
	}
	blocked := errors.New("binding changed")
	forwarder.bind(nil, blocked)
	if _, err := forwarder.Prepare(t.Context(), input); !errors.Is(err, blocked) {
		t.Fatalf("blocked preparation=%v", err)
	}
}

func TestRecordedRouteFingerprintPrecedesFirstAttempt(t *testing.T) {
	d := configuredDestination{ID: "team", Definition: destinationDefinition{API: "openai.responses", Endpoint: "https://team.test/responses", Models: []string{"same/model"}, DefaultModel: "same/model", CredentialEnv: "TEAM_KEY"}}
	agent, err := selectionAgent(resolvedSelection{Destination: d, Model: "same/model"}, llm.Settings{Temperature: new(0.0)})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*configuredDestination){
		func(d *configuredDestination) { d.Definition.Endpoint = "https://changed.test/responses" },
		func(d *configuredDestination) { d.Definition.API = "openai.completions" },
		func(d *configuredDestination) { d.Definition.CredentialEnv = "OTHER_KEY" },
		func(d *configuredDestination) { d.Definition.Models = append(d.Definition.Models, "new/model") },
	} {
		changed := d
		change(&changed)
		lookups, factories := 0, 0
		routes := newRouteClients(map[string]configuredDestination{"team": changed}, nil, func(string) (string, bool) { lookups++; return "fixture", true })
		routes.factory = func(configuredDestination, func(string) (string, bool), bool) (llm.Client, error) {
			factories++
			return nil, nil
		}
		if _, err := routes.clientFor(agent); err == nil || lookups != 0 || factories != 0 {
			t.Fatalf("changed route allowed: %v lookups=%d factories=%d", err, lookups, factories)
		}
	}
	original := map[string]configuredDestination{"team": d}
	routes := newRouteClients(original, nil, func(string) (string, bool) { return "fixture", true })
	delete(original, "team")
	if _, err := routes.clientFor(agent); err != nil {
		t.Fatalf("registry was not frozen: %v", err)
	}
	if agent.Settings.Temperature == nil || *agent.Settings.Temperature != 0 {
		t.Fatal("controls lost")
	}
}

func TestRecordedProjectRouteStillRequiresTrust(t *testing.T) {
	d := configuredDestination{ID: "team", Project: true, Definition: destinationDefinition{API: "openai.responses", Endpoint: "https://team.test/responses", Models: []string{"model"}, DefaultModel: "model", CredentialEnv: "TEAM_KEY"}}
	agent, err := selectionAgent(resolvedSelection{Destination: d, Model: "model"}, llm.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	routes := newRouteClients(map[string]configuredDestination{"team": d}, nil, func(string) (string, bool) { lookups++; return "fixture", true })
	if _, err := routes.clientFor(agent); err == nil || lookups != 0 {
		t.Fatalf("project trust bypass: %v lookups=%d", err, lookups)
	}
	fingerprint, _ := d.fingerprint()
	routes.grants[fingerprint] = true
	if _, err := routes.clientFor(agent); err != nil || lookups != 1 {
		t.Fatalf("granted route failed: %v lookups=%d", err, lookups)
	}
}

func TestLegacyRouteDoesNotInventCustomEndpoint(t *testing.T) {
	registry := builtinDestinations()
	routes := newRouteClients(registry, nil, func(string) (string, bool) { return "fixture", true })
	agent := dashi.Agent{ID: "dashi-cli", Revision: "auto:legacy", Provider: "openai", API: "openai.responses", Model: registry["openai"].Definition.DefaultModel}
	if _, err := routes.clientFor(agent); err != nil {
		t.Fatal(err)
	}
	custom := registry["openai"]
	custom.ID, custom.Definition.Endpoint, custom.BuiltinAliases = "custom", "https://custom.test/responses", nil
	routes = newRouteClients(map[string]configuredDestination{"custom": custom}, nil, func(string) (string, bool) { t.Fatal("legacy route read credential"); return "", false })
	agent.Provider = "custom"
	if _, err := routes.clientFor(agent); err == nil {
		t.Fatal("legacy custom endpoint accepted")
	}
}
