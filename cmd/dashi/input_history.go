package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const historyLimit = 1000
const historyByteLimit = 64 << 20

var errHistoryEntryTooLarge = errors.New("input history: an entry exceeds the 64 MiB serialized size limit and was not saved; it remains in local recall")
var errHistoryEntryInvalid = errors.New("input history: an entry is not valid UTF-8 and was not saved; it remains in local recall")

// inputHistory persists only submitted text. The worker serializes this process's
// pending additions; the advisory lock serializes concurrent CLI writers.
type inputHistory struct {
	closeOnce      sync.Once
	path           string
	warn           func(error)
	mu             sync.Mutex
	pending        []string
	replaceCorrupt bool
	wake           chan struct{}
	stop           chan struct{}
	done           chan struct{}
}

func openInputHistory(path string, warn func(error)) (*inputHistory, []string) {
	h := &inputHistory{path: path, warn: warn, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	entries, corrupt, err := readInputHistory(path)
	h.replaceCorrupt = corrupt
	if err != nil && warn != nil {
		warn(err)
	}
	go func() {
		defer close(h.done)
		for {
			select {
			case <-h.wake:
				h.flush()
			case <-h.stop:
				h.flush()
				return
			}
		}
	}()
	return h, entries
}
func (h *inputHistory) append(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	h.mu.Lock()
	h.pending = append(h.pending, value)
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *inputHistory) close() { h.closeOnce.Do(func() { close(h.stop) }); <-h.done }
func historyAppend(entries []string, value string) []string {
	if strings.TrimSpace(value) == "" || len(entries) > 0 && entries[len(entries)-1] == value {
		return entries
	}
	entries = append(entries, value)
	if len(entries) > historyLimit {
		entries = entries[len(entries)-historyLimit:]
	}
	return entries
}
func readInputHistory(path string) ([]string, bool, error) {
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if parentErr == nil && (!parent.IsDir() || parent.Mode()&os.ModeSymlink != 0) {
		return nil, false, errors.New("input history directory must be a real directory")
	}
	flags, err := preferenceFileFlags()
	if err != nil {
		return nil, false, fmt.Errorf("input history: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|flags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("input history could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errors.New("input history must be a regular file")
	}
	// Keep old existing files private before loading their contents.
	if err = file.Chmod(0600); err != nil {
		return nil, false, errors.New("input history permissions could not be secured")
	}
	data, err := io.ReadAll(io.LimitReader(file, historyByteLimit+1))
	if err != nil {
		return nil, false, errors.New("input history could not be read")
	}
	if len(data) > historyByteLimit {
		return nil, false, errors.New("input history exceeds the 64 MiB load limit")
	}
	var saved struct {
		Entries []string `json:"entries"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &saved) != nil {
		return nil, true, errors.New("input history is invalid; recall starts empty")
	}
	var entries []string
	for _, entry := range saved.Entries {
		entries = historyAppend(entries, entry)
	}
	return entries, false, nil
}
func (h *inputHistory) flush() {
	h.mu.Lock()
	additions := slices.Clone(h.pending)
	replaceCorrupt := h.replaceCorrupt
	h.mu.Unlock()
	if len(additions) == 0 {
		return
	}
	// Permanent entry failures stay in composer recall but leave the persistence
	// retry list. Filesystem and lock failures keep all supported additions.
	var supported []string
	var rejected []error
	for _, value := range additions {
		if _, err := encodeHistoryEntry(value, historyByteLimit); err != nil {
			rejected = append(rejected, err)
			continue
		}
		supported = append(supported, value)
	}
	var saveErr error
	if len(supported) > 0 {
		saveErr = h.save(supported, replaceCorrupt)
	}
	h.mu.Lock()
	if saveErr != nil {
		h.pending = append(supported, h.pending[len(additions):]...)
	} else {
		h.pending = h.pending[len(additions):]
		if len(supported) > 0 {
			h.replaceCorrupt = false
		}
	}
	h.mu.Unlock()
	if h.warn != nil {
		for _, err := range rejected {
			h.warn(err)
		}
		if saveErr != nil {
			h.warn(saveErr)
		}
	}
}
func (h *inputHistory) save(additions []string, replaceCorrupt bool) error {
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("input history directory could not be created")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("input history directory must be a real directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return errors.New("input history directory permissions could not be secured")
	}
	flags, err := preferenceFileFlags()
	if err != nil {
		return fmt.Errorf("input history: %w", err)
	}
	lock, err := os.OpenFile(h.path+".lock", os.O_CREATE|os.O_RDWR|flags, 0600)
	if err != nil {
		return errors.New("input history lock could not be opened")
	}
	defer lock.Close()
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("input history lock must be a regular file")
	}
	if err = lock.Chmod(0600); err != nil {
		return errors.New("input history lock permissions could not be secured")
	}
	if err = lockPreferences(lock); err != nil {
		return errors.New("input history is busy or cannot be locked; retry at the next submission")
	}
	entries, corrupt, err := readInputHistory(h.path)
	if err != nil && !(corrupt && replaceCorrupt) {
		return err
	}
	for _, value := range additions {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := encodeHistoryEntry(value, historyByteLimit); err != nil {
			return err
		}
		entries = historyAppend(entries, value)
	}
	data, err := encodeInputHistory(entries, historyByteLimit)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return errors.New("input history temporary file could not be created")
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return errors.New("input history could not be written")
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return errors.New("input history could not be synced")
	}
	if err = temp.Close(); err != nil {
		return errors.New("input history could not be closed")
	}
	if err = os.Rename(temp.Name(), h.path); err != nil {
		return errors.New("input history could not be replaced; previous history kept")
	}
	return nil
}

// The byte budget uses encoded size, including JSON escaping and delimiters.
// Retention removes only oldest whole entries; retained text stays exact.
func encodeInputHistory(entries []string, limit int) ([]byte, error) {
	const empty = `{"entries":[]}`
	size := len(empty)
	var encoded [][]byte
	for i := len(entries) - 1; i >= 0; i-- {
		entry, err := encodeHistoryEntry(entries[i], limit)
		if err != nil {
			return nil, err
		}
		extra := len(entry)
		if len(encoded) > 0 {
			extra++
		}
		if size+extra > limit {
			break
		}
		encoded = append(encoded, entry)
		size += extra
	}
	var out bytes.Buffer
	out.Grow(size)
	out.WriteString(`{"entries":[`)
	for i := len(encoded) - 1; i >= 0; i-- {
		if i != len(encoded)-1 {
			out.WriteByte(',')
		}
		out.Write(encoded[i])
	}
	out.WriteString(`]}`)
	return out.Bytes(), nil
}
func encodeHistoryEntry(value string, limit int) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, errHistoryEntryInvalid
	}
	entry, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("input history could not be encoded; previous history kept")
	}
	if len(entry)+len(`{"entries":[]}`) > limit {
		return nil, errHistoryEntryTooLarge
	}
	return entry, nil
}
