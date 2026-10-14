package providerhttp

import (
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestReadResponseBoundsBody(t *testing.T) {
	raw, err := readResponse("p", strings.NewReader("12345678"), 8)
	if err != nil || string(raw) != "12345678" {
		t.Fatalf("body at the limit: raw=%q err=%v", raw, err)
	}
	_, err = readResponse("p", strings.NewReader("123456789"), 8)
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Category != llm.ErrorProtocol {
		t.Fatalf("body over the limit: err=%v", err)
	}
}
