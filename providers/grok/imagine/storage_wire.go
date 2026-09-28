package imagine

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

type fileOutputWire struct {
	FileID             string          `json:"file_id"`
	Filename           string          `json:"filename"`
	ExpiresAt          json.RawMessage `json:"expires_at"`
	PublicURL          string          `json:"public_url"`
	PublicURLExpiresAt json.RawMessage `json:"public_url_expires_at"`
	PublicURLError     string          `json:"public_url_error"`
}

func (wire fileOutputWire) storedOutput(
	index int,
	mimeType string,
	files content.Origin,
) (media.StoredOutput, error) {
	if wire.FileID == "" || wire.Filename == "" {
		return media.StoredOutput{}, errors.New("imagine: stored output requires file_id and filename")
	}
	file := content.Source{ID: wire.FileID, Origin: files, MIMEType: mimeType}
	file.ExpiresAt = parseOutputExpiry(wire.ExpiresAt)
	output := media.StoredOutput{
		ContentIndex: index,
		File:         file,
		Filename:     wire.Filename,
	}
	if wire.PublicURLError != "" {
		output.PublicURLError = wire.PublicURLError
	} else if wire.PublicURL != "" {
		if validHTTPURL(wire.PublicURL) {
			public := content.Source{
				URL:       wire.PublicURL,
				MIMEType:  mimeType,
				ExpiresAt: parseOutputExpiry(wire.PublicURLExpiresAt),
			}
			output.PublicURL = &public
		} else {
			// Public URL creation is independent of successful file storage.
			output.PublicURLError = "provider returned an invalid public URL"
		}
	}
	return output, nil
}

// The REST docs describe Unix seconds; protobuf JSON surfaces RFC3339. An
// unknown or malformed optional expiry stays unknown without hiding the file.
func parseOutputExpiry(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err == nil {
		value := time.Unix(seconds, 0).UTC()
		if validOutputExpiry(value) {
			return &value
		}
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil
	}
	if value, err := time.Parse(time.RFC3339Nano, encoded); err == nil {
		value = value.UTC()
		if validOutputExpiry(value) {
			return &value
		}
		return nil
	}
	if value, err := strconv.ParseInt(
		strings.TrimSpace(encoded),
		10,
		64,
	); err == nil {
		parsed := time.Unix(value, 0).UTC()
		if validOutputExpiry(parsed) {
			return &parsed
		}
	}
	return nil
}

func validOutputExpiry(value time.Time) bool {
	return value.Year() >= 1 && value.Year() <= 9999
}
