package responses

import "testing"

func TestFileName(t *testing.T) {
	for _, tc := range []struct{ title, mimeType, want string }{
		{title: "", mimeType: "application/pdf", want: "document.pdf"},
		{title: "Q3 report", mimeType: "application/pdf", want: "Q3 report.pdf"},
		{title: "Q3 v1.2", mimeType: "application/pdf", want: "Q3 v1.2.pdf"},
		{title: "report.PDF", mimeType: "application/pdf", want: "report.PDF"},
		{title: "notes.md", mimeType: "text/markdown", want: "notes.md"},
		{title: "deck.pptx", mimeType: "application/x-unknown", want: "deck.pptx"},
	} {
		if got := fileName(tc.title, tc.mimeType); got != tc.want {
			t.Errorf("fileName(%q, %q) = %q, want %q", tc.title, tc.mimeType, got, tc.want)
		}
	}
}
