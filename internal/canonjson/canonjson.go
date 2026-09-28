// Package canonjson encodes JSON values in one canonical spelling, so values
// that mean the same thing produce the same bytes and therefore the same
// digest.
//
// Kernel digests recorded values (tool definitions, call arguments, commands,
// history) and later compares them with live values. A recorder may store a
// step in a form that keeps only the JSON meaning, such as a Postgres jsonb
// column, which reorders keys, drops whitespace, and respells numbers and
// strings. Hashing the canonical form instead of json.Marshal output makes the
// digest survive that round trip.
//
// The form follows RFC 8785, the JSON Canonicalization Scheme (JCS):
//
//   - no insignificant whitespace;
//   - object members sorted by the UTF-16 code units of their names;
//   - strings escape only '"', '\\', and control characters, using the short
//     escapes \b \t \n \f \r and lowercase \u00xx otherwise; all other
//     characters, including '<', '>', '&', and non-ASCII, are written as
//     UTF-8;
//   - numbers use the ECMAScript Number-to-String layout (plain decimal for
//     exponents -6 through 20, otherwise d.ddde±x) with no trailing zeros.
//
// One deliberate difference from RFC 8785: numbers keep their exact decimal
// value instead of being rounded to an IEEE 754 double first. RFC 8785 writes
// 9007199254740993 as 9007199254740992; this package keeps the two apart, as
// jsonb's arbitrary-precision numeric does. For every number whose exact value
// is the shortest representation of a double, which includes every number Go's
// encoder writes for a float64 and every integer up to 2^53, the output is
// identical to RFC 8785.
//
// Two further rules make equal meaning produce equal bytes:
//
//   - a duplicate object name keeps its last value, as jsonb and Go's decoder
//     do;
//   - invalid UTF-8 and lone UTF-16 surrogate escapes decode to U+FFFD, as in
//     Go's decoder.
//
// The guarantee: two JSON texts canonicalize to the same bytes exactly when
// they have the same structure, the same member names and string contents,
// the same literals, and numerically equal numbers, after duplicate names are
// resolved to their last value. So 1e2, 100, and 100.0 agree, as do 1.5 and
// 1.50; key order and whitespace never matter.
package canonjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Marshal encodes v with encoding/json and returns its canonical form.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Canonicalize returns the canonical form of a single JSON text. Surrounding
// whitespace is allowed; trailing data is an error.
func Canonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("canonjson: trailing data after JSON value")
	}
	var buf bytes.Buffer
	buf.Grow(len(data))
	if err := write(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Equal reports whether a and b are single JSON texts with the same canonical
// form. Invalid JSON is never equal to anything.
func Equal(a, b []byte) bool {
	ca, err := Canonicalize(a)
	if err != nil {
		return false
	}
	cb, err := Canonicalize(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func write(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(v))
	case string:
		writeString(buf, v)
	case json.Number:
		n, err := formatNumber(string(v))
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := write(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonjson: unexpected decoded type %T", value)
	}
	return nil
}

// compareUTF16 orders strings by UTF-16 code units, as RFC 8785 requires. It
// differs from byte order only when a character above U+FFFF meets one in
// U+E000 through U+FFFF.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[r>>4])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			// Ranging over a string yields U+FFFD for invalid UTF-8, which
			// Go's decoder has already substituted, so this stays valid.
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// maxExponent bounds the decimal exponent of a number. It is far beyond any
// value a store keeps (jsonb's numeric allows about 131072 digits before the
// point) and keeps exponent arithmetic from overflowing.
const maxExponent = 1 << 30

// formatNumber writes a JSON number literal in canonical form. It works on
// the decimal digits directly, so it is exact at any size or precision.
func formatNumber(lit string) (string, error) {
	neg := strings.HasPrefix(lit, "-")
	lit = strings.TrimPrefix(lit, "-")
	mantissa, expPart, hasExp := strings.Cut(strings.ToLower(lit), "e")
	exp := 0
	if hasExp {
		e, err := strconv.Atoi(expPart)
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", fmt.Errorf("canonjson: number exponent out of range: %s", lit)
		}
		exp = e
	}
	intPart, fracPart, _ := strings.Cut(mantissa, ".")
	if intPart == "" || !digitsOnly(intPart) || !digitsOnly(fracPart) {
		return "", fmt.Errorf("canonjson: invalid number: %s", lit)
	}
	// value = digits × 10^exp, with no leading or trailing zeros in digits.
	digits := strings.TrimLeft(intPart+fracPart, "0")
	exp -= len(fracPart)
	if digits == "" {
		return "0", nil // Also normalizes -0, as RFC 8785 and jsonb do.
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += len(digits) - len(trimmed)
	digits = trimmed

	// ECMAScript Number::toString: value = 0.d1…dk × 10^n.
	k := len(digits)
	n := k + exp
	var out strings.Builder
	if neg {
		out.WriteByte('-')
	}
	switch {
	case k <= n && n <= 21:
		out.WriteString(digits)
		out.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		out.WriteString(digits[:n])
		out.WriteByte('.')
		out.WriteString(digits[n:])
	case -6 < n && n <= 0:
		out.WriteString("0.")
		out.WriteString(strings.Repeat("0", -n))
		out.WriteString(digits)
	default:
		out.WriteByte(digits[0])
		if k > 1 {
			out.WriteByte('.')
			out.WriteString(digits[1:])
		}
		out.WriteByte('e')
		if n-1 >= 0 {
			out.WriteByte('+')
		}
		out.WriteString(strconv.Itoa(n - 1))
	}
	return out.String(), nil
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
