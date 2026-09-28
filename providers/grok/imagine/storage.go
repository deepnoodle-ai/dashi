package imagine

import (
	"errors"
	"strings"
	"time"
)

// StorageOptions asks xAI to retain each generated asset in the caller's
// Files API storage. File storage and public URL creation are separate facts.
// A nil StorageOptions leaves storage to the host's usual output handling.
type StorageOptions struct {
	Filename              string        `json:"filename"`
	ExpiresAfter          time.Duration `json:"expires_after,omitempty"`
	PublicURL             bool          `json:"public_url,omitempty"`
	PublicURLExpiresAfter time.Duration `json:"public_url_expires_after,omitempty"`
}

func (s StorageOptions) wire() (map[string]any, error) {
	if strings.TrimSpace(s.Filename) != s.Filename || s.Filename == "" ||
		s.Filename == "." || s.Filename == ".." ||
		strings.ContainsAny(s.Filename, "/\\\x00") {
		return nil, errors.New("imagine: storage filename must be a single nonempty path segment")
	}
	if err := validateStorageExpiry(s.ExpiresAfter); err != nil {
		return nil, err
	}
	if err := validateStorageExpiry(s.PublicURLExpiresAfter); err != nil {
		return nil, err
	}
	if s.PublicURLExpiresAfter > 0 && !s.PublicURL {
		return nil, errors.New("imagine: public URL expiry requires public URL creation")
	}
	if s.ExpiresAfter > 0 && s.PublicURLExpiresAfter > s.ExpiresAfter {
		return nil, errors.New("imagine: public URL cannot outlive the stored file")
	}
	body := map[string]any{"filename": s.Filename}
	if s.ExpiresAfter > 0 {
		body["expires_after"] = int64(s.ExpiresAfter / time.Second)
	}
	if s.PublicURL {
		if s.PublicURLExpiresAfter > 0 {
			body["public_url"] = map[string]any{
				"expires_after": int64(s.PublicURLExpiresAfter / time.Second),
			}
		} else {
			body["public_url"] = true
		}
	}
	return body, nil
}

func validateStorageExpiry(duration time.Duration) error {
	if duration == 0 {
		return nil
	}
	if duration < time.Hour ||
		duration > 30*24*time.Hour ||
		duration%time.Second != 0 {
		return errors.New("imagine: storage expiry must be whole seconds from 1 hour to 30 days")
	}
	return nil
}

func addStorageOptions(body map[string]any, options *StorageOptions) error {
	if options == nil {
		return nil
	}
	wire, err := options.wire()
	if err != nil {
		return err
	}
	body["storage_options"] = wire
	return nil
}
