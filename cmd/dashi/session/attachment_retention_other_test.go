//go:build !darwin && !linux

package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedAttachmentDiscard(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "existing"}[existing], func(t *testing.T) {
			st := NewStore(t.TempDir())
			s, err := st.Create(t.TempDir(), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(st.root, s.ID)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.Mkdir(filepath.Join(dir, "attachments"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			st.Discard(s)
			_, err = os.Stat(dir)
			if existing && err != nil || !existing && !os.IsNotExist(err) {
				t.Fatalf("existing=%v stat=%v", existing, err)
			}
		})
	}
}

func TestUnsupportedTaskStorageAbsence(t *testing.T) {
	st := NewStore(t.TempDir())
	s, err := st.Create(t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(st.root, s.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if !st.TaskStorageAbsent(s) {
		t.Fatal("empty session cannot establish absent child storage")
	}
	if err = os.Mkdir(filepath.Join(dir, "children"), 0700); err != nil {
		t.Fatal(err)
	}
	if st.TaskStorageAbsent(s) {
		t.Fatal("existing child storage certified absence")
	}
}
