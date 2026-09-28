package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/deepnoodle-ai/kernel/internal/canonjson"
)

// Recorded values are compared by meaning, not by bytes. A recorder only has
// to preserve JSON meaning: a store such as Postgres jsonb may reorder keys,
// drop whitespace, and respell numbers and strings, and every comparison
// Kernel makes between a recorded value and a live one still agrees. See
// package canonjson for the canonical form and its exact guarantee.

// digestPrefix marks a digest of a value's canonical JSON. Digests written
// before canonical hashing are bare hex of the json.Marshal output; see
// digestMatches.
const digestPrefix = "v2:sha256:"

// digest identifies v by the SHA-256 of its canonical JSON.
func digest(v any) string {
	b, err := canonjson.Marshal(v)
	if err != nil {
		// Unreachable for values that encode: canonicalization only fails on
		// numbers beyond any store's range. Hash the plain encoding so the
		// result stays deterministic.
		b, _ = json.Marshal(v)
	}
	h := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(h[:])
}

// commandDigest is digest for a command, reporting values that cannot be
// encoded.
func commandDigest(c Command) (string, error) {
	if _, err := json.Marshal(c); err != nil {
		return "", err
	}
	return digest(c), nil
}

// sameJSONValue reports whether a and b encode to the same JSON meaning.
func sameJSONValue(a, b any) bool {
	return digest(a) == digest(b)
}

// digestMatches reports whether stored, a digest taken from a turn record or
// supplied by the application, identifies v.
//
// Compatibility: records written before canonical hashing hold bare hex of
// json.Marshal output. Such a digest is checked the old way, so those turns
// keep working, though it matches only while v's encoding is byte-identical.
func digestMatches(stored string, v any) bool {
	if strings.HasPrefix(stored, digestPrefix) {
		return stored == digest(v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	h := sha256.Sum256(b)
	return stored == hex.EncodeToString(h[:])
}
