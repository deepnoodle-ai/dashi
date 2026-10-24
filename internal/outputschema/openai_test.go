package outputschema

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOpenAIStrictSubset(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{
			name:   "nested objects arrays enum and nullable",
			schema: `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"state":{"type":["string","null"],"enum":["yes","no",null]}},"required":["state"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`,
		},
		{
			name:   "optional field",
			schema: `{"type":"object","properties":{"x":{"type":"string"}},"required":[],"additionalProperties":false}`,
			want:   ".required",
		},
		{
			name:   "nested additional properties",
			schema: `{"type":"object","properties":{"x":{"type":"object","properties":{},"required":[]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].additionalProperties`,
		},
		{
			name:   "local reference remains outside first slice",
			schema: `{"type":"object","properties":{"x":{"$ref":"#/$defs/x"}},"required":["x"],"additionalProperties":false,"$defs":{"x":{"type":"string"}}}`,
			want:   ".$defs",
		},
		{
			name:   "root union",
			schema: `{"type":["object","null"],"properties":{},"required":[],"additionalProperties":false}`,
			want:   ".type",
		},
		{
			name:   "tuple array",
			schema: `{"type":"object","properties":{"x":{"type":"array","items":[{"type":"string"}]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].items`,
		},
		{
			name:   "unsupported format",
			schema: `{"type":"object","properties":{"x":{"type":"string","format":"date"}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].format`,
		},
		{
			name:   "null enum value on non-nullable type",
			schema: `{"type":"object","properties":{"x":{"type":"string","enum":["a",null]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].enum[1]`,
		},
		{
			name:   "string enum value on integer type",
			schema: `{"type":"object","properties":{"x":{"type":"integer","enum":[1,"x"]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].enum[1]`,
		},
		{
			name:   "fractional enum value on integer type",
			schema: `{"type":"object","properties":{"x":{"type":"integer","enum":[1.5]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].enum[0]`,
		},
		{
			name:   "enum on array type",
			schema: `{"type":"object","properties":{"x":{"type":"array","items":{"type":"string"},"enum":["a"]}},"required":["x"],"additionalProperties":false}`,
			want:   `properties["x"].enum`,
		},
		{
			name:   "number enum accepts integers",
			schema: `{"type":"object","properties":{"x":{"type":["number","null"],"enum":[1,2.5,null]}},"required":["x"],"additionalProperties":false}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOpenAIStrict(json.RawMessage(tc.schema))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}
}

func TestOpenAIStrictCaps(t *testing.T) {
	var many strings.Builder
	many.WriteString(`{"type":"object","properties":{`)
	for i := range 101 {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`"p`)
		many.WriteString(fmt.Sprint(i))
		many.WriteString(`":{"type":"string"}`)
	}
	many.WriteString(`},"required":[`)
	for i := range 101 {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`"p`)
		many.WriteString(fmt.Sprint(i))
		many.WriteByte('"')
	}
	many.WriteString(`],"additionalProperties":false}`)
	if err := CheckOpenAIStrict(json.RawMessage(many.String())); err == nil || !strings.Contains(err.Error(), "total exceeds 100") {
		t.Fatalf("property cap: %v", err)
	}

	deep := `{"type":"string"}`
	for range 5 {
		deep = `{"type":"array","items":` + deep + `}`
	}
	deep = `{"type":"object","properties":{"x":` + deep + `},"required":["x"],"additionalProperties":false}`
	if err := CheckOpenAIStrict(json.RawMessage(deep)); err == nil || !strings.Contains(err.Error(), "nesting exceeds 5") {
		t.Fatalf("depth cap: %v", err)
	}

	var enums strings.Builder
	enums.WriteString(`{"type":"object","properties":{"x":{"type":"string","enum":[`)
	for i := range 251 {
		if i > 0 {
			enums.WriteByte(',')
		}
		fmt.Fprintf(&enums, `"v%d"`, i)
	}
	enums.WriteString(`]}},"required":["x"],"additionalProperties":false}`)
	if err := CheckOpenAIStrict(json.RawMessage(enums.String())); err == nil ||
		!strings.Contains(err.Error(), "total exceeds 250") {
		t.Fatalf("enum cap: %v", err)
	}
}
