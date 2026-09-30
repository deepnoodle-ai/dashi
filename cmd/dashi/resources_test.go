package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func instructionText(item *discoveredResources) string {
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
			text := instructionText(found)
			if !strings.Contains(text, expected) || strings.Contains(text, "secret-outside") || (warnings && strings.Contains(text, "fallback-body")) {
				t.Fatalf("wrong instruction selection, len=%d", len(text))
			}

		})
	}
}
func TestResourceDiscoveryRefreshesAndHonorsInvocationRestrictions(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	putResource(t, filepath.Join(dir, "AGENTS.md"), "old-instructions")
	project := fixtureResourceSkill(t, dir, "review", "old-skill-body", "")
	fixtureResourceSkill(t, home, "REVIEW", "shadow-body", "")
	putResource(t, filepath.Join(dir, ".dashi/commands/private.md"), "---\nname: private\ndisable-model-invocation: true\n---\nprivate-user-body $ARGUMENTS")
	found, err := discoverResources(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Candidates) != 3 || len(found.Skills) != 2 || modelSkillCount(found) != 1 {
		t.Fatalf("selected metadata=%+v", found.Skills)
	}
	putResource(t, project, "---\nname: review\ndescription: Fixture skill\n---\nnew-skill-body")
	putResource(t, filepath.Join(dir, "AGENTS.md"), "new-instructions")
	newer, err := discoverResources(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructionText(newer), "new-instructions") {
		t.Fatal("refresh reused stale instructions")
	}
	expanded, ok := newer.Set.Expand("review", "")
	if !ok || !strings.Contains(expanded, "new-skill-body") {
		t.Fatalf("refresh=%q/%v", expanded, ok)
	}
}
