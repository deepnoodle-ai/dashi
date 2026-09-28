package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/deepnoodle-ai/kernel/internal/canonjson"
	"github.com/deepnoodle-ai/kernel/llm"
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

// commandDigestMatches reports whether stored identifies command, so a retry
// with a recorded command ID acknowledges the first application.
//
// Compatibility: before exact money, a budget's amount was a float64 and
// encoded as a JSON number; it now encodes as a decimal string. A digest
// recorded then covers the number form, so a command with a cost cap is also
// checked in that form. A changed amount still encodes differently and is
// still a conflict.
func commandDigestMatches(stored string, command Command) bool {
	if digestMatches(stored, command) {
		return true
	}
	legacy, ok := floatMoneyCommand(command)
	return ok && digestMatches(stored, legacy)
}

// floatMoneyBudget, floatMoneyStart, and floatMoneyContinue reproduce the
// JSON of Budget, Start, and Continue from before exact money: the same
// fields in the same order, with a float64 amount. Keep them in step with
// those types only as they were then; new fields never appear in old digests.
type floatMoneyBudget struct {
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int `json:"max_total_tokens,omitempty"`
	MaxCost         *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"max_cost,omitempty"`
}

type floatMoneyStart struct {
	ID           string
	Origin       StartOrigin
	Budget       *floatMoneyBudget
	OutputSchema json.RawMessage
	History      []llm.Message
	Input        []llm.Message
}

type floatMoneyContinue struct {
	ID      string
	History []llm.Message
	Budget  *floatMoneyBudget
}

// floatMoneyCommand returns command as it encoded before exact money, or
// false when it has no cost cap and so encodes the same either way.
func floatMoneyCommand(command Command) (any, bool) {
	convert := func(b *Budget) (*floatMoneyBudget, bool) {
		if b == nil || b.MaxCost == nil {
			return nil, false
		}
		out := &floatMoneyBudget{
			MaxOutputTokens: b.MaxOutputTokens,
			MaxTotalTokens:  b.MaxTotalTokens,
		}
		out.MaxCost = &struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		}{
			// Float64 is the nearest double to the exact amount, which is
			// what the old decimal literal or parsed value held.
			Amount:   b.MaxCost.Amount.Float64(),
			Currency: b.MaxCost.Currency,
		}
		return out, true
	}
	switch c := command.(type) {
	case Start:
		budget, ok := convert(c.Budget)
		return floatMoneyStart{
			ID:           c.ID,
			Origin:       c.Origin,
			Budget:       budget,
			OutputSchema: c.OutputSchema,
			History:      c.History,
			Input:        c.Input,
		}, ok
	case Continue:
		budget, ok := convert(c.Budget)
		return floatMoneyContinue{
			ID:      c.ID,
			History: c.History,
			Budget:  budget,
		}, ok
	}
	return nil, false
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
