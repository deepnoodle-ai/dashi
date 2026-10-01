package dashi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// historyHashDomain separates history hashes from other SHA-256 hashes of
// the same bytes.
const historyHashDomain = "dashi/history/v2\n"

// hashMessage names the Message fields that participate in a history hash
// explicitly, so a future Message field cannot silently change hashes of
// existing turns.
type hashMessage struct {
	Role    llm.Role           `json:"role"`
	Content []llm.Content      `json:"content"`
	Source  *llm.MessageSource `json:"source,omitempty"`
}

func hashMessages(history []llm.Message) []hashMessage {
	// A nil slice and an empty slice both mean there is no prior history.
	messages := make([]hashMessage, 0, len(history))
	for _, message := range history {
		messages = append(
			messages,
			hashMessage{
				Role:    message.Role,
				Content: message.Content,
				Source:  message.Source,
			},
		)
	}
	return messages
}

// HistoryHash identifies history as StartFact.HistoryHash
// and CompactionFact.Hash do. Equal model-visible messages give equal
// hashes, also after a JSON round trip; descriptive context identity
// does not count.
func HistoryHash(history []llm.Message) (string, error) {
	return historyHash(history)
}

// historyHash identifies the prior messages supplied for one turn. It
// hashes canonical JSON, so history the application reloads from a store that
// keeps only JSON meaning (such as jsonb) still matches.
func historyHash(history []llm.Message) (string, error) {
	encoded, err := canonjson.Marshal(hashMessages(history))
	if err != nil {
		return "", fmt.Errorf("dashi: encode history hash: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(historyHashDomain))
	_, _ = hash.Write(encoded)
	return canonjson.HashPrefix + hex.EncodeToString(hash.Sum(nil)), nil
}
