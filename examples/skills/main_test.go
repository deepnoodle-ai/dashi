package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/skill"
)

func TestFixtureFoldersLoadWithoutWarnings(t *testing.T) {
	set, warnings := skill.LoadDirs("skills", "commands")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v", warnings)
	}
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	catalog := bundle.Catalog()
	if !strings.Contains(catalog, "incident-review") ||
		strings.Contains(catalog, "summarize") {
		t.Fatalf("catalog = %q", catalog)
	}
}

func TestSlashUseExpandsCommandFile(t *testing.T) {
	set, _ := skill.LoadDirs("skills", "commands")
	prompt, ok := set.Expand("summarize", "INC-42")
	if !ok || prompt != "Give a three-line summary of incident INC-42." {
		t.Fatalf("prompt = %q, %v", prompt, ok)
	}
}
