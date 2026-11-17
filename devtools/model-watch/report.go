package main

import (
	"encoding/json"
	"io"
	"strings"
)

func printJSON(w io.Writer, reports []report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(reports)
}

// printText writes one section per provider. The groups that need action,
// new and missing, are listed one ID per line; the rest are compact.
func printText(w io.Writer, reports []report) error {
	out := &errWriter{w: w}
	for i, r := range reports {
		if i > 0 {
			out.printf("\n")
		}
		switch {
		case r.Skipped != "":
			out.printf("%s: skipped, %s\n", r.Provider, r.Skipped)
			continue
		case r.Error != "":
			out.printf("%s: error: %s\n", r.Provider, r.Error)
			continue
		}
		out.printf("%s: %d listed, %d in catalog\n", r.Provider, r.ListedCount, r.CatalogCount)
		long := func(title string, ids []string) {
			if len(ids) == 0 {
				return
			}
			out.printf("  %s (%d):\n", title, len(ids))
			for _, id := range ids {
				out.printf("    %s\n", id)
			}
		}
		short := func(title string, ids []string) {
			if len(ids) == 0 {
				return
			}
			out.printf("  %s (%d): %s\n", title, len(ids), strings.Join(ids, ", "))
		}
		long("new: listed, not in catalog", r.New)
		long("missing: in catalog, not listed", r.Missing)
		long("unchecked: not listed, and the direct check failed", r.Unchecked)
		if len(r.New) == 0 && len(r.Missing) == 0 && len(r.Unchecked) == 0 {
			out.printf("  no new or missing models\n")
		}
		short("resolved: unlisted aliases the API still serves", r.Resolved)
		short("snapshots of cataloged models", r.Snapshots)
		short("media and other", r.MediaAndOther)
		short("ignored", r.Ignored)
	}
	return out.err
}
