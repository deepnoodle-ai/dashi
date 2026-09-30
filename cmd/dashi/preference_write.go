package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var errPreferencesChanged = errors.New("user settings changed during save; retry")

// userPreferenceWriter updates only user preferences. Root keeps every file
// operation bound to the opened directory while editors and other CLI writers
// may replace path names. The advisory lock orders cooperating CLI writers.
type userPreferenceWriter struct {
	path          string
	beforeReplace func() // test boundary for an uncooperative external editor
}

func (w userPreferenceWriter) update(updates map[string]json.RawMessage) error {
	if len(updates) == 0 {
		return nil
	}
	for field, value := range updates {
		switch field {
		case "provider", "model", "thinking_effort", "show_thinking", "temperature", "max_tokens":
		default:
			return errors.New("cannot save unknown preference field")
		}
		if !json.Valid(value) {
			return errors.New("cannot save invalid preference value")
		}
	}
	flags, err := preferenceFileFlags()
	if err != nil {
		return err
	}
	dir, name := filepath.Dir(w.path), filepath.Base(w.path)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("cannot create user settings directory")
	}
	directory, err := os.Lstat(dir)
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o022 != 0 {
		return errors.New("unsafe user settings directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.New("cannot open user settings directory")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(directory, opened) {
		return errPreferencesChanged
	}
	lockName := "." + name + ".lock"
	beforeLock, statErr := root.Lstat(lockName)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) || statErr == nil && !beforeLock.Mode().IsRegular() {
		return errors.New("unsafe user settings lock")
	}
	// Exclusive creation avoids Darwin openat races between creators; the
	// existing-file path never tries to create or truncate the lock.
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_RDWR|flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		lock, err = root.OpenFile(lockName, os.O_RDWR|flags, 0)
	}
	if err != nil {
		return errors.New("cannot open user settings lock")
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	afterLock, statErr := root.Lstat(lockName)
	if err != nil || !lockInfo.Mode().IsRegular() || statErr != nil || !afterLock.Mode().IsRegular() || !os.SameFile(lockInfo, afterLock) || beforeLock != nil && !os.SameFile(beforeLock, lockInfo) {
		return errors.New("unsafe user settings lock")
	}
	if err := lockPreferences(lock); err != nil {
		return err
	}
	old, oldInfo, err := readPreferenceDocument(root, name, flags)
	if err != nil {
		return err
	}
	fields := map[string]json.RawMessage{}
	if oldInfo != nil {
		if json.Unmarshal(old, &fields) != nil || fields == nil {
			return errors.New("user settings must contain a JSON object")
		}
	}
	for field, value := range updates {
		fields[field] = bytes.Clone(value)
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return errors.New("cannot encode user settings")
	}
	data = append(data, '\n')
	// Validate the generation fields before replacing any readable document.
	var candidate preferenceFile
	if json.Unmarshal(data, &candidate) != nil || candidate.generationPreferences.validate() != nil {
		return errors.New("invalid user preference value")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("cannot create user settings temporary name")
	}
	tempName := "." + name + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|flags, 0o600)
	if err != nil {
		return errors.New("cannot create user settings temporary file")
	}
	defer root.Remove(tempName)
	_, writeErr := temp.Write(data)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		return errors.New("cannot write user settings temporary file")
	}
	if w.beforeReplace != nil {
		w.beforeReplace()
	}
	current, currentInfo, err := readPreferenceDocument(root, name, flags)
	if err != nil {
		return err
	}
	if !bytes.Equal(old, current) || (oldInfo == nil) != (currentInfo == nil) || oldInfo != nil && !os.SameFile(oldInfo, currentInfo) {
		return errPreferencesChanged
	}
	currentDirectory, err := os.Lstat(dir)
	if err != nil || !os.SameFile(directory, currentDirectory) {
		return errPreferencesChanged
	}
	if err := root.Rename(tempName, name); err != nil {
		return errors.New("cannot replace user settings")
	}
	directoryFile, err := root.Open(".")
	if err != nil {
		return errors.New("user settings saved but directory sync failed")
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return errors.New("user settings saved but directory sync failed")
	}
	return nil
}

func readPreferenceDocument(root *os.Root, name string, flags int) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, nil, errors.New("user settings must be a regular file")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|flags, 0)
	if err != nil {
		return nil, nil, errors.New("cannot read user settings safely")
	}
	defer file.Close()
	info, err := file.Stat()
	after, statErr := root.Lstat(name)
	if err != nil || statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, info) || !os.SameFile(info, after) {
		return nil, nil, errPreferencesChanged
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, errors.New("cannot read user settings")
	}
	return data, info, nil
}

func preferenceJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("invalid internal preference: %v", err))
	}
	return data
}
