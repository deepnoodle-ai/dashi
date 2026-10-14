package providerhttp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// MultipartBoundary returns a multipart boundary derived from the values a
// form will carry, so the same values always give the same request body. The
// boundary never occurs in any of the values.
func MultipartBoundary(values ...[]byte) string {
	h := sha256.New()
	for _, value := range values {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		h.Write(value)
	}
	return boundaryFrom(h.Sum(nil), values)
}

// boundaryFrom returns the first boundary in the hash chain that starts at
// sum and occurs in none of values.
func boundaryFrom(sum []byte, values [][]byte) string {
	for {
		boundary := "dashi-" + hex.EncodeToString(sum[:20])
		if !containsAny(values, []byte(boundary)) {
			return boundary
		}
		next := sha256.Sum256(sum)
		sum = next[:]
	}
}

func containsAny(values [][]byte, sub []byte) bool {
	for _, value := range values {
		if bytes.Contains(value, sub) {
			return true
		}
	}
	return false
}
