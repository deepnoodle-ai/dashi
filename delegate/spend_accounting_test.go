package delegate_test

import (
	"context"
	"math"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

func TestSpendReportPreservesIncompleteChildUsage(t *testing.T) {
	child := func(context.Context, llm.Input) (llm.Result, error) {
		r := costed(text("done"), "0.25")
		r.Usage.Requests = map[string]int{llm.MeterUnreported: math.MaxInt}
		return r, nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	out, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	check := func(recorder dashi.Loader) {
		t.Helper()
		report, err := delegate.Report(t.Context(), recorder, out.TurnView)
		if err != nil {
			t.Fatal(err)
		}
		if report.Parent.UsageIncomplete || !report.Children.UsageIncomplete ||
			!report.Inclusive.UsageIncomplete {
			t.Fatal("child report aggregation lost usage incompleteness")
		}
		if report.Inclusive.CostUnknown != 0 ||
			report.Inclusive.Cost["USD"] != money.MustParseAmount("2.25") {
			t.Fatal("child usage failure changed independently exact money")
		}
	}
	check(h.rec)
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	check(h.rec)
	if !h.tasks.List()[0].Spend.UsageIncomplete {
		t.Fatal("reopened task summary lost incomplete usage")
	}
}
