package content

import (
	"encoding/json"
	"testing"
)

func TestBlockValidateRejectsInvalid(t *testing.T) {
	tests := []struct {
		name  string
		block Block
	}{
		{
			name:  "empty",
			block: Block{},
		},
		{
			name: "kind mismatch",
			block: Block{
				Kind: KindText,
				Image: &Image{
					Source: Source{
						URL: "https://example.com/i.png",
					},
				},
			},
		},
		{
			name: "multiple source locations",
			block: ImageBlock(Source{
				URL:      "https://example.com/i.png",
				Data:     []byte{1},
				MIMEType: "image/png",
			}),
		},
		{
			name:  "empty inline data",
			block: DocumentBlock(Source{Data: []byte{}}, "empty.pdf"),
		},
		{
			name: "raw citation without provider",
			block: Block{
				Kind: KindText,
				Text: &Text{
					Citations: []Citation{
						{
							Raw: json.RawMessage(`{}`),
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.block.Validate(); err == nil {
				t.Fatalf(
					"accepted invalid block: %+v",
					tt.block,
				)
			}
		})
	}
}

func TestBlockCloneCopiesImageBytes(t *testing.T) {
	b := ImageBlock(Source{Data: []byte{1, 2}, MIMEType: "image/png"})
	if err := b.Validate(); err != nil {
		t.Fatalf("valid image source rejected: %v", err)
	}
	c := b.Clone()
	c.Image.Source.Data[0] = 9
	if b.Image.Source.Data[0] != 1 {
		t.Fatal("image bytes were shared")
	}
}

func TestBlockCloneCopiesCitations(t *testing.T) {
	b := TextBlock("hello")
	b.Text.Citations = []Citation{
		{
			Origin: Origin{
				Provider: "test",
				API:      "api",
				Model:    "m",
				Scope:    "scope",
			},
			Raw: json.RawMessage(`{"id":1}`),
		},
	}
	c := b.Clone()
	c.Text.Citations[0].Raw[0] = '['
	if !json.Valid(b.Text.Citations[0].Raw) {
		t.Fatal("citation bytes were shared")
	}
}

func TestSourceRequiresOriginOnlyForProviderFiles(t *testing.T) {
	origin := Origin{Provider: "p", API: "api", Scope: "account"}
	for _, tc := range []struct {
		name   string
		source Source
		valid  bool
	}{
		{
			name: "file",
			source: Source{
				ID:     "file",
				Origin: origin,
			},
			valid: true,
		},
		{name: "unscoped file", source: Source{ID: "file"}},
		{
			name: "URL",
			source: Source{
				URL: "https://example.com",
			},
			valid: true,
		},
		{
			name: "URL with origin",
			source: Source{
				URL:    "https://example.com",
				Origin: origin,
			},
		},
		{
			name: "inline",
			source: Source{
				Data:     []byte("x"),
				MIMEType: "text/plain",
			},
			valid: true,
		},
		{
			name: "inline with origin",
			source: Source{
				Data:     []byte("x"),
				MIMEType: "text/plain",
				Origin:   origin,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.source.Validate(); (err == nil) != tc.valid {
				t.Fatalf(
					"source validity=%t want=%t: %v",
					err == nil,
					tc.valid,
					err,
				)
			}
		})
	}
}
