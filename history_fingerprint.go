package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/kernel/llm"
)

const historyFingerprintVersion = "kernel/history/v1\n"

// historyFingerprint identifies the prior messages supplied for one turn.
// This v1 encoding names the participating Message fields explicitly, so a
// future Message field cannot silently change fingerprints of existing turns.
func historyFingerprint(history []llm.Message) (string, error) {
	type messageV1 struct {
		Role    llm.Role           `json:"role"`
		Content []llm.Content      `json:"content"`
		Source  *llm.MessageSource `json:"source,omitempty"`
	}

	// A nil slice and an empty slice both mean there is no prior history.
	messages := make([]messageV1, 0, len(history))
	for _, message := range history {
		messages = append(messages, messageV1{
			Role:    message.Role,
			Content: message.Content,
			Source:  message.Source,
		})
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return "", fmt.Errorf("kernel: encode history fingerprint: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(historyFingerprintVersion))
	_, _ = hash.Write(encoded)
	return "v1:sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
