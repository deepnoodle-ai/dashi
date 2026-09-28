// Package providertest holds provider contract checks shared by the root
// module and provider adapters published as separate modules. Each check
// takes one configured client, so a module keeps its own table of cases and
// the root module never imports an adapter with heavy dependencies.
package providertest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Events encodes values as a server-sent event stream.
func Events(values ...string) string {
	return "data: " + strings.Join(values, "\n\ndata: ") + "\n\n"
}

// CatalogDefaults checks that a client at its public endpoint applies the
// documented catalog facts for model without configuration.
func CatalogDefaults(t *testing.T, client llm.Client, model string) {
	t.Helper()
	info, ok := client.ModelInfo(model)
	if !ok {
		t.Fatalf("%s is unknown at the public endpoint", model)
	}
	if info.Evidence.Owner != catalog.Owner {
		t.Fatalf("evidence owner = %q", info.Evidence.Owner)
	}
	if info.Evidence.API != client.API() {
		t.Fatalf("evidence API = %q", info.Evidence.API)
	}
	if info.Evidence.Method != llm.EvidenceDocumentation {
		t.Fatalf("evidence method = %q", info.Evidence.Method)
	}
	prep, err := client.Prepare(
		context.Background(),
		llm.Input{
			Model:    model,
			Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
	)
	if err != nil {
		t.Fatalf("prepare with catalog knowledge: %v", err)
	}
	snapshot := prep.Snapshot()
	if snapshot.ModelInfo == nil {
		t.Fatal("snapshot did not record the catalog facts")
	}
	if slices.Contains(snapshot.Unverified, llm.FeatureText) {
		t.Fatal("documented text support was recorded as unverified")
	}
}

// CatalogModelsPrepare checks that every catalog model the provider serves
// at the client's API prepares a plain request, with the shared default output
// limit lowered to the model's documented limit. Catalog facts must never make
// a request fail that would succeed without them.
func CatalogModelsPrepare(t *testing.T, provider string, client llm.Client) {
	t.Helper()
	temperature := 0.7
	for _, entry := range catalog.Entries(provider) {
		if !slices.Contains(entry.APIs, client.API()) {
			continue
		}
		request, err := client.Prepare(
			context.Background(),
			llm.Input{
				Model:    entry.Model,
				Stream:   true,
				Settings: llm.Settings{Temperature: &temperature},
				Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
			},
		)
		if err != nil {
			t.Fatalf("%s at %s: %v", entry.Model, client.API(), err)
		}
		want := llm.DefaultMaxTokens
		if limit := entry.Capabilities.MaxOutputTokens; limit > 0 {
			want = min(want, limit)
		}
		if got := request.Effective().MaxTokens; got != want {
			t.Fatalf(
				"%s at %s: default max tokens = %d, want %d",
				entry.Model,
				client.API(),
				got,
				want,
			)
		}
	}
}

// TransportFixture is one provider's wire responses for a single "Hello"
// answer with ID r1, three input tokens, and two output tokens.
type TransportFixture struct {
	// Complete is the blocking response body.
	Complete string
	// Stream is the server-sent event body.
	Stream string
	// New configures a client against the test server URL.
	New func(url string) llm.Client
}

// FinalFactsAgree checks that a provider adapter reports the same
// agent-facing final facts in blocking and streaming mode.
func FinalFactsAgree(t *testing.T, fixture TransportFixture) {
	t.Helper()
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stream := strings.Contains(r.URL.Path, "streamGenerateContent") ||
				r.URL.Query().Get("alt") == "sse"
			if !stream {
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("request body: %v", err)
				}
				stream = body.Stream
			}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, fixture.Stream)
			} else {
				_, _ = io.WriteString(w, fixture.Complete)
			}
		}),
	)
	t.Cleanup(server.Close)
	client := fixture.New(server.URL)
	for _, stream := range []bool{false, true} {
		input := llm.Input{
			Model:    "requested",
			Stream:   stream,
			Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		}
		request, err := client.Prepare(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		var result llm.Result
		if stream {
			deltas, terminals := 0, 0
			for event, err := range llm.Stream(
				context.Background(),
				client,
				llm.Call{Request: request},
			) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Delta != nil {
					deltas++
				}
				if event.Result != nil {
					terminals++
					result = *event.Result
				}
			}
			if deltas == 0 || terminals != 1 {
				t.Fatalf("stream events: deltas=%d terminals=%d", deltas, terminals)
			}
		} else {
			result, err = client.Send(
				context.Background(),
				llm.Call{Request: request},
			)
			if err != nil {
				t.Fatal(err)
			}
		}
		if result.Stop.Kind != llm.StopFinished ||
			result.Message.Text() != "Hello" ||
			result.Usage == nil ||
			result.Usage.InputTokens != 3 ||
			result.Usage.OutputTokens != 2 ||
			result.ID != "r1" {
			t.Fatalf("stream=%t result=%+v", stream, result)
		}
	}
}

// ReminderRendered checks that a reminder is rendered into the prepared wire
// body under role and recorded as a content change.
func ReminderRendered(t *testing.T, client llm.Client, role llm.Role) string {
	t.Helper()
	req, err := client.Prepare(
		context.Background(),
		llm.Input{
			Model: "example-model",
			Messages: []llm.Message{
				llm.UserMessage(llm.Text("question")),
				llm.ReminderMessage(llm.Reminder{
					Name:    "scope",
					Tier:    llm.ReminderOperator,
					Content: "visible reminder",
				}),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := req.Snapshot()
	if len(snapshot.ContentChanges) != 1 ||
		snapshot.ContentChanges[0].Action != llm.ContentChangeRenderReminder ||
		snapshot.ContentChanges[0].ToRole != role ||
		snapshot.Input.Messages[1].Content[0].Kind != llm.ContentReminder {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	body := string(req.Body())
	if !strings.Contains(body, "visible reminder") {
		t.Fatalf("wire reminder missing: %s", body)
	}
	return body
}
