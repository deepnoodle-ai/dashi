package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestRegisteredAdaptersDispatchExactModelsToLocalFixtures(t *testing.T) {
	bodies := map[string]string{
		"openai.responses":        `{"id":"r","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture answer"}]}]}`,
		"openai.completions":      `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"fixture answer"}}]}`,
		"anthropic.messages":      `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		"google.generate_content": `{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture answer"}]},"finishReason":"STOP"}],"modelVersion":"acme/code-v1"}`,
	}
	for api, body := range bodies {
		t.Run(api, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if api == "google.generate_content" {
					if !strings.Contains(r.URL.EscapedPath(), "acme%2Fcode-v1") {
						t.Errorf("model path=%s", r.URL.EscapedPath())
					}
				} else {
					var sent struct{ Model string }
					if json.Unmarshal(raw, &sent) != nil || sent.Model != "acme/code-v1" {
						t.Errorf("wrong exact model")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			d := fixtureDestination("team", server.URL+"/v1/call", "TEAM_LLM_KEY")
			d.Definition.API = api
			if api == "google.generate_content" {
				d.Definition.Endpoint = server.URL + "/v1/models/{model}:generateContent"
			}
			client, err := newDestinationClient(d, func(string) (string, bool) { return "fixture-key", true }, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "acme/code-v1", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Send(t.Context(), llm.Call{Request: request})
			if err != nil || requests != 1 || result.Message.Text() != "fixture answer" {
				t.Fatalf("send=%+v %v requests=%d", result, err, requests)
			}
		})
	}
}

func TestOllamaDispatchBindsPlaceholderOrExplicitCredential(t *testing.T) {
	for _, state := range []struct {
		value   string
		present bool
	}{{"", false}, {"", true}, {"fixture-key", true}} {
		t.Run(fmt.Sprintf("%t/%s", state.present, state.value), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				want := state.value
				if want == "" {
					want = "ollama"
				}
				if got := r.Header.Get("X-Api-Key"); got != want {
					t.Errorf("credential was not explicitly bound")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()
			d := builtinDestinations()["ollama"]
			d.Definition.Endpoint = server.URL + "/v1/messages"
			d.Definition.Models = []string{"qwen:7b"}
			d.Definition.DefaultModel = "qwen:7b"
			client, err := newDestinationClient(d, func(string) (string, bool) { return state.value, state.present }, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "qwen:7b", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Send(t.Context(), llm.Call{Request: request}); err != nil || requests != 1 {
				t.Fatalf("send=%v requests=%d", err, requests)
			}
		})
	}
}
