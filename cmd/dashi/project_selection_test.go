package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestProjectSelectionStaysWorkspaceScoped(t *testing.T) {
	e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("preview dispatched"); return 0, "" })
	p.routes.user = map[string]configuredDestination{"one": p.routes.registry["one"]}
	project := p.routes.registry["two"]
	project.Project, project.Source = true, "workspace A/.dashi/settings.json"
	p.routes.registry["two"] = project
	p.routes.grants[mustHash(project)] = true
	if err := os.WriteFile(p.writer.path, []byte(`{"provider":"one","model":"same/model","unrelated":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
	ctrl.destinationModel(t.Context(), "two same/model", p)
	if p.agent().Provider != "two" {
		t.Fatal("selection not active")
	}
	var next modelSelection
	if err := json.Unmarshal(p.metadata(), &next); err != nil || next.Provider != "two" {
		t.Fatalf("session selection=%+v error=%v", next, err)
	}
	user, err := readPreferenceFile(p.writer.path, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveLaunchPreferences(Options{}, p.routes.user, p.routes.user, user, preferenceFile{}, preferenceFile{}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("workspace B startup failed: %v", err)
	}
	if user.Provider != "one" {
		t.Fatalf("global preference changed to %q", user.Provider)
	}
}

func TestMissingRequiredCredentialStopsDispatch(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"fixture missing credential"}}`)
	}))
	defer server.Close()
	d := builtinDestinations()["openai"]
	d.Definition.Endpoint = server.URL + "/v1/responses"
	client, err := newDestinationClient(d, func(string) (string, bool) { return "", false }, true)
	if err == nil {
		request, prepareErr := client.Prepare(t.Context(), llm.Input{Model: d.Definition.DefaultModel, Messages: []llm.Message{llm.UserMessage(llm.Text("fixture"))}})
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		_, sendErr := client.Send(t.Context(), llm.Call{Request: request})
		t.Fatalf("missing credential accepted: requests=%d send error=%v", requests.Load(), sendErr)
	}
	if client != nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") || requests.Load() != 0 {
		t.Fatalf("client=%v error=%v requests=%d", client, err, requests.Load())
	}
}

func TestProjectOverrideCannotBecomeGlobalSelection(t *testing.T) {
	for _, change := range []string{"endpoint", "models"} {
		t.Run(change, func(t *testing.T) {
			e, p := routedTestEngine(t, func(string) (int, string) { t.Fatal("preview dispatched"); return 0, "" })
			p.routes.user = map[string]configuredDestination{"one": p.routes.registry["one"], "two": p.routes.registry["two"]}
			project := p.routes.registry["two"]
			project.Project = true
			if change == "endpoint" {
				project.Definition.Endpoint = "https://other.example/v1/responses"
			} else {
				project.Definition.Models = append([]string{"project/model"}, project.Definition.Models...)
			}
			p.routes.registry["two"] = project
			p.routes.grants[mustHash(project)] = true
			original := []byte(`{"provider":"one","model":"same/model","unrelated":true}`)
			if err := os.WriteFile(p.writer.path, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
			ctrl.destinationModel(t.Context(), "two same/model", p)
			if p.agent().Provider != "two" {
				t.Fatal("session switch rejected")
			}
			saved, err := os.ReadFile(p.writer.path)
			if err != nil || string(saved) != string(original) {
				t.Fatalf("global binding changed: %s %v", saved, err)
			}
		})
	}
}

func TestRequiredCredentialsGateAllAdapters(t *testing.T) {
	missing := func(string) (string, bool) { return "", false }
	for id, d := range builtinDestinations() {
		if len(d.Definition.Models) == 0 {
			d.Definition.Models = []string{"fixture/model"}
			d.Definition.DefaultModel = "fixture/model"
		}
		if id == "ollama" {
			continue
		}
		t.Run(id, func(t *testing.T) {
			client, err := newDestinationClient(d, missing, true)
			if client != nil || err == nil {
				t.Fatalf("missing key accepted: %v %v", client, err)
			}
			for _, alias := range d.BuiltinAliases {
				if !strings.Contains(err.Error(), alias) {
					t.Fatalf("missing alias %s: %v", alias, err)
				}
			}
		})
	}
	for _, api := range []string{"openai.responses", "openai.completions", "anthropic.messages", "google.generate_content"} {
		t.Run(api, func(t *testing.T) {
			d := fixtureDestination("team", "https://proxy.example/v1/responses", "TEAM_LLM_KEY")
			d.Definition.API = api
			if api == "google.generate_content" {
				d.Definition.Endpoint = "https://proxy.example/v1/models/{model}:generateContent"
			}
			client, err := newDestinationClient(d, missing, true)
			if client != nil || err == nil || !strings.Contains(err.Error(), "TEAM_LLM_KEY") {
				t.Fatalf("missing custom key: %v %v", client, err)
			}
			d.Definition.CredentialEnv = ""
			if _, err = newDestinationClient(d, missing, true); err != nil {
				t.Fatalf("explicitly keyless route rejected: %v", err)
			}
		})
	}
}
