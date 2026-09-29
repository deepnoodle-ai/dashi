package main

import "testing"

func TestRun(t *testing.T) {
	if err := run(t.Context()); err != nil {
		t.Fatal(err)
	}
}
