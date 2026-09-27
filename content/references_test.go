package content

import (
	"testing"
	"time"
)

func TestSourceValidateProviderURIRequiresOriginAndMIMEType(t *testing.T) {
	origin := Origin{Provider: "google", API: "google.generate_content", Scope: "key"}
	tests := []struct {
		name   string
		source Source
		valid  bool
	}{
		{
			name:   "provider URI",
			source: Source{URI: "https://files.example.test/f", MIMEType: "application/pdf", Origin: origin},
			valid:  true,
		},
		{
			name:   "missing origin",
			source: Source{URI: "https://files.example.test/f", MIMEType: "application/pdf"},
		},
		{
			name:   "missing MIME type",
			source: Source{URI: "https://files.example.test/f", Origin: origin},
		},
		{
			name: "ambiguous location",
			source: Source{
				URI:      "https://files.example.test/f",
				URL:      "https://example.test/f",
				MIMEType: "application/pdf",
				Origin:   origin,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.source.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("source validity=%t want=%t: %v", err == nil, tt.valid, err)
			}
		})
	}
}

func TestBlockCloneCopiesReferenceExpiry(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	block := Block{
		Kind: KindResource,
		Resource: &Resource{
			URI:       "https://example.test/file",
			ExpiresAt: &expires,
		},
	}
	clone := block.Clone()
	*clone.Resource.ExpiresAt = expires.AddDate(1, 0, 0)
	if !block.Resource.ExpiresAt.Equal(expires) {
		t.Fatal("resource expiry shared caller pointer")
	}
}
