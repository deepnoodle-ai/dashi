package dashi_test

import (
	"fmt"

	"github.com/deepnoodle-ai/dashi"
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
