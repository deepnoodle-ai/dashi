package sessionstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
)

const attachmentBlobLimit = 10 << 20
const attachmentMetadataLimit = 16 << 10

var attachmentIDPattern = regexp.MustCompile(`^att_[0-9a-f]{32}$`)
var attachmentSessionPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

type attachmentMetadata struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	MIMEType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

// AttachmentStore is bound to an existing CLI session. Its caller must hold
// that session's Store.Lock lease until Close and throughout publication and
// resolution. It grants no source-file or tool permission.
type AttachmentStore struct {
	sessionID string
	session   *attachmentDir
}

// Attachments binds to a validated session whose directory already exists
// under the caller's established session lease. It does not create sessions.
func (st *Store) Attachments(s *Session) (*AttachmentStore, error) {
	if s == nil || !validAttachmentSession(s.ID) {
		return nil, fmt.Errorf("session: invalid attachment session")
	}
	anchor, err := openAttachmentAnchor(st.root)
	if err != nil {
		return nil, err
	}
	defer anchor.close()
	dir, err := anchor.child(s.ID)
	if err != nil {
		return nil, err
	}
	return &AttachmentStore{s.ID, dir}, nil
}

func validAttachmentSession(id string) bool {
	if !attachmentSessionPattern.MatchString(id) {
		return false
	}
	_, err := time.Parse("20060102-150405", id[:15])
	return err == nil
}

// Close releases descriptors, not the caller's session lease.
func (a *AttachmentStore) Close() { a.session.close() }

// SaveSessionHeader persists non-authoritative discovery/preferences metadata
// before attachment Start admission. Only Turn records acknowledge submitted
// input. The caller holds this same session's lease; no new session is created.
func (a *AttachmentStore) SaveSessionHeader(ctx context.Context, s *Session) error {
	if s == nil || s.ID != a.sessionID {
		return fmt.Errorf("session: attachment header belongs to another session")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	id, err := NewAttachmentID()
	if err != nil {
		return err
	}
	tmp := ".session-" + id + ".tmp"
	defer a.session.root.Remove(tmp)
	if err := writeAttachment(ctx, a.session, tmp, data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.session.root.Rename(tmp, "session.json"); err != nil {
		return err
	}
	return a.session.file.Sync()
}

// NewAttachmentID allocates an opaque snapshot identity. Keep this ID with
// its pending input; retry Publish with the same ID and immutable bytes.
func NewAttachmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "att_" + hex.EncodeToString(b[:]), nil
}

func attachmentMIME(m string) bool {
	switch m {
	case "text/plain", "image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf":
		return true
	}
	return false
}

func (a *AttachmentStore) directory(create bool) (*attachmentDir, error) {
	if create {
		if err := a.session.root.Mkdir("attachments", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return a.session.child("attachments")
}

// Publish atomically writes a private immutable snapshot before Start. It
// verifies any existing ID before retry reuse, never overwrites it, and never
// rereads a source path. A failed sync can leave a retained publication.
func (a *AttachmentStore) Publish(ctx context.Context, id, name, mime string, data []byte) (content.Ref, error) {
	if err := ctx.Err(); err != nil {
		return content.Ref{}, err
	}
	if !attachmentIDPattern.MatchString(id) || !attachmentMIME(mime) || len(data) == 0 || len(data) > attachmentBlobLimit || name == "" || name != filepath.Base(name) || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00/\\") || len(name) > 1024 {
		return content.Ref{}, fmt.Errorf("session: invalid attachment snapshot")
	}
	digest := sha256.Sum256(data)
	ref := content.Ref{ID: id, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
	meta := attachmentMetadata{1, name, mime, ref.SHA256, ref.Size}
	dir, err := a.directory(true)
	if err != nil {
		return content.Ref{}, err
	}
	defer dir.close()
	if _, err := dir.root.Lstat(id); err == nil {
		oldMeta, oldData, err := readAttachment(ctx, dir, ref)
		if err != nil {
			return content.Ref{}, err
		}
		oldDigest := sha256.Sum256(oldData)
		if oldMeta != meta || hex.EncodeToString(oldDigest[:]) != ref.SHA256 || int64(len(oldData)) != ref.Size {
			return content.Ref{}, fmt.Errorf("session: attachment retry conflicts with its publication")
		}
		if err := dir.file.Sync(); err != nil {
			return content.Ref{}, err
		}
		if err := a.session.file.Sync(); err != nil {
			return content.Ref{}, err
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return content.Ref{}, err
	}
	nonce, err := NewAttachmentID()
	if err != nil {
		return content.Ref{}, err
	}
	tmp := ".tmp-" + nonce
	if err := dir.root.Mkdir(tmp, 0o700); err != nil {
		return content.Ref{}, err
	}
	published := false
	defer func() {
		if !published {
			dir.root.RemoveAll(tmp)
		}
	}()
	private, err := dir.child(tmp)
	if err != nil {
		return content.Ref{}, err
	}
	metaBytes, err := json.Marshal(meta)
	if err == nil {
		err = writeAttachment(ctx, private, "blob", data)
	}
	if err == nil {
		err = writeAttachment(ctx, private, "metadata.json", metaBytes)
	}
	if err == nil {
		err = private.file.Sync()
	}
	private.close()
	if err != nil {
		return content.Ref{}, err
	}
	if err := ctx.Err(); err != nil {
		return content.Ref{}, err
	}
	if err := dir.publishUnused(tmp, id); err != nil {
		return content.Ref{}, err
	}
	published = true
	if err := dir.file.Sync(); err != nil {
		return content.Ref{}, err
	}
	// Persist the attachments directory entry as well as the ID entry.
	if err := a.session.file.Sync(); err != nil {
		return content.Ref{}, err
	}
	return ref, nil
}

func writeAttachment(ctx context.Context, dir *attachmentDir, name string, data []byte) error {
	f, err := dir.regular(name, true)
	if err != nil {
		return err
	}
	for len(data) > 0 && err == nil {
		if err = ctx.Err(); err != nil {
			break
		}
		n := min(len(data), 32<<10)
		var written int
		written, err = f.Write(data[:n])
		if err == nil && written != n {
			err = io.ErrShortWrite
		}
		data = data[written:]
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// Resolve reads only a strictly identified snapshot in this bound session.
// The runner verifies the returned bytes' final size and digest.
func (a *AttachmentStore) Resolve(ctx context.Context, turnID string, ref content.Ref) ([]byte, error) {
	prefix := a.sessionID + "-"
	if !strings.HasPrefix(turnID, prefix) {
		return nil, fmt.Errorf("session: attachment belongs to another session")
	}
	number := strings.TrimPrefix(turnID, prefix)
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 || strconv.Itoa(n) != number {
		return nil, fmt.Errorf("session: invalid attachment Turn ID")
	}
	dir, err := a.directory(false)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	_, data, err := readAttachment(ctx, dir, ref)
	return data, err
}

func readAttachment(ctx context.Context, dir *attachmentDir, ref content.Ref) (attachmentMetadata, []byte, error) {
	var meta attachmentMetadata
	if !attachmentIDPattern.MatchString(ref.ID) || ref.Size <= 0 || ref.Size > attachmentBlobLimit {
		return meta, nil, fmt.Errorf("session: invalid attachment reference")
	}
	if err := ref.Validate(); err != nil {
		return meta, nil, err
	}
	id, err := dir.child(ref.ID)
	if err != nil {
		return meta, nil, err
	}
	defer id.close()
	metadata, err := readAttachmentFile(ctx, id, "metadata.json", attachmentMetadataLimit)
	if err != nil {
		return meta, nil, err
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return meta, nil, err
	}
	if meta.Version != 1 || !attachmentMIME(meta.MIMEType) || meta.SHA256 != ref.SHA256 || meta.Size != ref.Size || meta.Name == "" || meta.Name != filepath.Base(meta.Name) || strings.ContainsAny(meta.Name, "\x00/\\") || len(meta.Name) > 1024 {
		return meta, nil, fmt.Errorf("session: attachment metadata does not match reference")
	}
	data, err := readAttachmentFile(ctx, id, "blob", ref.Size)
	if err != nil {
		return meta, nil, err
	}
	return meta, data, nil
}

func readAttachmentFile(ctx context.Context, dir *attachmentDir, name string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := dir.regular(name, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var data []byte
	buf := make([]byte, 32<<10)
	remaining := limit + 1
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf[:min(int64(len(buf)), remaining)])
		data = append(data, buf[:n]...)
		remaining -= int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("session: attachment exceeds recorded size")
	}
	return data, ctx.Err()
}

// RetainsAttachments reports whether Discard must preserve this session.
// Errors and unexpected entries are retained conservatively. It follows no
// links and does not inspect or remove published, corrupt, or orphan content.
func (st *Store) RetainsAttachments(s *Session) bool {
	a, err := st.Attachments(s)
	if errors.Is(err, errAttachmentUnsupported) {
		return !attachmentPathAbsent(filepath.Join(st.root, s.ID, "attachments"))
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer a.Close()
	dir, err := a.directory(false)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer dir.close()
	entries, err := dir.file.ReadDir(1)
	return len(entries) > 0 || err != nil && err != io.EOF
}
