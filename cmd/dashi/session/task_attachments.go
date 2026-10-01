package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
)

// TaskStorageAbsent establishes that no child could have started in this
// session. PrepareStart durably creates children before recording any child
// turn, even without attachments, and the CLI never removes that directory.
// Existing storage or an uncertain path cannot establish absence.
func (st *Store) TaskStorageAbsent(s *Session) bool {
	if s == nil || !validAttachmentSession(s.ID) {
		return false
	}
	parent, err := st.Attachments(s)
	if errors.Is(err, errAttachmentUnsupported) {
		return attachmentPathAbsent(filepath.Join(st.root, s.ID, "children"))
	}
	if err != nil {
		return false
	}
	defer parent.Close()
	children, err := parent.session.child("children")
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err == nil {
		children.close()
	}
	return false
}

// ChildAttachments binds one derived child to private evidence beneath the
// parent's leased store. Recovery opens existing directories without creation.
func (a *AttachmentStore) ChildAttachments(parent dashi.CallRef, create bool) (*AttachmentStore, error) {
	if err := parent.Validate(); err != nil {
		return nil, err
	}
	prefix := a.sessionID + "-"
	if !strings.HasPrefix(parent.TurnID, prefix) || !validTurnSuffix(strings.TrimPrefix(parent.TurnID, prefix)) {
		return nil, fmt.Errorf("session: task parent belongs to another session")
	}
	childID := delegate.ChildID(parent.TurnID, parent.CallID)
	digest := sha256.Sum256([]byte(childID))
	if create {
		if err := a.session.root.Mkdir("children", 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	children, err := a.session.child("children")
	if err != nil {
		return nil, err
	}
	defer children.close()
	name := hex.EncodeToString(digest[:])
	if create {
		if err := children.root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	dir, err := children.child(name)
	if err != nil {
		return nil, err
	}
	if create {
		err = errors.Join(children.file.Sync(), a.session.file.Sync())
		if err != nil {
			dir.close()
			return nil, err
		}
	}
	return &AttachmentStore{sessionID: childID, session: dir}, nil
}

func validTurnSuffix(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && strconv.Itoa(n) == s
}
