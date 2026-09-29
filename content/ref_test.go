package content

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func validRefSource() Source {
	return Source{
		Ref:      &Ref{ID: "upl_1", SHA256: testDigest, Size: 812},
		MIMEType: "image/png",
	}
}

func TestSourceRefRoundTripsThroughJSON(t *testing.T) {
	source := validRefSource()

	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Source
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Ref == nil || *decoded.Ref != *source.Ref {
		t.Fatalf("decoded ref = %+v", decoded.Ref)
	}
	if decoded.MIMEType != "image/png" {
		t.Fatalf("MIME type = %q", decoded.MIMEType)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded source is invalid: %v", err)
	}
}

func TestSourceValidateRef(t *testing.T) {
	expires := time.Now()
	tests := []struct {
		name   string
		mutate func(*Source)
		valid  bool
	}{
		{name: "reference", mutate: func(*Source) {}, valid: true},
		{
			name:   "missing MIME type",
			mutate: func(s *Source) { s.MIMEType = "" },
		},
		{
			name:   "with data",
			mutate: func(s *Source) { s.Data = []byte("x") },
		},
		{
			name:   "with URL",
			mutate: func(s *Source) { s.URL = "https://example.test/a.png" },
		},
		{
			name:   "with file ID",
			mutate: func(s *Source) { s.ID = "file_1" },
		},
		{
			name: "with URI",
			mutate: func(s *Source) {
				s.URI = "https://files.example.test/f"
			},
		},
		{
			name:   "with origin",
			mutate: func(s *Source) { s.Origin = Origin{Provider: "x"} },
		},
		{
			name:   "with expiry",
			mutate: func(s *Source) { s.ExpiresAt = &expires },
		},
		{
			name:   "empty ID",
			mutate: func(s *Source) { s.Ref.ID = "" },
		},
		{
			name:   "short digest",
			mutate: func(s *Source) { s.Ref.SHA256 = "abc" },
		},
		{
			name: "uppercase digest",
			mutate: func(s *Source) {
				s.Ref.SHA256 = strings.ToUpper(testDigest)
			},
		},
		{
			name: "non-hex digest",
			mutate: func(s *Source) {
				s.Ref.SHA256 = strings.Repeat("z", 64)
			},
		},
		{
			name:   "zero size",
			mutate: func(s *Source) { s.Ref.Size = 0 },
		},
		{
			name:   "negative size",
			mutate: func(s *Source) { s.Ref.Size = -1 },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := validRefSource()
			ref := *source.Ref
			source.Ref = &ref
			tt.mutate(&source)

			err := source.Validate()

			if (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tt.valid)
			}
		})
	}
}

func TestSourceCloneDetachesRef(t *testing.T) {
	source := validRefSource()

	clone := source.Clone()
	clone.Ref.ID = "changed"

	if source.Ref.ID != "upl_1" {
		t.Fatal("clone shared the reference")
	}
}

func TestBlockSourcesAliasThePayload(t *testing.T) {
	block := DocumentBlock(validRefSource(), "a.png")

	sources := block.Sources()
	sources[0].Ref = nil

	if block.Document.Source.Ref != nil {
		t.Fatal("Sources did not alias the block")
	}
	if got := TextBlock("x").Sources(); len(got) != 0 {
		t.Fatalf("text block sources = %d", len(got))
	}
}
