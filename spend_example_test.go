package dashi_test

import (
	"fmt"
	"math"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

func ExampleSpend_String() {
	spend := dashi.Spend{
		Cost: map[string]money.Amount{
			"USD": money.MustParseAmount("0.000000001"),
			"EUR": money.MustParseAmount("0.5"),
		},
		ToolCost:    map[string]money.Amount{"USD": money.MustParseAmount("2")},
		CostPartial: 1,
	}
	fmt.Println(spend)
	fmt.Println(dashi.Spend{CostUnknown: 1})
	// Output:
	// model: at least 0.5 EUR, 0.000000001 USD; tools: 2 USD; partial: 1
	// no recorded cost; unknown: 1
}

func ExampleSpend_Add() {
	first := dashi.Spend{
		Usage:    llm.Usage{InputTokens: math.MaxInt},
		Cost:     map[string]money.Amount{"USD": money.MustParseAmount("0.5")},
		ToolCost: map[string]money.Amount{"USD": money.MustParseAmount("2")},
	}
	second := dashi.Spend{
		Usage:    llm.Usage{InputTokens: 1},
		Cost:     map[string]money.Amount{"USD": money.MustParseAmount("0.25")},
		ToolCost: map[string]money.Amount{"USD": money.MustParseAmount("3")},
	}
	sum := first.Add(second)
	fmt.Println(sum)
	fmt.Println("usage incomplete:", sum.UsageIncomplete,
		"unknown cost:", sum.CostUnknown)
	// Output:
	// model: 0.75 USD; tools: 5 USD; usage incomplete
	// usage incomplete: true unknown cost: 0
}
