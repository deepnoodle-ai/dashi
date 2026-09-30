package session

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MaxResourceBytes = 32 << 20

// ResourceReference identifies immutable recovery inputs, never turn status.
type ResourceReference struct {
	Version       int    `json:"version"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	CatalogDigest string `json:"catalog_digest"`
}

// SaveResources publishes private immutable bytes before recording their reference.
func (st *Store) SaveResources(s *Session, data []byte, catalog string) (ResourceReference, error) {
	sum := sha256.Sum256(data)
	ref := ResourceReference{1, hex.EncodeToString(sum[:]), int64(len(data)), catalog}
	if err := ref.validate(); err != nil {
		return ResourceReference{}, err
	}
	if !json.Valid(data) {
		return ResourceReference{}, errors.New("session: invalid resource snapshot JSON")
	}
	root, err := st.resourceRoot(s, true)
	if err != nil {
		return ResourceReference{}, err
	}
	defer root.Close()
	if stored, err := readResource(root, ref, true); err == nil {
		if !bytes.Equal(stored, data) {
			return ResourceReference{}, errors.New("session: resource snapshot bytes differ")
		}
		if err := syncResourceDirectory(root); err != nil {
			return ResourceReference{}, err
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ResourceReference{}, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return ResourceReference{}, err
	}
	temporary := "." + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ResourceReference{}, err
	}
	defer root.Remove(temporary)
	n, writeErr := file.Write(data)
	if n != len(data) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return ResourceReference{}, err
	}
	// Link publishes without replacing an existing immutable snapshot.
	if err = root.Link(temporary, ref.SHA256+".json"); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ResourceReference{}, err
		}
		stored, readErr := readResource(root, ref, true)
		if readErr != nil || !bytes.Equal(stored, data) {
			return ResourceReference{}, errors.Join(errors.New("session: resource publication collision"), readErr)
		}
	}
	if err = root.Remove(temporary); err != nil {
		return ResourceReference{}, err
	}
	if err = syncResourceDirectory(root); err != nil {
		return ResourceReference{}, err
	}
	return ref, nil
}

// ReadResources verifies version, private file identity, size, and content hash.
func (st *Store) ReadResources(s *Session, ref ResourceReference) ([]byte, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	root, err := st.resourceRoot(s, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readResource(root, ref, false)
}
func (ref ResourceReference) validate() error {
	digest := func(text string) bool {
		raw, err := hex.DecodeString(text)
		return err == nil && len(raw) == sha256.Size && text == strings.ToLower(text)
	}
	if ref.Version != 1 || ref.Size <= 0 || ref.Size > MaxResourceBytes || !digest(ref.SHA256) || !strings.HasPrefix(ref.CatalogDigest, "sha256:") || !digest(strings.TrimPrefix(ref.CatalogDigest, "sha256:")) {
		return errors.New("session: invalid resource reference")
	}
	return nil
}
func (st *Store) resourceRoot(s *Session, create bool) (*os.Root, error) {
	if s == nil || s.ID == "" || s.ID == "." || s.ID == ".." || filepath.Base(s.ID) != s.ID || strings.ContainsAny(s.ID, "/\\\x00") {
		return nil, errors.New("session: invalid resource session ID")
	}
	if create {
		if err := os.MkdirAll(st.root, 0700); err != nil {
			return nil, err
		}
	}
	before, err := os.Lstat(st.root)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session: resource store must be a directory and cannot be a symlink")
	}
	root, err := os.OpenRoot(st.root)
	if err != nil {
		return nil, err
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(before, current) {
		root.Close()
		return nil, errors.Join(errors.New("session: resource store changed"), err)
	}
	for _, part := range []string{s.ID, "resources"} {
		if create {
			if err := root.Mkdir(part, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
			if err := syncResourceDirectory(root); err != nil {
				root.Close()
				return nil, err
			}
		}
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !privateResourceDirectory(before) {
			root.Close()
			return nil, errors.New("session: resource directory must be private and cannot be a symlink")
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		current, err := next.Stat(".")
		if err != nil || !os.SameFile(before, current) {
			next.Close()
			return nil, errors.Join(errors.New("session: resource directory changed"), err)
		}
		root = next
	}
	return root, nil
}
func privateResourceDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 == 0
}
func syncResourceDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
func readResource(root *os.Root, ref ResourceReference, sync bool) ([]byte, error) {
	name := ref.SHA256 + ".json"
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session: resource file must be private and regular")
	}
	file, err := openResourceFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(before, current) {
		return nil, errors.Join(errors.New("session: resource file changed"), err)
	}
	if current.Size() != ref.Size || !current.Mode().IsRegular() || current.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session: resource file size or permissions changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxResourceBytes+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, errors.New("session: resource snapshot content hash mismatch")
	}
	if sync {
		if err := file.Sync(); err != nil {
			return nil, fmt.Errorf("session: sync resource snapshot: %w", err)
		}
	}
	return data, nil
}
