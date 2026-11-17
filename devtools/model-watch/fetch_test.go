package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const testKey = "sk-test-SECRET"

// testClient returns a client for provider pointed at handler.
func testClient(t *testing.T, provider string, handler http.HandlerFunc) *apiClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newClient(providerSpec{Name: provider, BaseURL: srv.URL}, testKey)
}

func ids(models []listedModel) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.ID
	}
	return out
}

func TestListAnthropicPaginatesWithHeaders(t *testing.T) {
	c := testClient(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testKey || r.Header.Get("anthropic-version") != "2023-06-01" {
			http.Error(w, "bad headers", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("limit") != "1000" {
			http.Error(w, "no limit", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-a"},{"id":"ft:custom"}],"has_more":true,"last_id":"claude-a"}`)
		case "claude-a":
			fmt.Fprint(w, `{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`)
		}
	})
	got, err := listAnthropic(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-a", "claude-b"}; !slices.Equal(ids(got), want) {
		t.Errorf("ids = %q, want %q", ids(got), want)
	}
}

func TestAnthropicExists(t *testing.T) {
	c := testClient(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models/claude-sonnet-4-5" {
			fmt.Fprint(w, `{"id":"claude-sonnet-4-5-20250929"}`)
			return
		}
		http.NotFound(w, r)
	})
	if ok, err := anthropicExists(c, "claude-sonnet-4-5"); err != nil || !ok {
		t.Errorf("alias: %v, %v", ok, err)
	}
	if ok, err := anthropicExists(c, "claude-gone"); err != nil || ok {
		t.Errorf("gone: %v, %v", ok, err)
	}
}

func TestListGooglePaginatesAndClassifies(t *testing.T) {
	c := testClient(t, "google", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != testKey {
			http.Error(w, "no key", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-x","supportedGenerationMethods":["generateContent","countTokens"]}],"nextPageToken":"p2"}`)
			return
		}
		fmt.Fprint(w, `{"models":[{"name":"models/text-embed-x","supportedGenerationMethods":["embedContent"]}]}`)
	})
	got, err := listGoogle(c)
	if err != nil {
		t.Fatal(err)
	}
	want := []listedModel{{ID: "gemini-x"}, {ID: "text-embed-x", NotText: true}}
	if !slices.EqualFunc(got, want, func(a, b listedModel) bool { return a.ID == b.ID && a.NotText == b.NotText }) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestListMistralAliasesAndCapabilities(t *testing.T) {
	c := testClient(t, "mistral", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"id":"mistral-large-2512","aliases":["mistral-large-latest"],"capabilities":{"completion_chat":true}},
			{"id":"mistral-embed","aliases":[],"capabilities":{"completion_chat":false}},
			{"id":"ft:open-mistral:abc","capabilities":{"completion_chat":true}}]}`)
	})
	got, err := listMistral(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !slices.Equal(got[0].Aliases, []string{"mistral-large-latest"}) || got[0].NotText || !got[1].NotText {
		t.Errorf("got %+v", got)
	}
}

func TestListOpenAIStyleSkipsFineTunes(t *testing.T) {
	c := testClient(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gpt-5.4"},{"id":"ft:gpt-4o:org::abc"}]}`)
	})
	got, err := listOpenAIStyle(c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{"gpt-5.4"}) {
		t.Errorf("ids = %q", ids(got))
	}
}

// A key that starts near the 200-byte cut must not leak in part.
func TestErrorRedactsBeforeTruncating(t *testing.T) {
	c := testClient(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 190)+testKey+" is not valid", http.StatusUnauthorized)
	})
	_, err := listOpenAIStyle(c)
	if err == nil || strings.Contains(err.Error(), testKey[:6]) {
		t.Errorf("error leaks the key: %v", err)
	}
}

// Only the direct Anthropic check treats 404 as "not found"; a list
// endpoint that 404s is an error, not an empty list.
func TestListNotFoundIsError(t *testing.T) {
	c := testClient(t, "openai", http.NotFound)
	if got, err := listOpenAIStyle(c); err == nil {
		t.Errorf("got %v and no error", got)
	}
}

// Google sends the key in the query string. Neither an HTTP error that
// echoes it nor a connection error may reveal it or the URL.
func TestErrorsDoNotLeakKey(t *testing.T) {
	c := testClient(t, "google", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"API key `+testKey+` not valid"}`, http.StatusBadRequest)
	})
	_, err := listGoogle(c)
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "key=") {
		t.Errorf("HTTP error: %v", err)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens now
	c = newClient(providerSpec{Name: "google", BaseURL: srv.URL}, testKey)
	_, err = listGoogle(c)
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "key=") {
		t.Errorf("connection error: %v", err)
	}
}
