package llm

import (
	"testing"
	"time"
)

func TestModelEvidenceRequiresScopeAndProvenance(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*ModelEvidence)
	}{
		{
			name: "model",
			edit: func(e *ModelEvidence) {
				e.Model = ""
			},
		},
		{
			name: "API",
			edit: func(e *ModelEvidence) {
				e.API = ""
			},
		},
		{
			name: "endpoint",
			edit: func(e *ModelEvidence) {
				e.Endpoint = ""
			},
		},
		{
			name: "endpoint user info",
			edit: func(e *ModelEvidence) {
				e.Endpoint = "https://secret@example.com/api"
			},
		},
		{
			name: "owner",
			edit: func(e *ModelEvidence) {
				e.Owner = ""
			},
		},
		{
			name: "source",
			edit: func(e *ModelEvidence) {
				e.Source = ""
			},
		},
		{
			name: "method",
			edit: func(e *ModelEvidence) {
				e.Method = ""
			},
		},
		{
			name: "unknown method",
			edit: func(e *ModelEvidence) {
				e.Method = "unknown"
			},
		},
		{
			name: "date",
			edit: func(e *ModelEvidence) {
				e.QualifiedAt = time.Time{}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := ModelEvidence{
				Model:       "m",
				API:         "api",
				Endpoint:    "https://example.com/api",
				Owner:       "test",
				Source:      "local fixture",
				Method:      EvidenceFixture,
				QualifiedAt: time.Now(),
			}
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			test.edit(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("incomplete evidence accepted")
			}
		})
	}
}
