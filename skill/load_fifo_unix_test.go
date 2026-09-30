//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package skill_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/skill"
)

func TestLoadDirsRefusesFIFODefinitions(t *testing.T) {
	for _, kind := range []string{"folder", "command"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "blocked.md")
			if kind == "folder" {
				if err := os.Mkdir(filepath.Join(root, "blocked"), 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(root, "blocked", "SKILL.md")
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan bool, 1)
			go func() { set, warnings := skill.LoadDirs(root); done <- len(set.Skills()) == 0 && len(warnings) > 0 }()
			select {
			case rejected := <-done:
				if !rejected {
					t.Fatal("FIFO definition accepted")
				}
			case <-time.After(time.Second):
				f, _ := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if f != nil {
					f.Close()
				}
				t.Fatal("discovery blocked on FIFO")
			}
		})
	}
}
