package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/kernel/internal/canonjson"
	"github.com/deepnoodle-ai/kernel/llm"
)

const (
	historyFingerprintV1 = "kernel/history/v1\n"
	historyFingerprintV2 = "kernel/history/v2\n"
)

// fingerprintMessage names the Message fields that participate in a history
// fingerprint explicitly, so a future Message field cannot silently change
// fingerprints of existing turns.
type fingerprintMessage struct {
	Role    llm.Role           `json:"role"`
	Content []llm.Content      `json:"content"`
	Source  *llm.MessageSource `json:"source,omitempty"`
}

func fingerprintMessages(history []llm.Message) []fingerprintMessage {
	// A nil slice and an empty slice both mean there is no prior history.
	messages := make([]fingerprintMessage, 0, len(history))
	for _, message := range history {
		messages = append(
			messages,
			fingerprintMessage{
				Role:    message.Role,
				Content: message.Content,
				Source:  message.Source,
			},
		)
	}
	return messages
}

// historyFingerprint identifies the prior messages supplied for one turn. The
// v2 fingerprint hashes canonical JSON, so history the application reloads
// from a store that keeps only JSON meaning (such as jsonb) still matches.
func historyFingerprint(history []llm.Message) (string, error) {
	encoded, err := canonjson.Marshal(fingerprintMessages(history))
	if err != nil {
		return "", fmt.Errorf("kernel: encode history fingerprint: %w", err)
	}
	return hashFingerprint("v2", historyFingerprintV2, encoded), nil
}

// historyMatches reports whether a stored fingerprint identifies history,
// whose current v2 fingerprint is current.
//
// Compatibility: turns started before v2 store a v1 fingerprint, which hashes
// json.Marshal output. For those, the v1 fingerprint is computed and
// compared; it still depends on the history's exact encoding.
func historyMatches(stored, current string, history []llm.Message) bool {
	if !strings.HasPrefix(stored, "v1:") {
		return stored == current
	}
	encoded, err := json.Marshal(fingerprintMessages(history))
	if err != nil {
		return false
	}
	return stored == hashFingerprint("v1", historyFingerprintV1, encoded)
}

func hashFingerprint(version, domain string, encoded []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write(encoded)
	return version + ":sha256:" + hex.EncodeToString(hash.Sum(nil))
}
