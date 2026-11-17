package main

import (
	"errors"
	"slices"
	"testing"
)

func TestCompareGroups(t *testing.T) {
	listed := []listedModel{
		{ID: "gpt-5.4"},                      // cataloged
		{ID: "gpt-5.4-2026-03-05"},           // snapshot of a cataloged alias
		{ID: "claude-sonnet-4-5-20250929"},   // a catalog entry's documented snapshot
		{ID: "gpt-9"},                        // new
		{ID: "gpt-image-2"},                  // media by name
		{ID: "mystery-model", NotText: true}, // media by provider metadata
		{ID: "mistral-large-2512", Aliases: []string{"mistral-large-latest"}}, // cataloged through an alias
		{ID: "secret-model"},     // ignored
		{ID: "gpt-7-2026-01-01"}, // dated, but its alias is not cataloged
	}
	cat := []catalogModel{
		{ID: "gpt-5.4"},
		{ID: "claude-sonnet-4-5", Snapshot: "claude-sonnet-4-5-20250929"},
		{ID: "mistral-large-latest"},
		{ID: "gone-model"},
		{ID: "alias-only"},
		{ID: "hidden-in-catalog"},
	}
	ignore := map[string]bool{"secret-model": true, "hidden-in-catalog": true}
	exists := func(id string) (bool, error) {
		if id == "gone-model" {
			return false, nil
		}
		return id == "alias-only", nil
	}

	r := compare("test", listed, cat, ignore, exists)

	check := func(name string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	check("New", r.New, []string{"gpt-7-2026-01-01", "gpt-9"})
	check("Missing", r.Missing, []string{"gone-model"})
	check("Snapshots", r.Snapshots, []string{"gpt-5.4-2026-03-05"})
	check("MediaAndOther", r.MediaAndOther, []string{"gpt-image-2", "mystery-model"})
	check("Resolved", r.Resolved, []string{"alias-only"})
	check("Ignored", r.Ignored, []string{"hidden-in-catalog", "secret-model"})
	if r.ListedCount != len(listed) || r.CatalogCount != len(cat) {
		t.Errorf("counts = %d, %d", r.ListedCount, r.CatalogCount)
	}
}

func TestCompareExistsErrorIsUnchecked(t *testing.T) {
	r := compare("test", nil, []catalogModel{{ID: "a"}}, nil,
		func(string) (bool, error) { return false, errors.New("HTTP 429") })
	if !slices.Equal(r.Unchecked, []string{"a: HTTP 429"}) || len(r.Missing) != 0 {
		t.Errorf("Unchecked = %q, Missing = %q", r.Unchecked, r.Missing)
	}
	if r.New == nil || r.Snapshots == nil {
		t.Error("empty groups should be empty slices, so JSON shows [] rather than null")
	}
}

func TestSnapshotBase(t *testing.T) {
	for id, want := range map[string]string{
		"gpt-5.4-2026-03-05":               "gpt-5.4",
		"claude-sonnet-4-5-20250929":       "claude-sonnet-4-5",
		"mistral-medium-2604":              "mistral-medium",
		"gemini-2.5-flash-preview-09-2025": "gemini-2.5-flash-preview",
		"gemini-2.0-flash-001":             "gemini-2.0-flash",
		"gpt-5.4":                          "",
		"claude-opus-4-5":                  "",
	} {
		if got := snapshotBase(id); got != want {
			t.Errorf("snapshotBase(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestIsNotText(t *testing.T) {
	for id, want := range map[string]bool{
		"gpt-4o-mini-tts":            true,
		"text-embedding-3-small":     true,
		"omni-moderation-latest":     true,
		"gpt-realtime-mini":          true,
		"dall-e-3":                   true,
		"mistral-ocr-latest":         true,
		"imagen-4.0-generate":        true,
		"grok-imagine-video":         true,
		"gemini-2.5-flash-image":     true,
		"gemini-omni-1.1-flash":      true,
		"gemini-omni-flash-preview":  true,
		"omni-moderation-2024-09-26": true,
		"gpt-4o":                     false, // "o" for omni, but not the word
		"gpt-4o-mini":                false,
		"o4-mini":                    false,
		"gpt-5.4":                    false,
		"claude-opus-4-5":            false,
		"computer-use-preview":       false,
		"gemini-3-pro-preview":       false,
		"codestral-latest":           false,
	} {
		if got := isNotText(listedModel{ID: id}); got != want {
			t.Errorf("isNotText(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestParseIgnore(t *testing.T) {
	set, err := parseIgnore(ignoreJSON)
	if err != nil {
		t.Fatalf("checked-in ignore.json: %v", err)
	}
	if !set["anthropic"]["claude-mythos-5-2"] {
		t.Error("ignore.json lost claude-mythos-5-2")
	}
	if _, err := parseIgnore([]byte(`[{"id":"x"}]`)); err == nil {
		t.Error("an entry without a provider should be an error")
	}
}
