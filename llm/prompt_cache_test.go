package llm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/money"
)

func TestPromptCacheJSONUsesDurationStrings(t *testing.T) {
	s := Settings{PromptCache: PromptCache{TTL: time.Hour, Key: "s-1"}}
	raw, err := json.Marshal(s)
	if err != nil || string(raw) != `{"prompt_cache":{"ttl":"1h","key":"s-1"}}` {
		t.Fatalf("settings JSON = %s, %v", raw, err)
	}
	var back Settings
	if err := json.Unmarshal(raw, &back); err != nil || back != s {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if raw, _ := json.Marshal(Settings{}); string(raw) != `{}` {
		t.Fatalf("zero prompt cache encoded: %s", raw)
	}
	for d, want := range map[time.Duration]string{
		5 * time.Minute:  "5m",
		time.Hour:        "1h",
		90 * time.Minute: "1h30m",
		24 * time.Hour:   "24h",
		30 * time.Second: "30s",
	} {
		if got := formatDuration(d); got != want {
			t.Fatalf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	var ttls Durations
	if err := json.Unmarshal([]byte(`["5m","1h"]`), &ttls); err != nil ||
		len(ttls) != 2 || ttls[1] != time.Hour {
		t.Fatalf("durations = %v, %v", ttls, err)
	}
	if raw, _ := json.Marshal(ttls); string(raw) != `["5m","1h"]` {
		t.Fatalf("durations JSON = %s", raw)
	}
}

func TestPromptCacheValidation(t *testing.T) {
	in := Input{Model: "m"}
	in.Settings.PromptCache = PromptCache{Disable: true, TTL: time.Hour}
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "disable and ttl") {
		t.Fatalf("disable with ttl: %v", err)
	}
	in.Settings.PromptCache = PromptCache{TTL: -time.Second}
	if in.Validate() == nil {
		t.Fatal("negative ttl accepted")
	}
	in.Settings.PromptCache = PromptCache{Disable: true, Key: "k"}
	if err := in.Validate(); err != nil {
		t.Fatalf("disable with a key: %v", err)
	}
}

func TestFitPromptCacheLifetime(t *testing.T) {
	offered := Capabilities{
		CacheTTLs: Durations{5 * time.Minute, time.Hour},
	}
	rejected := Capabilities{
		Features: map[Feature]Support{FeaturePromptCache: SupportUnsupported},
	}
	for _, tc := range []struct {
		name   string
		caps   Capabilities
		ttl    time.Duration
		want   time.Duration
		action AdjustmentAction
	}{
		{"met", offered, 30 * time.Minute, 30 * time.Minute, ""},
		{"clamped", offered, 2 * time.Hour, time.Hour, AdjustmentClamp},
		{"omitted", rejected, time.Hour, 0, AdjustmentOmit},
		{"unknown is left alone", Capabilities{}, 2 * time.Hour, 2 * time.Hour, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Model: "m"}
			in.Settings.PromptCache = PromptCache{TTL: tc.ttl, Key: "k"}
			fitted, adjustments, err := tc.caps.Fit(in, CapabilityAdjust, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := fitted.Settings.PromptCache; got.TTL != tc.want || got.Key != "k" {
				t.Fatalf("fitted = %+v", got)
			}
			var got []Adjustment
			for _, a := range adjustments {
				if a.Field == SettingPromptCacheTTL {
					got = append(got, a)
				}
			}
			if (len(got) == 1) != (tc.action != "") ||
				(len(got) == 1 && got[0].Action != tc.action) {
				t.Fatalf("adjustments = %+v", adjustments)
			}
			// The decision must satisfy the recorded settings, key and all.
			if err := validateAdjustments(in.Settings, fitted.Settings, adjustments); err != nil {
				t.Fatal(err)
			}
			if tc.action != "" {
				if _, _, err := tc.caps.Fit(in, CapabilityStrict, nil); err == nil {
					t.Fatal("strict accepted a changed lifetime")
				}
			}
		})
	}
}

func TestIntersectCacheLifetimes(t *testing.T) {
	a := Capabilities{CacheTTLs: Durations{5 * time.Minute, time.Hour}, CacheKeyDefault: true}
	if got := a.Intersect(Capabilities{}); len(got.CacheTTLs) != 2 || !got.CacheKeyDefault {
		t.Fatalf("unknown side constrained lifetimes: %+v", got)
	}
	got := a.Intersect(Capabilities{CacheTTLs: Durations{time.Hour}})
	if len(got.CacheTTLs) != 1 || got.CacheTTLs[0] != time.Hour {
		t.Fatalf("intersection = %v", got.CacheTTLs)
	}
	got = a.Intersect(Capabilities{CacheTTLs: Durations{30 * time.Minute}})
	if got.Features[FeaturePromptCache] != SupportUnsupported {
		t.Fatalf("disjoint lifetimes = %+v", got)
	}
	bad := ModelInfo{
		Capabilities: Capabilities{CacheTTLs: Durations{time.Hour, time.Minute}},
		Evidence:     cacheRiskRequest(t, 0, Durations{time.Minute}).Snapshot().ModelInfo.Evidence,
	}
	if err := bad.Validate(); err == nil ||
		!strings.Contains(err.Error(), "cache ttls") {
		t.Fatalf("unordered lifetimes: %v", err)
	}
}

func cacheRiskRequest(t *testing.T, ttl time.Duration, offered Durations) Request {
	t.Helper()
	input := Input{Model: "fixture", Settings: Settings{MaxTokens: 100}}
	effective := input.Settings
	effective.PromptCache.TTL = ttl
	var info *ModelInfo
	if offered != nil {
		info = &ModelInfo{
			Capabilities: Capabilities{CacheTTLs: offered},
			Evidence: ModelEvidence{
				Model:       "fixture",
				API:         "test.chat",
				Endpoint:    "https://example.test",
				Owner:       "tests",
				Source:      "fixture",
				Method:      EvidenceFixture,
				QualifiedAt: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
			},
		}
	}
	var adjustments []Adjustment
	if ttl > 0 {
		adjustments = []Adjustment{{
			Field:     SettingPromptCacheTTL,
			Action:    AdjustmentDefault,
			Requested: json.RawMessage("null"),
			Effective: json.RawMessage(`"` + formatDuration(ttl) + `"`),
			Reason:    "fixture",
		}}
	}
	request, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    "fixture",
			Scope:    "fixture",
		},
		Input:            input,
		Effective:        effective,
		Body:             []byte(`{"ok":true}`),
		Adjustments:      adjustments,
		ModelInfo:        info,
		CapabilityPolicy: CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestMaxCostAtRiskUsesOneHourWriteRate(t *testing.T) {
	p := pricedSheet()
	p.Base = Rates{
		Input:      amt("1"),
		Output:     amt("2"),
		CacheRead:  amt("0.1"),
		CacheWrite: amt("1.25"),
	}
	perToken := func(request Request, rate string) money.Amount {
		return money.AmountFromNanos(
			int64(EstimatedInputTokens(request)) * amt(rate).Nanos() / 1_000_000,
		)
	}

	// The default lifetime needs no one-hour rate.
	five := cacheRiskRequest(t, 5*time.Minute, nil)
	if cost, ok := WorstCaseCost(five, p); !ok || cost.Input != perToken(five, "1.25") {
		t.Fatalf("5m bound = %+v, %v", cost, ok)
	}

	// A one-hour lifetime without the rate has no bound.
	hour := cacheRiskRequest(t, time.Hour, nil)
	if _, ok := WorstCaseCost(hour, p); ok {
		t.Fatal("one-hour writes were bounded without a one-hour rate")
	}
	rate := amt("2")
	p.Base.CacheWrite1h = &rate
	cost, ok := WorstCaseCost(hour, p)
	if !ok || cost.Input != perToken(hour, "2") {
		t.Fatalf("1h bound = %+v, %v", cost, ok)
	}

	// A route whose lifetimes show it sends another one does not bill
	// one-hour writes, as OpenAI's 24 hours.
	p.Base.CacheWrite1h = nil
	day := cacheRiskRequest(t, time.Hour, Durations{5 * time.Minute, 24 * time.Hour})
	if _, ok := WorstCaseCost(day, p); !ok {
		t.Fatal("a 24h route needed a one-hour rate")
	}
}
