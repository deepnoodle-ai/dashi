package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompactionOutputDeduplicatesStatesAndKeepsCorrections(t *testing.T) {
	for _, mode := range []string{"plain", "print"} {
		t.Run(mode, func(t *testing.T) {
			var output bytes.Buffer
			var ui UI = newLineUI(&output)
			if mode == "print" {
				ui = &printUI{err: &output}
			}
			for _, b := range []Block{
				{ID: "compaction:one", Kind: KindCompaction, Text: "Compacting history..."},
				{ID: "compaction:one", Kind: KindCompaction, Text: "Compaction failed.", Done: true},
				{ID: "compaction:one", Kind: KindCompaction, Text: "Compaction completed.", Done: true},
				{ID: "compaction:two", Kind: KindCompaction, Text: "Compaction completed.", Done: true},
			} {
				ui.Emit(b)
				ui.Emit(b)
			}
			for text, want := range map[string]int{"Compacting history...": 1, "Compaction failed.": 1, "Compaction completed.": 2} {
				if got := strings.Count(output.String(), text); got != want {
					t.Fatalf("%s count=%d want=%d in %q", text, got, want, output.String())
				}
			}
		})
	}
}
