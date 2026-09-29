package prompt

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestConfirm(t *testing.T) {
	in := New(strings.NewReader(" Y \nno\n"))
	ctx := context.Background()
	for _, want := range []bool{true, false, false} {
		got, err := in.Confirm(ctx, "Deploy?")
		if err != nil || got != want {
			t.Fatalf("Confirm = %v, %v; want %v", got, err, want)
		}
	}
}

func TestAskEndsWithContext(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	in := New(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.Ask(ctx, "> "); err != context.Canceled {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}
