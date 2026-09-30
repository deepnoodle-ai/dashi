package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestRegisteredAdaptersPrepareWithoutDispatch(t *testing.T) {
	registry := builtinDestinations()
	for id, d := range registry {
		t.Run(id, func(t *testing.T) {
			if len(d.Definition.Models) == 0 {
				d.Definition.Models = []string{"publisher/fixture-v1"}
				d.Definition.DefaultModel = "publisher/fixture-v1"
			}
			client, err := newDestinationClient(d, func(string) (string, bool) { return "fixture-key", true }, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: d.Definition.DefaultModel, Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			if client.Provider() != id || client.API() != d.Definition.API || request.Snapshot().Input.Model != d.Definition.DefaultModel {
				t.Fatalf("wrong binding: %s %s %+v", client.Provider(), client.API(), request.Snapshot())
			}
		})
	}
	for _, api := range []string{"anthropic.messages", "openai.responses", "openai.completions", "google.generate_content"} {
		t.Run("custom/"+api, func(t *testing.T) {
			d := fixtureDestination("team", "https://proxy.example/v1/responses", "TEAM_LLM_KEY")
			d.Definition.API = api
			if api == "google.generate_content" {
				d.Definition.Endpoint = "https://proxy.example/v1/models/{model}:generateContent"
			}
			client, err := newDestinationClient(d, func(name string) (string, bool) {
				if name != "TEAM_LLM_KEY" {
					t.Fatalf("inherited %s", name)
				}
				return "fixture", true
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "acme/code-v1", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil || request.Snapshot().Input.Model != "acme/code-v1" {
				t.Fatalf("native ID changed: %v", err)
			}
			if _, known := client.ModelInfo("acme/code-v1"); known {
				t.Fatal("custom destination inherited public facts")
			}
		})
	}
}
func TestDestinationAdaptersRefuseRedirects(t *testing.T) {
	for _, api := range []string{"anthropic.messages", "openai.responses", "openai.completions", "google.generate_content"} {
		t.Run(api, func(t *testing.T) {
			var initial, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				initial.Add(1)
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			d := fixtureDestination("team", server.URL+"/v1/responses", "TEAM_LLM_KEY")
			d.Definition.API = api
			if api == "google.generate_content" {
				d.Definition.Endpoint = server.URL + "/v1/models/{model}:generateContent"
			}
			client, err := newDestinationClient(d, func(string) (string, bool) { return "fixture", true }, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "acme/code-v1", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Send(context.Background(), llm.Call{Request: request})
			if err == nil || initial.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("redirect: err=%v initial=%d target=%d", err, initial.Load(), redirected.Load())
			}
		})
	}
}
func TestOllamaAbsentEmptyAndPresentCredentials(t *testing.T) {
	for _, value := range []string{"", "fixture-key"} {
		for _, present := range []bool{false, true} {
			d := builtinDestinations()["ollama"]
			d.Definition.Models = []string{"qwen:7b"}
			d.Definition.DefaultModel = "qwen:7b"
			lookups := 0
			client, err := newDestinationClient(d, func(name string) (string, bool) {
				lookups++
				if name != "OLLAMA_API_KEY" {
					t.Fatalf("unexpected alias %s", name)
				}
				return value, present
			}, true)
			if err != nil || lookups != 1 {
				t.Fatalf("ollama: %v lookups=%d", err, lookups)
			}
			if _, err = client.Prepare(t.Context(), llm.Input{Model: "qwen:7b", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestUnsafeDestinationStopsFactoryBeforeCredentialLookup(t *testing.T) {
	d := fixtureDestination("team", "http://proxy.example/v1/responses", "TEAM_LLM_KEY")
	client, err := newDestinationClient(d, func(string) (string, bool) {
		t.Fatal("credential read before transport validation")
		return "fixture", true
	}, true)
	if err == nil || client != nil || strings.Contains(err.Error(), "fixture") {
		t.Fatalf("unsafe factory: %v", err)
	}
}
