package dashi

import (
	"bytes"
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

// Equal reports whether s and other have the same JSON meaning, the
// comparison Dashi uses for every recorded value (decision 27). Key order,
// whitespace, string escaping, and the spelling of numerically equal numbers
// do not matter, so a step reloaded from a store such as Postgres jsonb
// equals the step that was committed. A step that cannot be encoded equals
// nothing.
//
// A recorder uses Equal to decide whether a commit at an occupied sequence
// is a retry of the recorded step, which succeeds, or a different step from
// a stale worker, which it rejects.
func (s Step) Equal(other Step) bool {
	a, err := canonjson.Marshal(s)
	if err != nil {
		return false
	}
	b, err := canonjson.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}
