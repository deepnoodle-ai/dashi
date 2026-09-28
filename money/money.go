// Package money holds Kernel's exact monetary amount. It depends only on the
// standard library so that every package that prices or reports charges,
// including tool, can share one representation without an import cycle.
//
// Amounts are an integer count of nano-units (1e-9) of a currency, as
// google.type.Money represents them. Binary floating point cannot hold most
// decimal prices exactly, so float sums drift and a cap comparison near its
// boundary can go the wrong way. Minor units (cents) are exact but too coarse
// for per-token prices such as $0.075 per million tokens. Nanos represent
// every published LLM price exactly, and int64 covers about ±9.2 billion
// currency units.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Amount is an exact amount of one currency in nano-units (1e-9). The currency
// is carried beside it, for example by kernel.Money or llm.Cost. The zero
// value is zero.
//
// JSON encodes an Amount as a canonical decimal string such as "0.5", which is
// exact, readable, safe in JavaScript, and maps to a Postgres numeric column.
// Decoding also accepts a bare JSON number; see UnmarshalJSON.
//
// Hosts that keep ledgers in coarser units convert with Nanos and choose their
// own rounding. Integer micro-units are Nanos()/1000; round holds and
// reservations up and settled charges half to even, as Kernel does for nanos.
type Amount int64

// Nanos per whole currency unit.
const unit = 1_000_000_000

// maxDigits is the number of decimal digits in math.MaxInt64.
const maxDigits = 19

var (
	errSyntax   = errors.New("invalid decimal amount")
	errPrecise  = errors.New("more than nine fractional digits")
	errOverflow = errors.New("amount out of range")
)

// ParseAmount reads exact decimal text such as "0.075", "-3", or "12.50". It
// rejects exponents, signs other than a leading minus, a value that needs more
// than nine significant fractional digits, and a value outside the int64 nano
// range. It never rounds.
func ParseAmount(s string) (Amount, error) {
	amount, err := parse(s, false)
	if err != nil {
		return 0, fmt.Errorf("money: parse %q: %w", s, err)
	}
	return amount, nil
}

// MustParseAmount is ParseAmount for constant text, such as a price or a cap
// in source code. It panics when the text is not an exact amount.
func MustParseAmount(s string) Amount {
	amount, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return amount
}

// AmountFromNanos returns the amount of n nano-units, such as a value read
// from an integer ledger column.
func AmountFromNanos(n int64) Amount {
	return Amount(n)
}

// Nanos returns the exact integer count of nano-units.
func (a Amount) Nanos() int64 {
	return int64(a)
}

// Add returns a+b, or false when the sum is outside the int64 nano range.
// Kernel never lets an amount wrap.
func (a Amount) Add(b Amount) (Amount, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// Float64 approximates the amount for display, charts, or logs. It is lossy;
// never use it for sums, comparisons, or storage.
func (a Amount) Float64() float64 {
	return float64(a) / unit
}

// String returns canonical decimal text without trailing fractional zeros,
// such as "0.5", "12", "-0.000000003", or "0". ParseAmount reads it back to
// the same Amount.
func (a Amount) String() string {
	magnitude := uint64(a)
	sign := ""
	if a < 0 {
		magnitude = -magnitude
		sign = "-"
	}
	whole := strconv.FormatUint(magnitude/unit, 10)
	frac := magnitude % unit
	if frac == 0 {
		return sign + whole
	}
	digits := fmt.Sprintf("%09d", frac)
	return sign + whole + "." + strings.TrimRight(digits, "0")
}

// MarshalJSON writes the canonical decimal string. Each value has exactly one
// spelling, so digests over canonical JSON agree for equal amounts; a string
// such as "0.50" read from a store re-encodes as "0.5".
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(`"` + a.String() + `"`), nil
}

// UnmarshalJSON reads a decimal string exactly, as ParseAmount does. It also
// reads a bare JSON number, including exponent forms such as 1e-7, from its
// decimal text without passing through float64. A bare number is what some
// provider APIs report; it originated as a binary float, so digits beyond the
// nano are rounded half to even rather than rejected. JSON null leaves the amount unchanged.
func (a *Amount) UnmarshalJSON(data []byte) error {
	text := string(data)
	if text == "null" {
		return nil
	}
	var (
		amount Amount
		err    error
	)
	if strings.HasPrefix(text, `"`) {
		// Decode the JSON string first: an escaped spelling such as
		// "\u0030.5" is the same text as "0.5".
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf(
				"money: decode %s: %w",
				text,
				errSyntax,
			)
		}
		amount, err = parse(s, false)
	} else {
		amount, err = parse(text, true)
	}
	if err != nil {
		return fmt.Errorf("money: decode %s: %w", text, err)
	}
	*a = amount
	return nil
}

// parse reads decimal text. A JSON number may carry an exponent and is
// rounded half to even at the nano; exact text must be representable.
func parse(s string, number bool) (Amount, error) {
	rest := s
	negative := strings.HasPrefix(rest, "-")
	if negative {
		rest = rest[1:]
	}
	whole, rest := leadingDigits(rest)
	if whole == "" {
		return 0, errSyntax
	}
	if number && len(whole) > 1 && whole[0] == '0' {
		return 0, errSyntax
	}
	frac := ""
	if strings.HasPrefix(rest, ".") {
		frac, rest = leadingDigits(rest[1:])
		if frac == "" {
			return 0, errSyntax
		}
	}
	exponent := 0
	if number && (strings.HasPrefix(
		rest,
		"e",
	) || strings.HasPrefix(
		rest,
		"E",
	)) {
		value, err := parseExponent(rest[1:])
		if err != nil {
			return 0, err
		}
		exponent, rest = value, ""
	}
	if rest != "" {
		return 0, errSyntax
	}
	// The value is digits × 10^shift nanos.
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, nil
	}
	shift := 9 - len(frac) + exponent
	if shift >= 0 {
		if len(digits)+shift > maxDigits+1 {
			return 0, errOverflow
		}
		return fromDecimal(negative, digits+strings.Repeat("0", shift))
	}
	drop := -shift
	if drop > len(digits) {
		// Less than a tenth of a nano: zero after rounding, but not exact.
		if !number {
			return 0, errPrecise
		}
		return 0, nil
	}
	kept, dropped := digits[:len(digits)-drop], digits[len(digits)-drop:]
	if strings.Trim(dropped, "0") == "" {
		return fromDecimal(negative, kept)
	}
	if !number {
		return 0, errPrecise
	}
	return roundHalfEven(negative, kept, dropped)
}

// fromDecimal converts a magnitude in decimal digits to a signed Amount.
func fromDecimal(negative bool, digits string) (Amount, error) {
	if digits == "" {
		return 0, nil
	}
	magnitude, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, errOverflow
	}
	return signed(negative, magnitude)
}

// roundHalfEven rounds kept.dropped to the nearest integer, ties to even.
func roundHalfEven(negative bool, kept, dropped string) (Amount, error) {
	magnitude := new(big.Int)
	if kept != "" {
		magnitude.SetString(kept, 10)
	}
	half := "5" + strings.Repeat("0", len(dropped)-1)
	switch compare := strings.Compare(dropped, half); {
	case compare > 0,
		compare == 0 && magnitude.Bit(0) == 1:
		magnitude.Add(magnitude, big.NewInt(1))
	}
	if !magnitude.IsUint64() {
		return 0, errOverflow
	}
	return signed(negative, magnitude.Uint64())
}

func signed(negative bool, magnitude uint64) (Amount, error) {
	if negative {
		if magnitude > uint64(math.MaxInt64)+1 {
			return 0, errOverflow
		}
		return Amount(-magnitude), nil
	}
	if magnitude > math.MaxInt64 {
		return 0, errOverflow
	}
	return Amount(magnitude), nil
}

func leadingDigits(s string) (string, string) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return s[:end], s[end:]
}

// parseExponent reads a JSON exponent. A magnitude too large for int is
// clamped: any nonzero value then overflows or rounds to zero.
func parseExponent(s string) (int, error) {
	negative := false
	switch {
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	case strings.HasPrefix(s, "-"):
		negative, s = true, s[1:]
	}
	digits, rest := leadingDigits(s)
	if digits == "" || rest != "" {
		return 0, errSyntax
	}
	value, err := strconv.Atoi(digits)
	if err != nil || value > 1<<20 {
		value = 1 << 20
	}
	if negative {
		return -value, nil
	}
	return value, nil
}
