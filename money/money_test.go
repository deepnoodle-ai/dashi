package money

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParseAmountExactText(t *testing.T) {
	for _, tc := range []struct {
		text  string
		nanos int64
		canon string
	}{
		{"0", 0, "0"},
		{"-0", 0, "0"},
		{"0.000", 0, "0"},
		{"12", 12_000_000_000, "12"},
		{"12.50", 12_500_000_000, "12.5"},
		{"0.075", 75_000_000, "0.075"},
		{"0.000000003", 3, "0.000000003"},
		{"-0.000000003", -3, "-0.000000003"},
		{"-3", -3_000_000_000, "-3"},
		{"1.123456789", 1_123_456_789, "1.123456789"},
		{"0.1000000000", 100_000_000, "0.1"},
		{"007.5", 7_500_000_000, "7.5"},
		{
			"9223372036.854775807",
			math.MaxInt64,
			"9223372036.854775807",
		},
		{
			"-9223372036.854775808",
			math.MinInt64,
			"-9223372036.854775808",
		},
	} {
		got, err := ParseAmount(tc.text)
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", tc.text, err)
		}
		if got.Nanos() != tc.nanos {
			t.Fatalf(
				"ParseAmount(%q) = %d nanos, want %d",
				tc.text,
				got,
				tc.nanos,
			)
		}
		if got.String() != tc.canon {
			t.Fatalf(
				"String(%q) = %q, want %q",
				tc.text,
				got,
				tc.canon,
			)
		}
		back, err := ParseAmount(got.String())
		if err != nil || back != got {
			t.Fatalf(
				"canonical %q did not parse back: %v, %v",
				got,
				back,
				err,
			)
		}
	}
}

func TestParseAmountRejectsInexactOrInvalidText(t *testing.T) {
	for _, text := range []string{
		"",
		"-",
		".5",
		"5.",
		"+1",
		"1e-7",
		"0x10",
		" 1",
		"1 ",
		"1,000",
		"NaN",
		"Inf",
		"0.0000000001",
		"1.1234567891",
		"9223372036.854775808",
		"-9223372036.854775809",
		"100000000000",
	} {
		if got, err := ParseAmount(text); err == nil {
			t.Fatalf("ParseAmount(%q) = %s, want error", text, got)
		}
	}
}

func TestMustParseAmountPanicsOnInexactText(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("inexact constant did not panic")
		}
	}()
	MustParseAmount("0.0000000001")
}

func TestAmountAddDetectsOverflow(t *testing.T) {
	most := AmountFromNanos(math.MaxInt64)
	if _, ok := most.Add(1); ok {
		t.Fatal("positive overflow wrapped")
	}
	if _, ok := AmountFromNanos(math.MinInt64).Add(-1); ok {
		t.Fatal("negative overflow wrapped")
	}
	sum, ok := MustParseAmount("0.1").Add(MustParseAmount("0.2"))
	if !ok || sum != MustParseAmount("0.3") {
		t.Fatalf("0.1+0.2 = %s, want exactly 0.3", sum)
	}
}

func TestAmountFloat64IsDisplayApproximation(t *testing.T) {
	if got := MustParseAmount("0.5").Float64(); got != 0.5 {
		t.Fatalf("Float64 = %g", got)
	}
}

func TestAmountJSONRoundTripsAsDecimalString(t *testing.T) {
	type record struct {
		Amount Amount `json:"amount"`
	}
	for _, text := range []string{
		"0",
		"0.5",
		"-12.25",
		"0.000000001",
		"9223372036.854775807",
	} {
		in := record{Amount: MustParseAmount(text)}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"amount":"`+text+`"}` {
			t.Fatalf("encoded %s", raw)
		}
		var out record
		if err := json.Unmarshal(raw, &out); err != nil || out != in {
			t.Fatalf("round trip of %s = %+v, %v", text, out, err)
		}
	}
}

// TestAmountHasOneSpelling checks that every amount encodes in one trimmed
// form, so canonical JSON digests cannot see two spellings of one value.
func TestAmountHasOneSpelling(t *testing.T) {
	values := []int64{
		0,
		1,
		-1,
		10,
		100_000_000,
		500_000_000,
		1_000_000_000,
		1_000_000_001,
		math.MaxInt64,
		math.MinInt64,
	}
	for n := int64(7); n < math.MaxInt64/13; n = n*13 + 5 {
		values = append(values, n, -n)
	}
	for _, n := range values {
		a := AmountFromNanos(n)
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		text := a.String()
		if string(raw) != `"`+text+`"` {
			t.Fatalf("%d encoded %s, String %q", n, raw, text)
		}
		whole, frac, hasFrac := strings.Cut(
			strings.TrimPrefix(
				text,
				"-",
			),
			".",
		)
		if (len(whole) > 1 && whole[0] == '0') ||
			hasFrac && (frac == "" || strings.HasSuffix(
				frac,
				"0",
			)) ||
			text == "-0" {
			t.Fatalf("%d has a non-canonical spelling %q", n, text)
		}
		var back Amount
		if err := json.Unmarshal(raw, &back); err != nil || back != a {
			t.Fatalf("%d did not round trip: %s, %v", n, back, err)
		}
	}
	for _, spelling := range []string{
		`"0.50"`,
		`"0.500000000"`,
		`0.5`,
		`5e-1`,
	} {
		var a Amount
		if err := json.Unmarshal([]byte(spelling), &a); err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(a); string(raw) != `"0.5"` {
			t.Fatalf("%s re-encoded as %s", spelling, raw)
		}
	}
}

func TestAmountJSONReadsBareNumbersExactly(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		nanos int64
	}{
		{`0.5`, 500_000_000},
		{`12`, 12_000_000_000},
		{`-0.25`, -250_000_000},
		{`0`, 0},
		{`-0`, 0},
		{`1e-7`, 100},
		{`1E-7`, 100},
		{`2.5e+2`, 250_000_000_000},
		{`1.5e3`, 1_500_000_000_000},
		// A float sum rounds to the nano.
		{`0.30000000000000004`, 300_000_000},
		{`0.00014000000000000001`, 140_000},
		// Ties round to even.
		{`0.0000000005`, 0},
		{`0.0000000015`, 2},
		{`0.0000000025`, 2},
		{`-0.0000000025`, -2},
		{`0.00000000250001`, 3},
		{`1e-10`, 0},
		{`1e-400`, 0},
		{`9.2233720368547758074e9`, math.MaxInt64},
	} {
		var got Amount
		if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
			t.Fatalf("decode %s: %v", tc.raw, err)
		}
		if got.Nanos() != tc.nanos {
			t.Fatalf(
				"decode %s = %d nanos, want %d",
				tc.raw,
				got,
				tc.nanos,
			)
		}
	}
}

func TestAmountJSONDecodesEscapedStrings(t *testing.T) {
	backslash := string(rune(0x5c))
	// "0.5" is the JSON spelling of the text 0.5.
	var got Amount
	escaped := `"` + backslash + `u0030.5"`
	if err := json.Unmarshal([]byte(escaped), &got); err != nil ||
		got != MustParseAmount("0.5") {
		t.Fatalf("decode %s = %s, %v", escaped, got, err)
	}
	// "A" is the text A, which is not a decimal amount.
	for _, raw := range []string{
		`"` + backslash + `u0041"`,
		`"` + backslash + `x30"`,
		`"0.5`,
	} {
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Fatalf("decode %s = %s, want error", raw, got)
		}
	}
}

func TestAmountJSONRejectsInvalidInput(t *testing.T) {
	for _, raw := range []string{
		`"1e-7"`,
		`"0.0000000001"`,
		`"abc"`,
		`" 1"`,
		`true`,
		`01`,
		`1e400`,
		`9223372037`,
		`1e`,
		`.5`,
		`[1]`,
	} {
		var got Amount
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Fatalf("decode %s = %s, want error", raw, got)
		}
	}
	amount := MustParseAmount("3")
	if err := json.Unmarshal([]byte(`null`), &amount); err != nil ||
		amount != MustParseAmount("3") {
		t.Fatalf("null changed the amount: %s, %v", amount, err)
	}
}
