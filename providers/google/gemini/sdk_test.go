package gemini

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
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

func TestSDKErrorClassifiesOversizedResponseAsProtocol(t *testing.T) {
	// The SDK returns the body's read error as is, or wrapped.
	for _, cause := range []error{errCaptureTooLarge, fmt.Errorf("read: %w", errCaptureTooLarge)} {
		var got *llm.Error
		if !errors.As(sdkError("google", cause, nil, false), &got) ||
			got.Category != llm.ErrorProtocol || got.Retryable {
			t.Fatalf("cause %v: got %#v", cause, got)
		}
	}
}
