//go:build darwin || linux

package sessionstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func attachmentFixture(t *testing.T) (*Store, *Session, *AttachmentStore) {
	t.Helper()
	st := NewStore(filepath.Join(t.TempDir(), "store"))
	s, err := st.Create(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	a, err := st.Attachments(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return st, s, a
}

type attachmentCancelContext struct {
	context.Context
	calls atomic.Int32
	at    int32
}

func (c *attachmentCancelContext) Err() error {
	if c.calls.Add(1) >= c.at {
		return context.Canceled
	}
	return nil
}

func TestAttachmentInterruptedWriteRemovesOnlyOwnTemp(t *testing.T) {
	st, s, a := attachmentFixture(t)
	id, _ := NewAttachmentID()
	ctx := &attachmentCancelContext{Context: context.Background(), at: 5}
	if _, err := a.Publish(ctx, id, "evidence.txt", "text/plain", []byte(strings.Repeat("x", 96<<10))); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(st.root, s.ID, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished temp remains: %v", entries)
	}
	if st.RetainsAttachments(s) {
		t.Fatal("empty attachment directory retained")
	}
	orphan := filepath.Join(st.root, s.ID, "attachments", ".tmp-uncertain-other-operation")
	os.Mkdir(orphan, 0o700)
	if !st.RetainsAttachments(s) {
		t.Fatal("uncertain foreign temp not retained")
	}
	ctx = &attachmentCancelContext{Context: context.Background(), at: 5}
	a.Publish(ctx, id, "evidence.txt", "text/plain", []byte(strings.Repeat("x", 96<<10)))
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("another operation's content removed")
	}
}

func TestAttachmentStoreAncestorLinkAndSpecialBlob(t *testing.T) {
	st, s, a := attachmentFixture(t)
	id, _ := NewAttachmentID()
	ref, err := a.Publish(context.Background(), id, "a.txt", "text/plain", []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(st.root, s.ID, "attachments", id, "blob")
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Load(context.Background(), s.ID+"-1", ref); err == nil {
		t.Fatal("special blob accepted")
	}
	alias := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(filepath.Dir(st.root), alias); err != nil {
		t.Fatal(err)
	}
	linkedStore := NewStore(filepath.Join(alias, filepath.Base(st.root)))
	if bound, err := linkedStore.Attachments(s); err == nil {
		bound.Close()
		t.Fatal("user-controlled ancestor link accepted")
	}
}

func TestAttachmentCrossSessionNoFallback(t *testing.T) {
	_, s, a := attachmentFixture(t)
	_, other, b := attachmentFixture(t)
	id, _ := NewAttachmentID()
	ref, err := a.Publish(context.Background(), id, "a.txt", "text/plain", []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(context.Background(), other.ID+"-1", ref); err == nil {
		t.Fatal("foreign ref found outside bound session")
	}
	if _, err := b.Load(context.Background(), s.ID+"-1", ref); err == nil {
		t.Fatal("foreign Turn accepted")
	}
}

func TestAttachmentPublicationRetryAndSameSessionTurns(t *testing.T) {
	st, s, a := attachmentFixture(t)
	ctx := context.Background()
	id, _ := NewAttachmentID()
	data := []byte("frozen evidence")
	ref, err := a.Publish(ctx, id, "evidence.txt", "text/plain", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "2", "100"} {
		got, err := a.Load(ctx, s.ID+"-"+n, ref)
		if err != nil || string(got) != string(data) {
			t.Fatalf("%q %v", got, err)
		}
	}
	if again, err := a.Publish(ctx, id, "evidence.txt", "text/plain", data); err != nil || again != ref {
		t.Fatalf("retry=%+v %v", again, err)
	}
	if _, err := a.Publish(ctx, id, "different.txt", "text/plain", data); err == nil {
		t.Fatal("metadata conflict accepted")
	}
	if _, err := a.Publish(ctx, id, "evidence.txt", "text/plain", []byte("different")); err == nil {
		t.Fatal("content conflict accepted")
	}
	for _, turn := range []string{"other-1", s.ID + "-01", s.ID + "-0", s.ID + "--1", s.ID + "-1/.."} {
		if _, err := a.Load(ctx, turn, ref); err == nil {
			t.Fatalf("Turn %q accepted", turn)
		}
	}
	if !st.RetainsAttachments(s) {
		t.Fatal("published orphan is not retained")
	}
	for _, part := range []string{"", "blob", "metadata.json"} {
		info, err := os.Stat(filepath.Join(st.root, s.ID, "attachments", id, part))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if part == "" {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode=%o", part, info.Mode().Perm())
		}
	}
	metaBytes, _ := os.ReadFile(filepath.Join(st.root, s.ID, "attachments", id, "metadata.json"))
	if strings.Contains(string(metaBytes), s.Dir) {
		t.Fatal("source path persisted")
	}
}

func TestAttachmentCorruptionAndStrictIDs(t *testing.T) {
	st, s, a := attachmentFixture(t)
	ctx := context.Background()
	id, _ := NewAttachmentID()
	ref, err := a.Publish(ctx, id, "a.txt", "text/plain", []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../" + id, "ATT_" + strings.Repeat("a", 32), "att_" + strings.Repeat("a", 31), id + "/blob"} {
		r := ref
		r.ID = bad
		if _, err := a.Load(ctx, s.ID+"-1", r); err == nil {
			t.Fatalf("ID %q accepted", bad)
		}
	}
	path := filepath.Join(st.root, s.ID, "attachments", id)
	for _, tt := range []struct {
		name   string
		change func(*attachmentMetadata)
	}{
		{"version", func(m *attachmentMetadata) { m.Version = 2 }}, {"mime", func(m *attachmentMetadata) { m.MIMEType = "application/x-shellscript" }},
		{"size", func(m *attachmentMetadata) { m.Size++ }}, {"digest", func(m *attachmentMetadata) { m.SHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := attachmentMetadata{1, "a.txt", "text/plain", ref.SHA256, ref.Size}
			tt.change(&m)
			b, _ := json.Marshal(m)
			os.WriteFile(filepath.Join(path, "metadata.json"), b, 0o600)
			if _, err := a.Load(ctx, s.ID+"-1", ref); err == nil {
				t.Fatal("corrupt metadata accepted")
			}
		})
	}
	m := attachmentMetadata{1, "a.txt", "text/plain", ref.SHA256, ref.Size}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(path, "metadata.json"), b, 0o600)
	os.WriteFile(filepath.Join(path, "blob"), []byte("abcd"), 0o600)
	if _, err := a.Load(ctx, s.ID+"-1", ref); err == nil {
		t.Fatal("growing blob accepted")
	}
	os.WriteFile(filepath.Join(path, "blob"), []byte("xyz"), 0o600)
	got, err := a.Load(ctx, s.ID+"-1", ref)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(got)
	if hex.EncodeToString(digest[:]) == ref.SHA256 {
		t.Fatal("corruption fixture unchanged")
	}
	// Runner owns final digest verification; retry verifies it locally too.
	if _, err := a.Publish(ctx, id, "a.txt", "text/plain", []byte("abc")); err == nil {
		t.Fatal("corrupt retry reused")
	}
	os.WriteFile(filepath.Join(path, "metadata.json"), []byte(strings.Repeat(" ", attachmentMetadataLimit+1)), 0o600)
	if _, err := a.Load(ctx, s.ID+"-1", ref); err == nil {
		t.Fatal("large metadata accepted")
	}
	if !st.RetainsAttachments(s) {
		t.Fatal("corrupt content not retained")
	}
}

func TestAttachmentAllStorageLinksRejected(t *testing.T) {
	for _, component := range []string{"store", "session", "attachments", "id", "metadata.json", "blob"} {
		t.Run(component, func(t *testing.T) {
			st, s, a := attachmentFixture(t)
			ctx := context.Background()
			id, _ := NewAttachmentID()
			ref, err := a.Publish(ctx, id, "a.txt", "text/plain", []byte("abc"))
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(st.root, s.ID)
			paths := map[string]string{"store": st.root, "session": base, "attachments": filepath.Join(base, "attachments"), "id": filepath.Join(base, "attachments", id), "metadata.json": filepath.Join(base, "attachments", id, "metadata.json"), "blob": filepath.Join(base, "attachments", id, "blob")}
			p := paths[component]
			target := p + "-real"
			if err := os.Rename(p, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			if component == "store" || component == "session" {
				if bound, err := st.Attachments(s); err == nil {
					bound.Close()
					t.Fatal("linked binding accepted")
				}
			} else {
				if _, err := a.Load(ctx, s.ID+"-1", ref); err == nil {
					t.Fatal("linked storage accepted")
				}
			}
			if !st.RetainsAttachments(s) {
				t.Fatal("uncertain/link content not retained")
			}
		})
	}
}

func TestAttachmentCancellationAndEmptyCollision(t *testing.T) {
	st, s, a := attachmentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, _ := NewAttachmentID()
	if _, err := a.Publish(ctx, id, "a.txt", "text/plain", []byte("abc")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if st.RetainsAttachments(s) {
		t.Fatal("cancel before publication retained content")
	}
	if err := os.MkdirAll(filepath.Join(st.root, s.ID, "attachments", id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Publish(context.Background(), id, "a.txt", "text/plain", []byte("abc")); err == nil {
		t.Fatal("empty existing ID overwritten")
	}
	if !st.RetainsAttachments(s) {
		t.Fatal("uncertain empty publication not retained")
	}
	if _, err := a.Load(ctx, s.ID+"-1", content.Ref{ID: id, Size: 3, SHA256: strings.Repeat("0", 64)}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
