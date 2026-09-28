package dashi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
)

// Recorded values are compared by meaning, not by bytes. A recorder only has
// to preserve JSON meaning: a store such as Postgres jsonb may reorder keys,
// drop whitespace, and respell numbers and strings, and every comparison
// Dashi makes between a recorded value and a live one still agrees. See
// package canonjson for the canonical form and its exact guarantee.

// digestPrefix names the hash and the canonical encoding a digest covers.
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

// ArgsDigest returns the digest that binds an ApprovalFact to a tool call's
// arguments. It depends only on the arguments' JSON meaning, so arguments
// shown before a recorder stored them and the same arguments reloaded from a
// store that reorders keys or respells numbers give the same digest. A
// pending call's ToolView.ArgsDigest holds this value already.
func ArgsDigest(args json.RawMessage) string {
	return digest(args)
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
