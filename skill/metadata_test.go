package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryRetainsSelectedAndShadowedSources(t *testing.T) {
	root := t.TempDir()
	project, home := filepath.Join(root, "project"), filepath.Join(root, "home")
	for path, body := range map[string]string{
		filepath.Join(project, "a", "SKILL.md"): "---\nname: review\ndescription: project winner\n---\nproject body",
		filepath.Join(project, "b", "SKILL.md"): "---\nname: REVIEW\n---\nproject shadow",
		filepath.Join(home, "review.md"):        "home shadow",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	set, warnings := LoadDirs(project, home)
	if len(warnings) != 1 {
		t.Fatalf("warnings=%v", warnings)
	}
	candidates := set.Candidates()
	if len(candidates) != 3 || candidates[0].Description != "project winner" || candidates[1].Name != "REVIEW" || candidates[2].ModelInvocable {
		t.Fatalf("candidates=%+v", candidates)
	}
	if candidates[0].Path != filepath.Join(project, "a", "SKILL.md") || candidates[2].Path != filepath.Join(home, "review.md") {
		t.Fatalf("paths=%+v", candidates)
	}
	bundle, err := set.Bundle(Options{})
	if err != nil {
		t.Fatal(err)
	}
	definitions := bundle.Definitions()
	if len(definitions) != 1 || definitions[0].Instructions != "project body" {
		t.Fatalf("definitions=%+v", definitions)
	}
	digest := bundle.Digest()
	definitions[0].Instructions = "caller replacement"
	candidates[0].Name = "caller replacement"
	if bundle.Definitions()[0].Instructions != "project body" || set.Candidates()[0].Name != "review" || bundle.Digest() != digest {
		t.Fatal("caller mutation changed frozen resources")
	}
}
