package compaction

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestPolicyDueCountsCachedInput(t *testing.T) {
	p := policyWith(10_000)
	warm := llm.Usage{
		InputTokens:          100,
		CacheReadInputTokens: 7_800,
		OutputTokens:         100,
	}
	if !p.Due(warm) {
		t.Fatal("warm-cache usage at 80% was not due")
	}
	if p.Due(llm.Usage{InputTokens: 100, OutputTokens: 100}) {
		t.Fatal("small usage was due")
	}
	// InputTokens alone would hide the cache read.
	if warm.InputTokens+warm.OutputTokens >= 8_000 {
		t.Fatal("test usage does not exercise the cache")
	}
}

func TestPolicyDueTable(t *testing.T) {
	cases := []struct {
		name     string
		fraction float64
		used     int
		want     bool
	}{
		{"default below", 0, 7_999, false},
		{"default at", 0, 8_000, true},
		{"custom", 0.5, 5_000, true},
		{"negative disables", -1, 10_000, false},
		{"above one counts as one", 3, 9_999, false},
		{"above one at window", 3, 10_000, true},
		{"nothing used", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := policyWith(10_000)
			p.Fraction = c.fraction
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
	if !p.Due(llm.Usage{InputTokens: 103_000}) {
		t.Fatal("80% of the default window was not due")
	}
}

func TestPolicyDueSaturatesHugeUsage(t *testing.T) {
	huge := int(^uint(0) >> 1)
	if !(Policy{}).Due(llm.Usage{InputTokens: huge, OutputTokens: 10}) {
		t.Fatal("overflowing usage was not due")
	}
}
