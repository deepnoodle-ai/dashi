package content

import "testing"

func TestDetectMIMEType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	for _, tc := range []struct {
		name string
		data []byte
		file string
		want string
	}{
		{name: "pdf bytes", data: []byte("%PDF-1.7"), want: "application/pdf"},
		{name: "png bytes", data: png, want: "image/png"},
		{name: "jpeg bytes", data: []byte("\xff\xd8\xff"), want: "image/jpeg"},
		{name: "text strips charset", data: []byte("hello"), want: "text/plain"},
		{
			name: "markdown by extension",
			data: []byte("# Title"),
			file: "notes.MD",
			want: "text/markdown",
		},
		{name: "csv by extension", data: []byte("a,b"), file: "t.csv", want: "text/csv"},
		{name: "extension wins", data: png, file: "x.pdf", want: "application/pdf"},
		{
			name: "unknown extension falls back",
			data: []byte("%PDF-1.7"),
			file: "report.bin",
			want: "application/pdf",
		},
		{name: "unknown", data: []byte{0, 1, 2}, want: UnknownMIMEType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectMIMEType(tc.data, tc.file); got != tc.want {
				t.Fatalf("DetectMIMEType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCitationLocation(t *testing.T) {
	valid := []CitationLocation{
		{Unit: LocationChar, Start: 0, End: 10},
		{DocumentIndex: 2, Unit: LocationPage, Start: 1, End: 2},
	}
	for _, l := range valid {
		if err := l.Validate(); err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
	}
	invalid := []CitationLocation{
		{Unit: "block", Start: 0, End: 1},
		{Unit: LocationChar, Start: 5, End: 4},
		{Unit: LocationPage, Start: 0, End: 1},
		{DocumentIndex: -1, Unit: LocationChar},
	}
	for _, l := range invalid {
		if l.Validate() == nil {
			t.Fatalf("%+v validated", l)
		}
	}

	b := TextBlock("cited")
	b.Text.Citations = []Citation{{Location: &valid[0]}}
	clone := b.Clone()
	clone.Text.Citations[0].Location.End = 99
	if b.Text.Citations[0].Location.End != 10 {
		t.Fatal("Clone shared the citation location")
	}
	b.Text.Citations[0].Location = &invalid[0]
	if b.Validate() == nil {
		t.Fatal("block with an invalid location validated")
	}
}
