package main

import (
	"context"
	"io"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
)

// TestKeyless runs the stand-in scenario: two failing runs, one reminder,
// then a fix and a passing run.
func TestKeyless(t *testing.T) {
	out, err := run(context.Background(), false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if item, ok := out.Context(dashi.ContextRuntime, turnwatch.Name); !ok ||
		item.ID != "turnwatch-1" {
		t.Fatalf("reminder = %+v, %v; want turnwatch-1", item, ok)
	}
}
