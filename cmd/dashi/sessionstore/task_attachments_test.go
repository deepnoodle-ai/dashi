//go:build darwin || linux

package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/session"
)

func TestChildAttachmentsStrictNamespaceConflictAndNoFollow(t *testing.T) {
	st, s, parent := attachmentFixture(t)
	if !st.TaskStorageAbsent(s) {
		t.Fatal("fresh session cannot establish absent child storage")
	}
	ref := dashi.CallRef{TurnID: session.TurnID(s.ID, 1), CallID: "task:/weird"}
	if _, err := parent.ChildAttachments(ref, false); err == nil {
		t.Fatal("missing recovery directory created")
	}
	child, err := parent.ChildAttachments(ref, true)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if st.TaskStorageAbsent(s) {
		t.Fatal("child storage was omitted from recovery evidence")
	}
	id, _ := NewAttachmentID()
	snapshot, err := child.Publish(t.Context(), id, "proof.txt", "text/plain", []byte("fixed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Publish(t.Context(), id, "proof.txt", "text/plain", []byte("wrong")); err == nil {
		t.Fatal("conflicting child publication overwritten")
	}
	turn := session.TurnID(delegate.ChildID(ref.TurnID, ref.CallID), 1)
	if data, err := child.Resolve(t.Context(), turn, snapshot); err != nil || string(data) != "fixed" {
		t.Fatalf("%q %v", data, err)
	}
	for _, wrong := range []string{ref.TurnID, turn + "/../1", turn + "0", session.TurnID(delegate.ChildID(ref.TurnID, "other"), 1)} {
		// Turn 10 is valid in the same logical child, so use a leading-zero suffix.
		if wrong == turn+"0" {
			wrong = delegate.ChildID(ref.TurnID, ref.CallID) + "-01"
		}
		if _, err := child.Resolve(t.Context(), wrong, snapshot); err == nil {
			t.Fatalf("wrong namespace accepted %s", wrong)
		}
	}
	if _, err := parent.ChildAttachments(dashi.CallRef{TurnID: "another-1", CallID: "task"}, true); err == nil {
		t.Fatal("cross-parent storage accepted")
	}
	digest := sha256.Sum256([]byte(delegate.ChildID(ref.TurnID, ref.CallID)))
	dir := filepath.Join(st.root, s.ID, "children", hex.EncodeToString(digest[:]))
	if err := os.Rename(dir, dir+"-kept"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"-kept", dir); err != nil {
		t.Fatal(err)
	}
	if bound, err := parent.ChildAttachments(ref, false); err == nil {
		bound.Close()
		t.Fatal("child symlink followed")
	}
	if st.TaskStorageAbsent(s) {
		t.Fatal("uncertain child storage certified absence")
	}
}
