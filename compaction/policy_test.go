package compaction

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestPolicyDueCountsCachedInput(t *testing.T) {
	p := policyWith(10_000)
	warm := llm.Usage{
		InputTokens:          100,
		CacheReadInputTokens: 1_800,
		OutputTokens:         100,
	}
	if !p.Due(warm) {
		t.Fatal("warm-cache usage at 20% was not due")
	}
	if p.Due(llm.Usage{InputTokens: 100, OutputTokens: 100}) {
		t.Fatal("small usage was due")
	}
	// InputTokens alone would hide the cache read.
	if warm.InputTokens+warm.OutputTokens >= 2_000 {
		t.Fatal("test usage does not exercise the cache")
	}
}

func TestPolicyDueTable(t *testing.T) {
	cases := []struct {
		name     string
		fraction float64
		due      int
		window   int
		used     int
		want     bool
	}{
		{"default below", 0, 0, 10_000, 1_999, false},
		{"default at", 0, 0, 10_000, 2_000, true},
		{"custom", 0.5, 0, 10_000, 5_000, true},
		{"negative disables", -1, 0, 10_000, 10_000, false},
		{"above one counts as one", 3, 0, 10_000, 9_999, false},
		{"above one at window", 3, 0, 10_000, 10_000, true},
		{"nothing used", 0, 0, 10_000, 0, false},
		{"due tokens below", 0, 3_000, 10_000, 2_999, false},
		{"due tokens at", 0, 3_000, 10_000, 3_000, true},
		{"due tokens override fraction", 0.9, 3_000, 10_000, 3_000, true},
		{"negative fraction disables due tokens", -1, 3_000, 10_000, 9_000, false},
		{"negative due tokens are unset", 0, -5, 10_000, 2_000, true},
		{"due tokens capped at a known window", 0, 50_000, 10_000, 10_000, true},
		{"due tokens not capped at an unknown window", 0, 300_000, 0, 250_000, false},
		{"due tokens at an unknown window", 0, 300_000, 0, 300_000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := policyWith(c.window)
			p.Fraction, p.DueTokens = c.fraction, c.due
			got := p.Due(llm.Usage{InputTokens: c.used})
			if got != c.want {
				t.Fatalf("Due = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPolicyZeroValueUsesDefaultWindow(t *testing.T) {
	var p Policy
	window, known := p.Window()
	if window != DefaultWindow || known {
		t.Fatalf("window=%d known=%v", window, known)
	}
	if window != 200_000 || DefaultFraction != 0.2 {
		t.Fatalf("defaults: window %d, fraction %v", window, DefaultFraction)
	}
	if p.Due(llm.Usage{InputTokens: 39_999}) || !p.Due(llm.Usage{InputTokens: 40_000}) {
		t.Fatal("20% of the default window was not the due count")
	}
}

func TestPolicyDueSaturatesHugeUsage(t *testing.T) {
	huge := int(^uint(0) >> 1)
	if !(Policy{}).Due(llm.Usage{InputTokens: huge, OutputTokens: 10}) {
		t.Fatal("overflowing usage was not due")
	}
}
