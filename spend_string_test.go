package dashi

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

func TestSpendString(t *testing.T) {
	tests := []struct {
		name  string
		spend Spend
		want  string
	}{
		{
			name: "zero value",
			want: "no recorded cost",
		},
		{
			name: "empty maps",
			spend: Spend{
				Cost:     map[string]money.Amount{},
				ToolCost: map[string]money.Amount{},
			},
			want: "no recorded cost",
		},
		{
			name:  "tokens without cost",
			spend: Spend{Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}, Estimated: 1},
			want:  "no recorded cost",
		},
		{
			name:  "explicit zero",
			spend: Spend{Cost: map[string]money.Amount{"USD": 0}},
			want:  "model: 0 USD",
		},
		{
			name:  "nano precision",
			spend: Spend{Cost: map[string]money.Amount{"USD": 1}},
			want:  "model: 0.000000001 USD",
		},
		{
			name:  "tool only",
			spend: Spend{ToolCost: map[string]money.Amount{"EUR": amount("2.5")}},
			want:  "tools: 2.5 EUR",
		},
		{
			name: "sorted separate currencies",
			spend: Spend{
				Cost: map[string]money.Amount{
					"USD": amount("1"), "EUR": amount("0.5"),
				},
				ToolCost: map[string]money.Amount{
					"USD": amount("2"), "CREDITS": amount("3"),
				},
			},
			want: "model: 0.5 EUR, 1 USD; tools: 3 CREDITS, 2 USD",
		},
		{
			name: "signed bounds",
			spend: Spend{Cost: map[string]money.Amount{
				"MAX": money.Amount(math.MaxInt64), "MIN": money.Amount(math.MinInt64),
			}},
			want: "model: 9223372036.854775807 MAX, -9223372036.854775808 MIN",
		},
		{
			name:  "unknown only",
			spend: Spend{CostUnknown: 1},
			want:  "no recorded cost; unknown: 1",
		},
		{
			name:  "partial only",
			spend: Spend{CostPartial: 1},
			want:  "no recorded cost; partial: 1",
		},
		{
			name:  "overlapping counters without amounts",
			spend: Spend{CostPartial: 2, CostUnknown: 3},
			want:  "no recorded cost; partial: 2; unknown: 3",
		},
		{
			name: "partial model with complete tools",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostPartial: 1,
			},
			want: "model: at least 0.5 USD; tools: 2 USD; partial: 1",
		},
		{
			name: "unknown qualifies both categories",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostUnknown: 1,
			},
			want: "model: at least 0.5 USD; tools: at least 2 USD; unknown: 1",
		},
		{
			name: "overlapping counters keep separate labels",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				CostPartial: 1,
				CostUnknown: 1,
			},
			want: "model: at least 0.5 USD; partial: 1; unknown: 1",
		},
		{
			name: "unknown tool zero remains explicit",
			spend: Spend{
				ToolCost:    map[string]money.Amount{"USD": 0},
				CostUnknown: 1,
			},
			want: "tools: at least 0 USD; unknown: 1",
		},
		{
			name: "partial without model amounts does not qualify tools",
			spend: Spend{
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostPartial: 1,
			},
			want: "tools: 2 USD; partial: 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spend.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpendStringToolOverflow(t *testing.T) {
	most := money.Amount(math.MaxInt64)
	spend := Spend{
		Cost:     map[string]money.Amount{"USD": amount("0.5")},
		ToolCost: map[string]money.Amount{"USD": most},
	}.Add(Spend{ToolCost: map[string]money.Amount{"USD": 1}})
	if spend.CostUnknown != 1 || spend.ToolCost["USD"] != most {
		t.Fatal("overflow must retain the known tool amount and mark cost unknown")
	}
	want := "model: at least 0.5 USD; tools: at least 9223372036.854775807 USD; unknown: 1"
	if got := spend.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSpendStringStableAndPreservesFields(t *testing.T) {
	spend := Spend{
		Usage: llm.Usage{
			InputTokens: 10, OutputTokens: 2,
			Requests: map[string]int{llm.MeterCodeExecution: 1},
		},
		Cost:             map[string]money.Amount{"USD": amount("1"), "EUR": amount("0.5")},
		ToolCost:         map[string]money.Amount{"USD": amount("2"), "EUR": amount("3")},
		CostPartial:      1,
		CostUnknown:      2,
		Estimated:        3,
		UnpricedRequests: map[string]int{llm.MeterCodeExecution: 1},
	}
	before := spend.Clone()
	want := "model: at least 0.5 EUR, 1 USD; tools: at least 3 EUR, 2 USD; partial: 1; unknown: 2"
	for range 100 {
		if got := spend.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
	if !reflect.DeepEqual(spend, before) {
		t.Fatal("formatting modified Spend fields")
	}
	// Both values and pointers support the standard fmt display verbs.
	for _, value := range []fmt.Stringer{spend, &spend} {
		for _, verb := range []string{"%s", "%v"} {
			if got := fmt.Sprintf(verb, value); got != want {
				t.Fatalf("fmt.Sprintf(%q) = %q, want %q", verb, got, want)
			}
		}
	}
}
