package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// report is the comparison for one provider. The JSON keys match the field
// names.
type report struct {
	Provider string `json:"provider"`
	// Skipped says why the provider was not checked, such as a missing key.
	Skipped string `json:"skipped,omitempty"`
	// Error is the reason listing failed.
	Error string `json:"error,omitempty"`

	ListedCount  int `json:"listed_count"`
	CatalogCount int `json:"catalog_count"`

	// New are listed text models the catalog does not have.
	New []string `json:"new"`
	// Missing are catalog models the provider did not list (and, for
	// Anthropic, did not resolve when asked directly).
	Missing []string `json:"missing"`
	// Snapshots are listed dated IDs whose undated alias is cataloged.
	Snapshots []string `json:"snapshots"`
	// MediaAndOther are listed models that do not generate text, such as
	// image, audio, and embedding models.
	MediaAndOther []string `json:"media_and_other"`
	// Resolved are catalog models absent from the list that the provider
	// still resolves when asked directly (Anthropic aliases).
	Resolved []string `json:"resolved"`
	// Unchecked are catalog models absent from the list whose direct check
	// failed, as "id: error"; they may still be served.
	Unchecked []string `json:"unchecked"`
	// Ignored are IDs in ignore.json, in either direction.
	Ignored []string `json:"ignored"`
}

// catalogModel is the part of a catalog entry the comparison uses.
type catalogModel struct {
	ID       string
	Snapshot string
}

//go:embed ignore.json
var ignoreJSON []byte

// ignoreEntry is one deliberate skip in ignore.json.
type ignoreEntry struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Reason   string `json:"reason"`
}

// ignoreSet maps provider to the IDs to ignore.
type ignoreSet map[string]map[string]bool

func parseIgnore(data []byte) (ignoreSet, error) {
	var entries []ignoreEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("ignore.json: %w", err)
	}
	set := ignoreSet{}
	for _, e := range entries {
		if e.Provider == "" || e.ID == "" {
			return nil, fmt.Errorf("ignore.json: entry %+v needs provider and id", e)
		}
		if set[e.Provider] == nil {
			set[e.Provider] = map[string]bool{}
		}
		set[e.Provider][e.ID] = true
	}
	return set, nil
}

// notTextPattern matches IDs of models that do not generate text: image,
// video, speech, embedding, moderation, and similar. It is a name heuristic;
// a false match only moves a model to the compact "media and other" group.
var notTextPattern = regexp.MustCompile(`(?i)(^|[-_.])(` + strings.Join([]string{
	"image", "images", "imagen", "dall-e", "gpt-image", "sora", "video", "veo",
	"tts", "transcribe", "whisper", "speech", "audio", "realtime", "live", "voxtral",
	"lyria", "embed", "embedding", "embeddings", "moderation", "ocr", "aqa",
	"babbage", "davinci", "imagine", "nano-banana", "sam",
	"omni", // Google's gemini-omni video models; OpenAI's omni-moderation is media anyway

}, "|") + `)([-_.]|\d|$)`)

func isNotText(m listedModel) bool {
	return m.NotText || notTextPattern.MatchString(m.ID)
}

// datedSuffix matches the snapshot suffixes providers append to an alias:
// -2026-03-05, -20250929, -2604, -09-2025, and -001.
var datedSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8}|\d{4}|\d{2}-\d{4}|\d{3})$`)

// snapshotBase returns id without its dated suffix, or "" when it has none.
func snapshotBase(id string) string {
	loc := datedSuffix.FindStringIndex(id)
	if loc == nil {
		return ""
	}
	return id[:loc[0]]
}

// compare sorts a provider's listed models against its catalog entries.
// exists, when not nil, checks a catalog ID the list omitted.
func compare(provider string, listed []listedModel, cat []catalogModel, ignore map[string]bool,
	exists func(id string) (bool, error)) report {
	r := report{Provider: provider, ListedCount: len(listed), CatalogCount: len(cat)}

	known := map[string]bool{}
	for _, c := range cat {
		known[c.ID] = true
		if c.Snapshot != "" {
			known[c.Snapshot] = true
		}
	}
	listedIDs := map[string]bool{}
	for _, m := range listed {
		listedIDs[m.ID] = true
		for _, a := range m.Aliases {
			listedIDs[a] = true
		}
	}

	for _, m := range listed {
		switch {
		case known[m.ID] || slices.ContainsFunc(m.Aliases, func(a string) bool { return known[a] }):
			// Cataloged under this ID or one of its aliases.
		case ignore[m.ID]:
			r.Ignored = append(r.Ignored, m.ID)
		case isNotText(m):
			r.MediaAndOther = append(r.MediaAndOther, m.ID)
		case snapshotBase(m.ID) != "" && (known[snapshotBase(m.ID)] || known[snapshotBase(m.ID)+"-latest"]):
			r.Snapshots = append(r.Snapshots, m.ID)
		default:
			r.New = append(r.New, m.ID)
		}
	}

	for _, c := range cat {
		switch {
		case listedIDs[c.ID] || (c.Snapshot != "" && listedIDs[c.Snapshot]):
		case ignore[c.ID]:
			r.Ignored = append(r.Ignored, c.ID)
		default:
			if exists != nil {
				ok, err := exists(c.ID)
				if err != nil {
					r.Unchecked = append(r.Unchecked, c.ID+": "+err.Error())
					continue
				}
				if ok {
					r.Resolved = append(r.Resolved, c.ID)
					continue
				}
			}
			r.Missing = append(r.Missing, c.ID)
		}
	}

	for _, s := range []*[]string{&r.New, &r.Missing, &r.Snapshots, &r.MediaAndOther, &r.Resolved, &r.Unchecked, &r.Ignored} {
		slices.Sort(*s)
		*s = slices.Compact(*s)
		if *s == nil {
			*s = []string{}
		}
	}
	return r
}
