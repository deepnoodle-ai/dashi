//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
)

func ExampleOpenReader() {
	dir, _ := os.MkdirTemp("", "dashi-reader-")
	defer os.RemoveAll(dir)
	writer, _ := filerecorder.New(filerecorder.Config{Dir: filepath.Join(dir, "turns"), DisableSync: true})
	step := dashi.Step{Version: 1, TurnID: "session-1", Seq: 1, Kind: dashi.StepDelivered}
	_ = writer.Commit(context.Background(), dashi.CommitRequest{Step: step})
	reader, _ := filerecorder.OpenReader(filepath.Join(dir, "turns"))
	defer reader.Close()
	turn, _ := reader.Load(context.Background(), "session-1")
	last, _ := reader.LastTurn(context.Background(), "session")
	fmt.Printf("recorded steps: %d; latest Turn: %d\n", len(turn.Steps), last)
	// Output: recorded steps: 1; latest Turn: 1
}
