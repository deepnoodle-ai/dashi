package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorReasonHelpersSeeWrappedErrors(t *testing.T) {
	overflow := fmt.Errorf(
		"turn: %w",
		&Error{
			Provider: "p",
			Category: ErrorInvalidRequest,
			Reason:   ReasonContextWindowExceeded,
			Cause:    errors.New("prompt is too long"),
		},
	)
	media := &Error{
		Provider: "p",
		Category: ErrorInvalidRequest,
		Reason:   ReasonMediaRejected,
	}
	plain := &Error{Provider: "p", Category: ErrorInvalidRequest}
	if !IsContextWindowExceeded(overflow) || IsMediaRejected(overflow) {
		t.Fatal("wrapped context overflow was not recognized")
	}
	if !IsMediaRejected(media) || IsContextWindowExceeded(media) {
		t.Fatal("media rejection was not recognized")
	}
	if IsContextWindowExceeded(plain) ||
		IsMediaRejected(plain) ||
		IsMediaRejected(errors.New("x")) ||
		IsContextWindowExceeded(nil) {
		t.Fatal("unclassified error matched a reason")
	}
	if got, want := overflow.Error(), "turn: llm: p: invalid_request (context_window_exceeded): prompt is too long"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := plain.Error(), "llm: p: invalid_request (HTTP 0, )"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
