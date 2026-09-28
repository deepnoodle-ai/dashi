package tool

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestReportWithoutReporterIsNoOp(t *testing.T) {
	Report(t.Context(), Progress{Message: "ignored"})
	Report(WithReporter(t.Context(), nil), Progress{Message: "silenced"})
}

func TestReportCopiesData(t *testing.T) {
	var got Progress
	ctx := WithReporter(t.Context(), func(p Progress) {
		got = p
	})
	data := json.RawMessage(`{"n":1}`)
	Report(ctx, Progress{Message: "one", Done: 1, Total: 2, Data: data})
	data[5] = '9'
	if got.Message != "one" || got.Done != 1 || got.Total != 2 ||
		string(got.Data) != `{"n":1}` {
		t.Fatalf("got %+v data=%s", got, got.Data)
	}
}

func TestReportAdjustsInvalidValues(t *testing.T) {
	var got Progress
	ctx := WithReporter(t.Context(), func(p Progress) {
		got = p
	})
	cases := []struct {
		name string
		in   Progress
	}{
		{"invalid JSON", Progress{Data: json.RawMessage(`{`)}},
		{
			"oversized",
			Progress{Data: json.RawMessage(
				`"` + strings.Repeat("x", maxProgressData) + `"`,
			)},
		},
		{"NaN", Progress{Done: math.NaN(), Total: math.Inf(1)}},
	}
	for _, c := range cases {
		Report(ctx, c.in)
		if got.Data != nil || got.Done != 0 || got.Total != 0 {
			t.Fatalf("%s: got %+v", c.name, got)
		}
	}
}

func TestWithReporterShadowsOuterReporter(t *testing.T) {
	outer := 0
	ctx := WithReporter(t.Context(), func(Progress) {
		outer++
	})
	inner := 0
	Report(WithReporter(ctx, func(Progress) {
		inner++
	}), Progress{})
	Report(WithReporter(ctx, nil), Progress{})
	if outer != 0 || inner != 1 {
		t.Fatalf("outer=%d inner=%d", outer, inner)
	}
}
