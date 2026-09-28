package media

import (
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
)

// StoredOutput links one generated media block to an additional file held by
// the provider. File is a scoped provider ID, not a host-owned artifact. The
// generated block remains available in Content, often through a short-lived
// URL. A public URL, when present, has its own lifetime and access policy.
type StoredOutput struct {
	ContentIndex   int             `json:"content_index"`
	File           content.Source  `json:"file"`
	Filename       string          `json:"filename,omitempty"`
	PublicURL      *content.Source `json:"public_url,omitempty"`
	PublicURLError string          `json:"public_url_error,omitempty"`
}

// Clone detaches source expiry values from the original record.
func (s StoredOutput) Clone() StoredOutput {
	s.File = s.File.Clone()
	if s.PublicURL != nil {
		url := s.PublicURL.Clone()
		s.PublicURL = &url
	}
	return s
}

func validateStoredOutputs(
	outputs []StoredOutput,
	blocks []content.Block,
	provider string,
) error {
	seen := make(map[int]struct{}, len(outputs))
	for _, output := range outputs {
		if output.ContentIndex < 0 ||
			output.ContentIndex >= len(blocks) {
			return errors.New("media: stored output index is outside content")
		}
		if _, exists := seen[output.ContentIndex]; exists {
			return errors.New("media: duplicate stored output index")
		}
		seen[output.ContentIndex] = struct{}{}
		if blocks[output.ContentIndex].Kind != content.KindImage &&
			blocks[output.ContentIndex].Kind != content.KindVideo &&
			blocks[output.ContentIndex].Kind != content.KindAudio {
			return errors.New("media: stored output must refer to generated media")
		}
		if output.File.ID == "" ||
			output.File.Origin.Provider != provider {
			return errors.New("media: stored output requires a file ID from the result provider")
		}
		if err := output.File.Validate(); err != nil {
			return fmt.Errorf("media: stored output file: %w", err)
		}
		if output.PublicURL != nil {
			if output.PublicURL.URL == "" ||
				output.PublicURL.Origin != (content.Origin{}) {
				return errors.New("media: stored output public URL requires an unscoped URL")
			}
			if err := output.PublicURL.Validate(); err != nil {
				return fmt.Errorf(
					"media: stored output public URL: %w",
					err,
				)
			}
		}
		if output.PublicURL != nil && output.PublicURLError != "" {
			return errors.New("media: stored output cannot have both public URL and URL error")
		}
	}
	return nil
}

func cloneStoredOutputs(outputs []StoredOutput) []StoredOutput {
	copy := slices.Clone(outputs)
	for i := range copy {
		copy[i] = copy[i].Clone()
	}
	return copy
}
