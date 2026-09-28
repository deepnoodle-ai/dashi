package canonjson

import (
	"math"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"key order", `{"b":1,"a":2,"c":{"z":1,"y":2}}`, `{"a":2,"b":1,"c":{"y":2,"z":1}}`},
		{"whitespace", " {\n\t\"a\" : [ 1 , 2 ] ,\r\n \"b\" : null }\n", `{"a":[1,2],"b":null}`},
		{"literals", `[true,false,null]`, `[true,false,null]`},
		{"nested arrays keep order", `[[3,[2,{"b":0,"a":[]}]],1,{}]`, `[[3,[2,{"a":[],"b":0}]],1,{}]`},
		{"duplicate keeps last", `{"a":1,"b":2,"a":3}`, `{"a":3,"b":2}`},
		{"nested duplicate", `{"a":{"x":1,"x":{"y":2}}}`, `{"a":{"x":{"y":2}}}`},
		{"unicode escapes decoded", `"é中😀"`, `"é中😀"`},
		{"html not escaped", `"<a href=\"x\">&"`, `"<a href=\"x\">&"`},
		{"solidus unescaped", `"a\/b"`, `"a/b"`},
		{"short escapes", `"\u0008\u0009\u000a\u000c\u000d"`, `"\b\t\n\f\r"`},
		{"other controls lowercase hex", `"\u0001\u001F"`, `"\u0001\u001f"`},
		{"quote and backslash", `"\"\\"`, `"\"\\"`},
		{"line separators literal", `"  "`, "\"  \""},
		{"lone surrogate", `"\ud800"`, "\"�\""},
		{"utf16 key order", `{"":1,"😀":2,"a":3}`, `{"a":3,"😀":2,"` + "" + `":1}`},
		{"escaped keys sort by value", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
	}
	for _, c := range cases {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNumbers(t *testing.T) {
	cases := map[string]string{
		"0":                              "0",
		"-0":                             "0",
		"-0.0e5":                         "0",
		"100":                            "100",
		"1e2":                            "100",
		"1E+2":                           "100",
		"100.000":                        "100",
		"10000e-2":                       "100",
		"1.50":                           "1.5",
		"15e-1":                          "1.5",
		"0.150e1":                        "1.5",
		"-2.50":                          "-2.5",
		"0.000001":                       "0.000001",
		"1e-6":                           "0.000001",
		"1e-7":                           "1e-7",
		"0.0000001":                      "1e-7",
		"1.25e-7":                        "1.25e-7",
		"123e18":                         "123000000000000000000",
		"1e21":                           "1e+21",
		"1000000000000000000000":         "1e+21",
		"1.5e300":                        "1.5e+300",
		"1e400":                          "1e+400",
		"-1e-400":                        "-1e-400",
		"9007199254740993":               "9007199254740993",
		"9007199254740992":               "9007199254740992",
		"0.1000000000000000000001":       "0.1000000000000000000001",
		"123456789012345678901234567890": "1.2345678901234567890123456789e+29",
		"00012":                          "12", // Not valid JSON input; formatNumber tolerates it.
	}
	for in, want := range cases {
		got, err := formatNumber(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	if _, err := formatNumber("1e99999999999"); err == nil {
		t.Error("huge exponent accepted")
	}
}

// Go writes a float64 in its shortest round-trip form, so the output must be
// RFC 8785's. Vectors are from RFC 8785 Appendix B.
func TestFloatsMatchRFC8785(t *testing.T) {
	vectors := map[uint64]string{
		0x0000000000000000: "0",
		0x8000000000000000: "0",
		0x0000000000000001: "5e-324",
		0x8000000000000001: "-5e-324",
		0x7fefffffffffffff: "1.7976931348623157e+308",
		0xffefffffffffffff: "-1.7976931348623157e+308",
		0x4340000000000000: "9007199254740992",
		0xc340000000000000: "-9007199254740992",
		0x4430000000000000: "295147905179352830000",
		0x44b52d02c7e14af5: "9.999999999999997e+22",
		0x44b52d02c7e14af6: "1e+23",
		0x44b52d02c7e14af7: "1.0000000000000001e+23",
		0x444b1ae4d6e2ef4e: "999999999999999700000",
		0x444b1ae4d6e2ef4f: "999999999999999900000",
		0x444b1ae4d6e2ef50: "1e+21",
		0x3eb0c6f7a0b5ed8c: "9.999999999999997e-7",
		0x3eb0c6f7a0b5ed8d: "0.000001",
		0x41b3de4355555553: "333333333.3333332",
		0x41b3de4355555554: "333333333.33333325",
		0x41b3de4355555555: "333333333.3333333",
		0x41b3de4355555556: "333333333.3333334",
		0x41b3de4355555557: "333333333.33333343",
		0xbecbf647612f3696: "-0.0000033333333333333333",
		0x43143ff3c1cb0959: "1424953923781206.2",
	}
	for bits, want := range vectors {
		out, err := Marshal(math.Float64frombits(bits))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Errorf("%#x: got %s, want %s", bits, out, want)
		}
	}
}

func TestEqual(t *testing.T) {
	if !Equal([]byte(`{"a":1e2,"b":[1.50]}`), []byte(`{ "b": [1.5], "a": 100 }`)) {
		t.Error("equal meaning compared unequal")
	}
	if Equal([]byte(`{"a":1}`), []byte(`{"a":"1"}`)) {
		t.Error("number equals string")
	}
	if Equal([]byte(`{"a":1}`), []byte(`{"a":1}{}`)) {
		t.Error("trailing data accepted")
	}
	if Equal([]byte(`{`), []byte(`{`)) {
		t.Error("invalid JSON compared equal")
	}
}

func TestMarshalStructAndRawMessage(t *testing.T) {
	type value struct {
		Z   string `json:"z"`
		A   any    `json:"a"`
		Raw any    `json:"raw"`
	}
	got, err := Marshal(value{Z: "<&>", A: 1.5, Raw: map[string]any{"k": []any{2, "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":1.5,"raw":{"k":[2,"x"]},"z":"<&>"}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
