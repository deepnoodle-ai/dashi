package gemini

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestCaptureBodyBoundsResponseCopy(t *testing.T) {
	read := func(body string, limit int) ([]byte, *bytes.Buffer, error) {
		var out bytes.Buffer
		r := &captureBody{
			ReadCloser: io.NopCloser(strings.NewReader(body)),
			output:     &out,
			mu:         &sync.Mutex{},
			limit:      limit,
		}
		got, err := io.ReadAll(r)
		return got, &out, err
	}
	got, out, err := read("12345678", 8)
	if err != nil || string(got) != "12345678" || out.String() != "12345678" {
		t.Fatalf("body at the limit: got=%q copy=%q err=%v", got, out, err)
	}
	if _, _, err := read("123456789", 8); !errors.Is(err, errCaptureTooLarge) {
		t.Fatalf("body over the limit: err=%v", err)
	}
}
