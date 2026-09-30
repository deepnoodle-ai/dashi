package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func preferenceWriterForTest(t *testing.T) userPreferenceWriter {
	t.Helper()
	if _, err := preferenceFileFlags(); err != nil {
		t.Skip(err)
	}
	return userPreferenceWriter{path: filepath.Join(t.TempDir(), ".dashi", "settings.json")}
}
func writePreferenceFixture(t *testing.T, w userPreferenceWriter, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUserPreferenceWritePreservesOtherOwnersAndExplicitOmissions(t *testing.T) {
	w := preferenceWriterForTest(t)
	original := `{"model":"old","show_thinking":true,"resources":{"skill_dirs":["custom"]},"mcp":{"servers":{"fixture":{"future":42}}},"unknown":9007199254740993}`
	writePreferenceFixture(t, w, original)
	updates := map[string]json.RawMessage{"provider": preferenceJSON("custom"), "model": preferenceJSON("exact/id"), "show_thinking": preferenceJSON(false), "temperature": json.RawMessage("null"), "max_tokens": preferenceJSON(0)}
	if err := w.update(updates); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var old map[string]json.RawMessage
	json.Unmarshal([]byte(original), &old)
	for _, field := range []string{"resources", "mcp", "unknown"} {
		var a, b any
		decoderA := json.NewDecoder(strings.NewReader(string(old[field])))
		decoderA.UseNumber()
		decoderA.Decode(&a)
		decoderB := json.NewDecoder(strings.NewReader(string(got[field])))
		decoderB.UseNumber()
		decoderB.Decode(&b)
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if string(aa) != string(bb) {
			t.Fatalf("%s changed: %s", field, got[field])
		}
	}
	if string(got["show_thinking"]) != "false" || string(got["temperature"]) != "null" || string(got["max_tokens"]) != "0" {
		t.Fatalf("explicit controls lost: %s", data)
	}
	info, _ := os.Stat(w.path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("user preferences are not owner-only")
	}
}

func TestUserPreferenceWriteRefusesUnreadableOrUnsafeTargets(t *testing.T) {
	for _, kind := range []string{"corrupt", "array", "null", "symlink", "relative-symlink", "symlink-lock", "symlink-directory", "unsafe-directory", "invalid-update", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			w := preferenceWriterForTest(t)
			writePreferenceFixture(t, w, `{"model":"original"}`)
			updates := map[string]json.RawMessage{"model": preferenceJSON("new")}
			switch kind {
			case "corrupt":
				os.WriteFile(w.path, []byte("broken"), 0o600)
			case "array":
				os.WriteFile(w.path, []byte("[]"), 0o600)
			case "null":
				os.WriteFile(w.path, []byte("null"), 0o600)
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				os.WriteFile(target, []byte(`{"model":"SECRET"}`), 0o600)
				os.Remove(w.path)
				os.Symlink(target, w.path)
			case "relative-symlink":
				target := filepath.Join(filepath.Dir(w.path), "other.json")
				os.WriteFile(target, []byte(`{"model":"SECRET"}`), 0o600)
				os.Remove(w.path)
				os.Symlink("other.json", w.path)
			case "symlink-lock":
				target := filepath.Join(filepath.Dir(w.path), "other.json")
				os.WriteFile(target, []byte(`{"model":"SECRET"}`), 0o600)
				os.Symlink("other.json", filepath.Join(filepath.Dir(w.path), ".settings.json.lock"))
			case "symlink-directory":
				oldDir := filepath.Dir(w.path)
				moved := oldDir + "-moved"
				os.Rename(oldDir, moved)
				os.Symlink(moved, oldDir)
			case "unsafe-directory":
				os.Chmod(filepath.Dir(w.path), 0o777)
			case "invalid-update":
				updates = map[string]json.RawMessage{"max_tokens": preferenceJSON(-1)}
			case "unknown-field":
				updates = map[string]json.RawMessage{"api_key": preferenceJSON("SECRET")}
			}
			before, _ := os.ReadFile(w.path)
			err := w.update(updates)
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe write: %v", err)
			}
			after, _ := os.ReadFile(w.path)
			if string(before) != string(after) {
				t.Fatal("rejected write changed target")
			}
		})
	}
}

func TestUserPreferenceWriteDetectsExternalReplacement(t *testing.T) {
	for _, kind := range []string{"contents", "inode", "directory"} {
		t.Run(kind, func(t *testing.T) {
			w := preferenceWriterForTest(t)
			writePreferenceFixture(t, w, `{"model":"old"}`)
			w.beforeReplace = func() {
				switch kind {
				case "contents":
					os.WriteFile(w.path, []byte(`{"external":true}`), 0o600)
				case "inode":
					replacement := w.path + ".replacement"
					os.WriteFile(replacement, []byte(`{"model":"old"}`), 0o600)
					os.Rename(replacement, w.path)
				case "directory":
					dir := filepath.Dir(w.path)
					os.Rename(dir, dir+"-old")
					os.Mkdir(dir, 0o700)
					os.WriteFile(w.path, []byte(`{"external":true}`), 0o600)
				}
			}
			err := w.update(map[string]json.RawMessage{"model": preferenceJSON("new")})
			if !errors.Is(err, errPreferencesChanged) {
				t.Fatalf("collision=%v", err)
			}
			data, _ := os.ReadFile(w.path)
			if strings.Contains(string(data), "new") {
				t.Fatal("overwrote external editor")
			}
			entries, _ := os.ReadDir(filepath.Dir(w.path))
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") {
					t.Fatal("temporary file left behind")
				}
			}
		})
	}
}

func TestCooperatingPreferenceWritersRereadUnderLock(t *testing.T) {
	w := preferenceWriterForTest(t)
	writePreferenceFixture(t, w, `{"unknown":{"owner":"resources"}}`)
	updates := map[string]json.RawMessage{"provider": preferenceJSON("custom"), "model": preferenceJSON("exact"), "thinking_effort": preferenceJSON("low"), "show_thinking": preferenceJSON(false), "temperature": preferenceJSON(0), "max_tokens": preferenceJSON(200)}
	var group sync.WaitGroup
	for field, value := range updates {
		group.Go(func() {
			deadline := time.Now().Add(3 * time.Second)
			for {
				err := w.update(map[string]json.RawMessage{field: value})
				if err == nil {
					return
				}
				if !strings.Contains(err.Error(), "busy") || time.Now().After(deadline) {
					t.Errorf("%s: %v", field, err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	group.Wait()
	data, _ := os.ReadFile(w.path)
	var got map[string]json.RawMessage
	json.Unmarshal(data, &got)
	if got["unknown"] == nil {
		t.Fatal("other owner erased")
	}
	for field, value := range updates {
		if string(got[field]) != string(value) {
			t.Fatalf("lost update %s: %s", field, data)
		}
	}
}
