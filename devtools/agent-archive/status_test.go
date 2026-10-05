package main

import (
	"strings"
	"testing"
)

func TestPrintVerdict(t *testing.T) {
	for _, tc := range []struct {
		warnings, failed int
		want             string
	}{
		{0, 0, "\nall clear: no warnings and no failed files\n"},
		{1, 0, "\nneeds a look: 1 warning, no failed files\n"},
		{0, 2, "\nneeds a look: 0 warnings, 2 failed files\n"},
		{3, 1, "\nneeds a look: 3 warnings, 1 failed file\n"},
	} {
		var b strings.Builder
		printVerdict(&b, tc.warnings, tc.failed)
		if b.String() != tc.want {
			t.Errorf("printVerdict(%d, %d) = %q, want %q", tc.warnings, tc.failed, b.String(), tc.want)
		}
	}
}
