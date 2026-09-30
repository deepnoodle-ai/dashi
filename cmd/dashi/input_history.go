package main

import (
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
	data, err := io.ReadAll(io.LimitReader(file, 64<<20+1))
	if err != nil {
		return nil, false, errors.New("input history could not be read")
	}
	if len(data) > 64<<20 {
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
	if err := h.save(additions, replaceCorrupt); err != nil {
		if h.warn != nil {
			h.warn(err)
		}
		return
	}
	h.mu.Lock()
	h.pending = h.pending[len(additions):]
	h.replaceCorrupt = false
	h.mu.Unlock()
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
		entries = historyAppend(entries, value)
	}
	data, err := json.Marshal(struct {
		Entries []string `json:"entries"`
	}{entries})
	if err != nil {
		return errors.New("input history could not be encoded")
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
