package canonjson

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// HashPrefix names the hash function and the canonical encoding a hash covers.
const HashPrefix = "v2:sha256:"

// Hash identifies v by the SHA-256 of its canonical JSON.
func Hash(v any) string {
	b, err := Marshal(v)
	if err != nil {
		// Unreachable for values that encode: canonicalization only fails on
		// numbers beyond any store's range. Hash the plain encoding so the
		// result stays deterministic.
		b, _ = json.Marshal(v)
	}
	h := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(h[:])
}

// SameValue reports whether a and b encode to the same JSON meaning.
func SameValue(a, b any) bool {
	return Hash(a) == Hash(b)
}
