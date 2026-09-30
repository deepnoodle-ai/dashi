package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResourceBytesSurviveReopenAndDetectCorruption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewStore(root)
	s := &Session{ID: "fixture-session"}
	data := []byte(`{"instructions":"original bytes","skills":["review"]}`)
	catalog := "sha256:" + strings.Repeat("a", 64)
	ref, err := store.SaveResources(s, data, catalog)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, s.ID, "resources", ref.SHA256+".json")
	for _, directory := range []string{root, filepath.Join(root, s.ID), filepath.Dir(path)} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory %s: %v %v", directory, info, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	loaded, err := NewStore(root).ReadResources(s, ref)
	if err != nil || !bytes.Equal(loaded, data) {
		t.Fatalf("reopen=%s %v", loaded, err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			again, err := store.SaveResources(s, data, catalog)
			if err != nil || again != ref {
				t.Errorf("immutable reuse=%+v %v", again, err)
			}
		})
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("publication left files: %v %v", entries, err)
	}
	changed := bytes.Clone(data)
	changed[18] = 'X'
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadResources(s, ref); err == nil {
		t.Fatal("corrupt bytes accepted")
	}
	if _, err := store.SaveResources(s, data, catalog); err == nil {
		t.Fatal("corrupt immutable entry overwritten")
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(current, changed) {
		t.Fatal("corrupt entry replaced")
	}
}

func TestResourceSnapshotsRejectUnsafeFilesAndReferences(t *testing.T) {
	for _, scenario := range []string{"file-link", "directory-link", "public-file", "public-directory", "traversal", "bad-reference", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			store := NewStore(root)
			s := &Session{ID: "fixture"}
			raw := []byte(`{"instructions":"fixture"}`)
			ref, err := store.SaveResources(s, raw, "sha256:"+strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, s.ID, "resources")
			path := filepath.Join(directory, ref.SHA256+".json")
			switch scenario {
			case "file-link":
				external := filepath.Join(t.TempDir(), "snapshot.json")
				if err := os.WriteFile(external, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
					t.Skip(err)
				}
			case "directory-link":
				if err := os.RemoveAll(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), directory); err != nil {
					t.Skip(err)
				}
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				s.ID = "../outside"
			case "bad-reference":
				ref.SHA256 = "../../outside"
				ref.Version = 2
			case "oversize":
				if _, err := store.SaveResources(s, bytes.Repeat([]byte("x"), MaxResourceBytes+1), ref.CatalogDigest); err == nil {
					t.Fatal("oversized snapshot accepted")
				}
				return
			}
			if _, err := store.ReadResources(s, ref); err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}

func TestResourceConcurrentFirstPublication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewStore(root)
	s := &Session{ID: "fresh"}
	data := []byte(`{"instructions":"immutable first publication"}`)
	catalog := "sha256:" + strings.Repeat("b", 64)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			<-start
			ref, err := store.SaveResources(s, data, catalog)
			if err != nil {
				t.Error(err)
				return
			}
			if got, err := store.ReadResources(s, ref); err != nil || !bytes.Equal(got, data) {
				t.Errorf("publication=%s %v", got, err)
			}
		})
	}
	close(start)
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(root, s.ID, "resources"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("published entries=%v %v", entries, err)
	}
}
