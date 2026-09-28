package main

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/dashitest"
)

func TestFileRecorderConforms(t *testing.T) {
	dashitest.Run(t, func(t *testing.T) dashitest.Store {
		r, err := newFileRecorder(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return r
	})
}
