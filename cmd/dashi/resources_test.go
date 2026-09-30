package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func putResource(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func instructionText(item resourceSnapshot) string {
	var text strings.Builder
	for _, c := range item.Instructions.Message.Content {
		if c.Reminder != nil {
			text.WriteString(c.Reminder.Content)
		}
	}
	return text.String()
}
func TestResourceInstructionsFallbackAndFailures(t *testing.T) {
	for _, scenario := range []string{"missing", "fallback", "preferred", "broken-link", "external-link", "internal-link", "oversize", "utf8", "directory", "exact-bound"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "AGENTS.md")
			if scenario != "missing" {
				putResource(t, filepath.Join(dir, "CLAUDE.md"), "fallback-body")
			}
			expected, warnings := "fallback-body", false
			switch scenario {
			case "missing":
				expected = "No workspace instructions selected."
			case "fallback":
			case "preferred":
				putResource(t, path, "preferred-body")
				expected = "preferred-body"
			case "broken-link":
				if err := os.Symlink("absent", path); err != nil {
					t.Skip(err)
				}
				warnings = true
			case "external-link":
				outside := filepath.Join(t.TempDir(), "outside.md")
				putResource(t, outside, "secret-outside")
				if err := os.Symlink(outside, path); err != nil {
					t.Skip(err)
				}
				warnings = true
			case "internal-link":
				putResource(t, filepath.Join(dir, "guide.md"), "internal-body")
				if err := os.Symlink("guide.md", path); err != nil {
					t.Skip(err)
				}
				expected = "internal-body"
			case "oversize":
				putResource(t, path, strings.Repeat("x", instructionLimit+1))
				warnings = true
			case "utf8":
				putResource(t, path, "bad\xff")
				warnings = true
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				warnings = true
			case "exact-bound":
				putResource(t, path, strings.Repeat("x", instructionLimit))
				expected = strings.Repeat("x", instructionLimit)
			}
			found, err := discoverResources(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			if warnings {
				expected = "No workspace instructions selected."
				if len(found.Warnings) == 0 {
					t.Fatal("missing warning")
				}
			}
			text := instructionText(found.Snapshot)
			if !strings.Contains(text, expected) || strings.Contains(text, "secret-outside") || (warnings && strings.Contains(text, "fallback-body")) {
				t.Fatalf("wrong instruction selection, len=%d", len(text))
			}
			raw, err := found.Snapshot.encode()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := decodeResourceSnapshot(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestResourceSnapshotPinsBodiesAndInvocationRestrictions(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	putResource(t, filepath.Join(dir, "AGENTS.md"), "old-instructions")
	project := filepath.Join(dir, ".dashi/skills/review/SKILL.md")
	putResource(t, project, "---\nname: review\ndescription: Review changes\n---\nold-skill-body")
	putResource(t, filepath.Join(home, ".dashi/skills/review/SKILL.md"), "---\nname: REVIEW\n---\nshadow-body")
	putResource(t, filepath.Join(dir, ".dashi/commands/private.md"), "---\nname: private\ndisable-model-invocation: true\n---\nprivate-user-body $ARGUMENTS")
	found, err := discoverResources(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Candidates) != 3 || len(found.Snapshot.Skills) != 2 || len(found.Snapshot.Definitions) != 1 {
		t.Fatalf("selected metadata=%+v", found.Snapshot)
	}
	raw, err := found.Snapshot.encode()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-user-body")) || bytes.Contains(raw, []byte("shadow-body")) {
		t.Fatal("unselected bodies persisted")
	}
	putResource(t, project, "---\nname: review\ndescription: Review changes\n---\nnew-skill-body")
	putResource(t, filepath.Join(dir, "AGENTS.md"), "new-instructions")
	snapshot, bundle, err := decodeResourceSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructionText(snapshot), "old-instructions") || bundle.Definitions()[0].Instructions != "old-skill-body" {
		t.Fatal("recovery rediscovered disk")
	}
	newer, err := discoverResources(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Bundle.Digest() == bundle.Digest() || !strings.Contains(instructionText(newer.Snapshot), "new-instructions") {
		t.Fatal("refresh reused stale bytes")
	}
}
func TestResourceSnapshotRejectsMalformedRecovery(t *testing.T) {
	found, err := discoverResources(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := found.Snapshot.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeResourceSnapshot(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	found.Snapshot.Instructions.Message.Content[0].Reminder.Tier = llm.ReminderOperator
	unsafe, _ := json.Marshal(found.Snapshot)
	if _, _, err := decodeResourceSnapshot(unsafe); err == nil {
		t.Fatal("operator instruction accepted")
	}
}
