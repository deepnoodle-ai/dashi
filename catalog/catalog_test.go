package catalog

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
)

func TestEmbeddedFilesParseAndValidate(t *testing.T) {
	c, err := parseAll()
	if err != nil {
		t.Fatalf("embedded catalog is invalid: %v", err)
	}
	want := []string{
		"anthropic",
		"google",
		"grok",
		"meta",
		"mistral",
		"openai",
	}
	if got := Providers(); !slices.Equal(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
	for _, p := range c {
		if len(p.entries) == 0 {
			t.Fatalf("%s has no entries", p.name)
		}
	}
}

func TestEveryEntryBindsToEachDocumentedAPI(t *testing.T) {
	for _, name := range Providers() {
		for _, d := range Destinations(name) {
			for _, e := range Entries(name) {
				if !slices.Contains(e.APIs, d.API) {
					continue
				}
				endpoint := d.resolve(e.Model)
				info, ok := Lookup(d.API, endpoint, e.Model)
				if !ok {
					t.Fatalf(
						"%s %s: no lookup at %s",
						name,
						e.Model,
						endpoint,
					)
				}
				if err := info.Validate(); err != nil {
					t.Fatalf(
						"%s %s: %v",
						name,
						e.Model,
						err,
					)
				}
			}
		}
	}
}

func TestEntriesDateEveryClaim(t *testing.T) {
	today := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	for _, name := range Providers() {
		for _, e := range Entries(name) {
			if e.QualifiedAt.After(today) {
				t.Fatalf(
					"%s %s is qualified in the future",
					name,
					e.Model,
				)
			}
			if e.Pricing == nil {
				continue
			}
			updated, _ := time.Parse(
				time.DateOnly,
				e.Pricing.UpdatedAt,
			)
			if updated.After(today) {
				t.Fatalf(
					"%s %s price is dated in the future",
					name,
					e.Model,
				)
			}
			if e.Pricing.Currency != "USD" ||
				e.Pricing.Reference == "" {
				t.Fatalf(
					"%s %s price lacks a reference",
					name,
					e.Model,
				)
			}
		}
	}
}

func TestEntriesNeverClaimLiveProbes(t *testing.T) {
	for _, name := range Providers() {
		d := Destinations(name)[0]
		for _, e := range Entries(name) {
			if !slices.Contains(e.APIs, d.API) {
				continue
			}
			info, err := e.Bind(d.API, d.resolve(e.Model))
			if err != nil {
				t.Fatal(err)
			}
			if info.Evidence.Method != llm.EvidenceDocumentation {
				t.Fatalf(
					"%s %s method = %q",
					name,
					e.Model,
					info.Evidence.Method,
				)
			}
			if info.Evidence.Owner != Owner {
				t.Fatalf(
					"%s %s owner = %q",
					name,
					e.Model,
					info.Evidence.Owner,
				)
			}
		}
	}
}

func TestDefaultModelIsCurrentEntry(t *testing.T) {
	for _, name := range Providers() {
		model, ok := Default(name)
		if !ok {
			t.Fatalf("%s has no default", name)
		}
		e, ok := Find(name, model)
		if !ok {
			t.Fatalf("%s default %q is not an entry", name, model)
		}
		if e.Lifecycle == LifecycleDeprecated {
			t.Fatalf("%s default %q is deprecated", name, model)
		}
	}
}

func TestAnthropicCacheWritesUseFiveMinuteRate(t *testing.T) {
	// Kernel's Messages client sends no cache_control, so every write it can
	// cause is a 5-minute write at 1.25x input. Adding TTL control must add a
	// 1-hour usage bucket and rate; see design/model-catalog.md.
	for _, e := range Entries("anthropic") {
		if e.Pricing == nil {
			t.Fatalf("%s has no price", e.Model)
		}
		base := e.Pricing.Base
		// Exact nanos make 1.25x an integer identity: 4w == 5i.
		if base.CacheWrite*4 != base.Input*5 {
			t.Fatalf(
				"%s cache write %s is not 1.25x input %s",
				e.Model,
				base.CacheWrite,
				base.Input,
			)
		}
		if len(e.Pricing.Tiers) > 0 {
			t.Fatalf(
				"%s has a tier the Messages client cannot reach",
				e.Model,
			)
		}
	}
}

func TestMissingCacheRatesAreStatedAtInputRate(t *testing.T) {
	// gpt-5.5-pro publishes no cached-input rate. Dive recorded zero, which
	// meant free; the catalog states the input rate instead.
	e, ok := Find("openai", "gpt-5.5-pro")
	if !ok {
		t.Fatal("gpt-5.5-pro is missing")
	}
	base := e.Pricing.Base
	if base.CacheRead != base.Input || base.CacheWrite != base.Input {
		t.Fatalf("pro cache rates = %+v, want the input rate", base)
	}
	tier := e.Pricing.Tiers[0].Rates
	if tier.CacheRead != tier.Input || tier.CacheWrite != tier.Input {
		t.Fatalf("pro long-context cache rates = %+v", tier)
	}
}

func TestLongContextTierReplacesEveryRate(t *testing.T) {
	e, ok := Find("google", "gemini-2.5-pro")
	if !ok {
		t.Fatal("gemini-2.5-pro is missing")
	}
	usage := &llm.Usage{InputTokens: 200_001, OutputTokens: 1_000_000}
	cost, ok := e.Pricing.Estimate(usage)
	if !ok {
		t.Fatal("estimate was unavailable")
	}
	// 200,001 input tokens at $2.50/M plus 1M output tokens at $15/M.
	want := money.MustParseAmount("15.5000025")
	if cost.Total != want {
		t.Fatalf("total = %s, want %s", cost.Total, want)
	}
}

func TestLookupIgnoresUndocumentedDestinations(t *testing.T) {
	cases := []struct {
		name, api, endpoint, model string
	}{
		{
			name:     "proxy endpoint",
			api:      "anthropic.messages",
			endpoint: "https://proxy.example.test/v1/messages",
			model:    "claude-sonnet-5",
		},
		{
			name:     "API the model lacks",
			api:      "openai.completions",
			endpoint: "https://api.openai.com/v1/chat/completions",
			model:    "gpt-6-astra",
		},
		{
			name:     "unknown model",
			api:      "anthropic.messages",
			endpoint: "https://api.anthropic.com/v1/messages",
			model:    "claude-opus-4-9",
		},
		{
			name:     "prefix of a known model",
			api:      "openai.responses",
			endpoint: "https://api.openai.com/v1/responses",
			model:    "gpt-5.7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := Lookup(
				tc.api,
				tc.endpoint,
				tc.model,
			); ok {
				t.Fatal("lookup matched an undocumented destination")
			}
		})
	}
}

func TestLookupResolvesModelInEndpointTemplate(t *testing.T) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" +
		"gemini-3.8-flash:generateContent"
	info, ok := Lookup(
		"google.generate_content",
		endpoint,
		"gemini-3.8-flash",
	)
	if !ok {
		t.Fatal("templated endpoint did not match")
	}
	if info.Evidence.Endpoint != endpoint {
		t.Fatalf("evidence endpoint = %q", info.Evidence.Endpoint)
	}
}

func TestBindScopesEntryToCallerEndpoint(t *testing.T) {
	e, ok := Find("anthropic", "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 is missing")
	}
	proxy := "https://proxy.example.test/v1/messages"
	info, err := e.Bind("anthropic.messages", proxy)
	if err != nil {
		t.Fatal(err)
	}
	if info.Evidence.Endpoint != proxy || info.Evidence.Model != e.Model {
		t.Fatalf("evidence = %+v", info.Evidence)
	}
	if _, err := e.Bind("openai.responses", proxy); err == nil {
		t.Fatal("bound an entry to an API that does not serve it")
	}
}

func TestEntriesAreIndependentCopies(t *testing.T) {
	e, _ := Find("anthropic", "claude-sonnet-5")
	e.APIs[0] = "changed"
	e.Capabilities.Features[llm.FeatureText] = llm.SupportUnsupported
	e.Capabilities.ReasoningEfforts[0] = "changed"
	e.Pricing.Base.Input = 0
	again, _ := Find("anthropic", "claude-sonnet-5")
	if again.APIs[0] != "anthropic.messages" {
		t.Fatal("APIs were shared with the catalog")
	}
	if again.Capabilities.Features[llm.FeatureText] != llm.SupportSupported {
		t.Fatal("features were shared with the catalog")
	}
	if again.Capabilities.ReasoningEfforts[0] == "changed" {
		t.Fatal("effort levels were shared with the catalog")
	}
	if again.Pricing.Base.Input == 0 {
		t.Fatal("pricing was shared with the catalog")
	}
}

func TestServesAcceptsDocumentedSnapshot(t *testing.T) {
	e, ok := Find("anthropic", "claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 is missing")
	}
	if !e.Serves("claude-haiku-4-5") {
		t.Fatal("entry does not serve its own ID")
	}
	if !e.Serves("claude-haiku-4-5-20251001") {
		t.Fatal("entry does not serve its documented snapshot")
	}
	if e.Serves("claude-haiku-4-5-20260101") {
		t.Fatal("entry served an undocumented snapshot")
	}
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	valid := `{"provider":"p","source":"https://example.test/models",` +
		`"default":"m","destinations":[{"api":"a",` +
		`"endpoint":"https://example.test/v1"}],"models":[%s]}`
	entry := `{"model":"m","apis":["a"],"qualified_at":"2026-09-27",` +
		`"capabilities":{%s}%s}`
	cases := []struct {
		name, data, want string
	}{
		{
			name: "unknown field",
			data: strings.Replace(
				fmt.Sprintf(valid, fmt.Sprintf(entry, "", "")),
				`"source"`,
				`"extra":1,"source"`,
				1,
			),
			want: "unknown field",
		},
		{
			name: "unknown feature",
			data: fmt.Sprintf(
				valid,
				fmt.Sprintf(
					entry,
					`"features":{"x":"supported"}`,
					"",
				),
			),
			want: "invalid capability",
		},
		{
			name: "free rate by omission",
			data: fmt.Sprintf(
				valid,
				fmt.Sprintf(
					entry,
					"",
					`,"pricing":{"currency":"USD","updated_at":"2026-09-27",`+
						`"base":{"input":1,"output":2,"cache_read":0.1}}`,
				),
			),
			want: "explicit cache_write",
		},
		{
			name: "undocumented API",
			data: fmt.Sprintf(
				valid,
				strings.Replace(
					fmt.Sprintf(
						entry,
						"",
						"",
					),
					`["a"]`,
					`["b"]`,
					1,
				),
			),
			want: "undocumented API",
		},
		{
			name: "missing default",
			data: strings.Replace(
				fmt.Sprintf(
					valid,
					fmt.Sprintf(
						entry,
						"",
						"",
					),
				),
				`"default":"m"`,
				`"default":"n"`,
				1,
			),
			want: "default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse([]byte(tc.data))
			if err == nil || !strings.Contains(
				err.Error(),
				tc.want,
			) {
				t.Fatalf(
					"error = %v, want %q",
					err,
					tc.want,
				)
			}
		})
	}
}
