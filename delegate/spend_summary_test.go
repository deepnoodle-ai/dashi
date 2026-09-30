package delegate

import (
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/money"
)

func TestFormatSpendShowsIncompleteUsageWithoutUnpricedMoney(t *testing.T) {
	spend := dashi.Spend{
		UsageIncomplete: true,
		Cost:            map[string]money.Amount{"USD": money.MustParseAmount("0.5")},
	}
	if got := formatSpend(spend); got != "0.5 USD; usage incomplete" {
		t.Fatalf("summary changed exact money or hid failed usage: %q", got)
	}
}
