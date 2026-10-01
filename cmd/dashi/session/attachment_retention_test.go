package session

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
