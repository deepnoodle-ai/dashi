//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/filerecorder"
)

// BenchmarkCommitTurn commits whole turns of 500 steps. Without its index,
// Commit read the whole log each time, so a turn's cost grew with the square
// of its size; this keeps that from returning. Sync is off to isolate the
// reads.
func BenchmarkCommitTurn(b *testing.B) {
	for _, size := range []int{1 << 10, 100 << 10} {
		b.Run(fmt.Sprintf("500x%dKB", size>>10), func(b *testing.B) {
			r, err := filerecorder.New(filerecorder.Config{
				Dir:         b.TempDir(),
				DisableSync: true,
			})
			if err != nil {
				b.Fatal(err)
			}
			text := strings.Repeat("x", size)
			turn := 0
			for b.Loop() {
				turn++
				id := fmt.Sprintf("turn-%d", turn)
				for seq := int64(1); seq <= 500; seq++ {
					commit(b, r, step(id, seq, text), "")
				}
			}
		})
	}
}
