package providers_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/catalog"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/grok"
	"github.com/deepnoodle-ai/kernel/providers/meta"
	"github.com/deepnoodle-ai/kernel/providers/mistral"
	"github.com/deepnoodle-ai/kernel/providers/ollama"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/providers/openrouter"
)

// publicClients builds every client whose default endpoint a catalog file
// documents, paired with the catalog provider and a model it serves there.
func publicClients(t *testing.T) []struct {
	provider, model string
	client          llm.Client
} {
	t.Helper()
	must := func(c llm.Client, err error) llm.Client {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return []struct {
		provider, model string
		client          llm.Client
	}{
		{
			provider: "anthropic",
			model:    "claude-sonnet-5",
			client:   must(messages.New(messages.Config{APIKey: "k"})),
		},
		{
			provider: "openai",
			model:    "gpt-6-luna",
			client:   must(responses.New(responses.Config{APIKey: "k"})),
		},
		{
			provider: "openai",
			model:    "gpt-5.4",
			client:   must(completions.New(completions.Config{APIKey: "k"})),
		},
		{
			provider: "google",
			model:    "gemini-3.8-flash",
			client:   must(gemini.New(gemini.Config{APIKey: "k"})),
		},
		{
			provider: "grok",
			model:    "grok-4.7",
			client:   must(grok.New(grok.Config{APIKey: "k"})),
		},
		{
			provider: "meta",
			model:    "muse-spark-1.3",
			client:   must(meta.New(meta.Config{APIKey: "k"})),
		},
		{
			provider: "mistral",
			model:    "mistral-medium-latest",
			client:   must(mistral.New(mistral.Config{APIKey: "k"})),
		},
	}
}

func TestPublicEndpointsUseCatalogByDefault(t *testing.T) {
	for _, tc := range publicClients(t) {
		t.Run(tc.provider+"/"+tc.client.API(), func(t *testing.T) {
			info, ok := tc.client.ModelInfo(tc.model)
			if !ok {
				t.Fatalf("%s is unknown at the public endpoint", tc.model)
			}
			if info.Evidence.Owner != catalog.Owner {
				t.Fatalf("evidence owner = %q", info.Evidence.Owner)
			}
			if info.Evidence.API != tc.client.API() {
				t.Fatalf("evidence API = %q", info.Evidence.API)
			}
			if info.Evidence.Method != llm.EvidenceDocumentation {
				t.Fatalf("evidence method = %q", info.Evidence.Method)
			}
			prep, err := tc.client.Prepare(context.Background(), llm.Input{
				Model: tc.model,
				Messages: []llm.Message{
					llm.UserMessage(llm.Text("hello")),
				},
			})
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
		})
	}
}

func TestEveryCatalogModelPreparesPlainRequest(t *testing.T) {
	// Catalog facts must never make a request fail that would succeed
	// without them.
	temperature := 0.7
	for _, tc := range publicClients(t) {
		for _, entry := range catalog.Entries(tc.provider) {
			if !slices.Contains(entry.APIs, tc.client.API()) {
				continue
			}
			_, err := tc.client.Prepare(context.Background(), llm.Input{
				Model:    entry.Model,
				Stream:   true,
				Settings: llm.Settings{Temperature: &temperature},
				Messages: []llm.Message{
					llm.UserMessage(llm.Text("hello")),
				},
			})
			if err != nil {
				t.Fatalf("%s at %s: %v", entry.Model, tc.client.API(), err)
			}
		}
	}
}

func TestCatalogLimitsAdjustRequestsWithWarnings(t *testing.T) {
	p := newMessages(t, messages.Config{APIKey: "k"})
	temperature := 0.5
	prep, err := p.Prepare(context.Background(), llm.Input{
		Model: "claude-opus-4-7",
		Settings: llm.Settings{
			MaxTokens:   500_000,
			Temperature: &temperature,
		},
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
	})
	if err != nil {
		t.Fatalf("default policy rejected fittable settings: %v", err)
	}
	effective := prep.Effective()
	if effective.Temperature != nil {
		t.Fatal("temperature was sent to a model documented to reject it")
	}
	if effective.MaxTokens != 128_000 {
		t.Fatalf("max tokens = %d, want 128000", effective.MaxTokens)
	}
	fields := map[llm.Setting]bool{}
	for _, w := range prep.Warnings() {
		fields[w.Field] = true
	}
	if !fields[llm.SettingTemperature] || !fields[llm.SettingMaxTokens] {
		t.Fatalf("warnings = %+v, want both fields", prep.Warnings())
	}
}

func TestCatalogLimitsFailUnderStrictPolicy(t *testing.T) {
	p := newMessages(t, messages.Config{
		APIKey:           "k",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	_, err := p.Prepare(context.Background(), llm.Input{
		Model:    "claude-opus-4-7",
		Settings: llm.Settings{MaxTokens: 500_000},
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
	})
	if err == nil {
		t.Fatal("strict policy accepted max tokens above the documented limit")
	}
}

func TestConfiguredModelInfoOverridesCatalog(t *testing.T) {
	const endpoint = "https://api.anthropic.com/v1/messages"
	info := llm.ModelInfo{
		Capabilities: llm.Capabilities{MaxOutputTokens: 1000},
		Evidence: llm.ModelEvidence{
			Model:       "claude-sonnet-5",
			API:         messages.API,
			Endpoint:    endpoint,
			Owner:       "application",
			Source:      "fixture",
			Method:      llm.EvidenceFixture,
			QualifiedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
		},
	}
	p := newMessages(t, messages.Config{
		APIKey: "k",
		Models: map[string]llm.ModelInfo{"claude-sonnet-5": info},
	})
	got, ok := p.ModelInfo("claude-sonnet-5")
	if !ok || got.Evidence.Owner != "application" {
		t.Fatalf("configured knowledge did not win: %+v", got.Evidence)
	}
	if got.Capabilities.MaxOutputTokens != 1000 {
		t.Fatal("configured limits were merged with catalog limits")
	}
}

func TestUndocumentedDestinationsGetNoCatalog(t *testing.T) {
	custom := newMessages(t, messages.Config{
		Provider: "proxy",
		APIKey:   "k",
		Endpoint: "https://proxy.example.test/v1/messages",
	})
	local, err := ollama.New(ollama.Config{})
	if err != nil {
		t.Fatal(err)
	}
	router, err := openrouter.New(openrouter.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	vertex, err := gemini.New(gemini.Config{
		Vertex:          true,
		Project:         "p",
		Location:        "us-central1",
		CredentialScope: "service-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		client llm.Client
		model  string
	}{
		{name: "custom endpoint", client: custom, model: "claude-sonnet-5"},
		{name: "ollama", client: local, model: "claude-sonnet-5"},
		{name: "openrouter", client: router, model: "gpt-6-luna"},
		{name: "vertex", client: vertex, model: "gemini-3.8-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.client.ModelInfo(tc.model); ok {
				t.Fatal("catalog facts applied to an undocumented destination")
			}
		})
	}
}

func TestCatalogEntryBindsToCustomEndpoint(t *testing.T) {
	const endpoint = "https://proxy.example.test/v1/messages"
	entry, ok := catalog.Find("anthropic", "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 is missing")
	}
	info, err := entry.Bind(messages.API, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	p := newMessages(t, messages.Config{
		Provider: "proxy",
		APIKey:   "k",
		Endpoint: endpoint,
		Models:   map[string]llm.ModelInfo{"claude-sonnet-5": info},
	})
	if _, ok := p.ModelInfo("claude-sonnet-5"); !ok {
		t.Fatal("bound catalog entry was not accepted as configuration")
	}
}
