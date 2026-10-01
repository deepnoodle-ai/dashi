package sessionstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedAttachmentAbsencePreservesExistingAndUncertainPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session", "attachments")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if !attachmentPathAbsent(path) {
		t.Fatal("missing attachment directory was retained")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if attachmentPathAbsent(path) {
		t.Fatal("existing attachment directory was treated as absent")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Dir(path), link); err == nil && attachmentPathAbsent(filepath.Join(link, "missing")) {
		t.Fatal("symlink ancestor was followed to certify absence")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("uncertain"), 0600); err != nil {
		t.Fatal(err)
	}
	if attachmentPathAbsent(filepath.Join(file, "attachments")) {
		t.Fatal("unexpected ancestor type was treated as absent")
	}
}

func TestTaskStorageAbsenceChecksExistingAndUnsafeMarkers(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(root)
	s, err := st.Create(root, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, s.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if !st.TaskStorageAbsent(s) {
		t.Fatal("absent child storage not established")
	}
	path := filepath.Join(dir, "children")
	if err = os.WriteFile(path, []byte("uncertain"), 0600); err != nil {
		t.Fatal(err)
	}
	if st.TaskStorageAbsent(s) {
		t.Fatal("unexpected child marker certified absence")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "absent"), path); err == nil {
		if st.TaskStorageAbsent(s) {
			t.Fatal("child symlink certified absence")
		}
	}
	if st.TaskStorageAbsent(nil) || st.TaskStorageAbsent(&Session{ID: "../outside"}) {
		t.Fatal("invalid identity certified absence")
	}
}
