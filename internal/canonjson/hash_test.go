package canonjson

import (
	"encoding/json"
	"math"
	"testing"
)

func TestSameValue(t *testing.T) {
	if !SameValue(map[string]any{"a": 1.0, "b": 2}, json.RawMessage(`{"b":2,"a":1}`)) {
		t.Error("equal meaning compared unequal")
	}
	if SameValue(map[string]any{"a": 1}, map[string]any{"a": 2}) {
		t.Error("different values compared equal")
	}
	if SameValue(math.NaN(), math.Inf(1)) {
		t.Error("two different unencodable values compared equal")
	}
	if SameValue(json.RawMessage(`{`), json.RawMessage(`[`)) {
		t.Error("two different invalid raw messages compared equal")
	}
	if !SameValue(json.RawMessage(`{`), json.RawMessage(`{`)) {
		t.Error("identical invalid raw messages compared unequal")
	}
	if SameValue(math.NaN(), 1.0) {
		t.Error("unencodable value equals an encodable one")
	}
}
