package canonjson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

// HashPrefix names the hash function and the canonical encoding a hash covers.
const HashPrefix = "v2:sha256:"

// Hash identifies v by the SHA-256 of its canonical JSON.
func Hash(v any) string {
	b, _ := encode(v)
	h := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(h[:])
}

// SameValue reports whether a and b encode to the same JSON meaning. Values
// that do not encode at all are the same only when they are deeply equal.
func SameValue(a, b any) bool {
	ab, aerr := encode(a)
	bb, berr := encode(b)
	switch {
	case aerr != nil && berr != nil:
		return reflect.DeepEqual(a, b)
	case aerr != nil || berr != nil:
		return false
	}
	return bytes.Equal(ab, bb)
}

// encode returns the canonical JSON of v. Canonicalization only fails on
// numbers beyond any store's range; encode then returns the plain encoding
// so the result stays deterministic.
func encode(v any) ([]byte, error) {
	b, err := Marshal(v)
	if err != nil {
		return json.Marshal(v)
	}
	return b, nil
}
