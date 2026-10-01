package sessionstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateTitle accepts one printable Unicode line of 1–160 runes.
func ValidateTitle(title string) (string, error) {
	if !utf8.ValidString(title) || strings.ContainsAny(title, "\r\n") {
		return "", errors.New("session: title must be one printable line")
	}
	for _, r := range title {
		if !unicode.IsPrint(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errors.New("session: title must contain printable characters only")
		}
	}
	title = strings.TrimSpace(title)
	if n := utf8.RuneCountInString(title); n < 1 || n > 160 {
		return "", errors.New("session: title must contain 1–160 runes")
	}
	return title, nil
}

// Rename returns independently owned metadata only after atomic publication.
// The Engine must hold the current session lease and require idle mode before
// calling it. This helper takes no lease and never mutates the caller's value.
func (st *Store) Rename(current *Session, title string) (*Session, error) {
	return st.rename(current, title, nil)
}

func (st *Store) rename(current *Session, title string, beforeReplace func() error) (*Session, error) {
	title, err := ValidateTitle(title)
	if err != nil {
		return nil, err
	}
	if current == nil || !validAttachmentSession(current.ID) {
		return nil, errors.New("session: send the first request before naming the session")
	}
	anchor, err := openAttachmentAnchor(st.root)
	if err != nil {
		return nil, errors.New("session: saved session unavailable")
	}
	defer anchor.close()
	dir, err := anchor.child(current.ID)
	if err != nil {
		return nil, errors.New("session: saved session unavailable")
	}
	defer dir.close()
	data, err := readInspectionMetadata(context.Background(), dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("session: send the first request before naming the session")
	}
	if err != nil {
		return nil, errors.New("session: saved metadata unavailable")
	}
	var saved Session
	if json.Unmarshal(data, &saved) != nil || saved.ID != current.ID || saved.Dir != current.Dir {
		return nil, errors.New("session: saved metadata identity changed")
	}
	candidate := *current
	candidate.NextTurn = bytes.Clone(current.NextTurn)
	candidate.Title = title
	candidate.Updated = st.now()
	data, err = json.MarshalIndent(candidate, "", "  ")
	if err != nil || len(data) > inspectionMetadataLimit {
		return nil, errors.New("session: cannot encode bounded session metadata")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	name := "session-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := dir.regular(name, true)
	if err != nil {
		return nil, errors.New("session: cannot create metadata temporary file")
	}
	defer dir.root.Remove(name)
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if errors.Join(writeErr, closeErr) != nil {
		return nil, errors.New("session: cannot write metadata temporary file")
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return nil, err
		}
	}
	if err := dir.root.Rename(name, "session.json"); err != nil {
		return nil, errors.New("session: cannot replace session metadata")
	}
	return &candidate, nil
}
