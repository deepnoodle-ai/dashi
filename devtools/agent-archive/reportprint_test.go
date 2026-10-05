package main

import (
	"bytes"
	"testing"

	"github.com/deepnoodle-ai/dashi/money"
)

func TestCompareCost(t *testing.T) {
	exact := amount{Low: amt("10"), High: amt("10"), Rows: 1}
	// A range, from calls not in session files that write cache.
	span := amount{Low: amt("10"), High: amt("12"), Rows: 1}
	for _, c := range []struct {
		name      string
		dashi     amount
		app       string
		agreement string
		diff      string
	}{
		{"same", exact, "10", agreementAgrees, "0"},
		{"within 1%", exact, "10.09", agreementAgrees, "0.09"},
		{"over 1% higher", exact, "10.11", agreementHigher, "0.11"},
		{"over 1% lower", exact, "9.89", agreementLower, "-0.11"},
		{"cent floor", amount{Low: amt("0.5"), High: amt("0.5"), Rows: 1}, "0.51", agreementAgrees, "0.01"},
		{"above the cent floor", amount{Low: amt("0.5"), High: amt("0.5"), Rows: 1}, "0.52", agreementHigher, "0.02"},
		{"inside the range", span, "11", agreementAgrees, "0"},
		{"just below the range", span, "9.95", agreementAgrees, "-0.05"},
		{"below the range", span, "9", agreementLower, "-1"},
		{"above the range", span, "12.5", agreementHigher, "0.5"},
		{"lower bound", amount{Low: amt("10"), High: amt("10"), LowerBound: true, Rows: 2}, "50", agreementNotCompared, "0"},
	} {
		got, diff := compareCost(c.dashi, amt(c.app))
		if got != c.agreement || diff != amt(c.diff) {
			t.Errorf("%s: %s %s, want %s %s", c.name, got, diff, c.agreement, c.diff)
		}
	}
}

func TestFormatMoney(t *testing.T) {
	for in, want := range map[string]string{
		"0":          "$0.00",
		"0.004":      "$0.00",
		"0.005":      "$0.01",
		"1234567.89": "$1,234,567.89",
		"-12.345":    "-$12.35",
		"-0.003":     "$0.00",
		"999.999":    "$1,000.00",
	} {
		if got := formatMoney(money.MustParseAmount(in)); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestWriteTable(t *testing.T) {
	var b bytes.Buffer
	writeTable(&b, []string{"Model", "Total"}, [][]string{{"a", "$1.00"}, {"longer", "$10.00+"}}, 1)
	want := "Model     Total\n" +
		"a         $1.00\n" +
		"longer  $10.00+\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
