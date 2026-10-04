package main

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/deepnoodle-ai/wonton/color"
)

// copySummary is what one copy run did.
type copySummary struct {
	RunID           string
	OriginalsSeen   int
	OriginalsCopied int // first copies, rewrites, and new copies after a supersede
	Superseded      int
	ModifiedAt      int // modified_at_updated
	Gone            int
	Warnings        []string
}

// runCopy copies new and changed originals into the archive and updates
// the database. Problems with single originals become warnings; only a
// failure to write the copy log or database stops the run, which then has
// no run_ended event.
func runCopy(o options) (copySummary, error) {
	a, err := openArchive(o.ArchiveDir)
	if err != nil {
		return copySummary{}, err
	}
	defer a.Close()
	sum := copySummary{RunID: a.RunID}
	if err := a.record(logEvent{Event: eventRunStarted}); err != nil {
		return sum, err
	}

	apps := []struct{ app, dir string }{{appClaudeCode, o.ClaudeDir}, {appCodex, o.CodexDir}}
	lists := map[string]listResult{}
	for _, s := range apps {
		l := listOriginals(s.app, s.dir)
		lists[s.app] = l
		a.Warnings = append(a.Warnings, l.Warnings...)
	}
	if err := a.deleteTempFiles(); err != nil {
		return sum, err
	}
	current, err := currentCopies(a.DB)
	if err != nil {
		return sum, err
	}

	for _, s := range apps {
		l := lists[s.app]
		seen := map[string]bool{}
		for _, org := range l.Originals {
			sum.OriginalsSeen++
			seen[org.CopyPath] = true
			var prev *copyRow
			if c, ok := current[org.CopyPath]; ok {
				prev = &c
			}
			outcome, err := a.copyOriginal(org, prev)
			if fe := (fatalError{}); errors.As(err, &fe) {
				return sum, fe.err
			}
			if err != nil {
				a.warn("copy %s: %v", org.Path, err)
				continue
			}
			switch outcome {
			case copyOutcomeModifiedAtUpdated:
				sum.ModifiedAt++
			case copyOutcomeWritten, copyOutcomeRewritten:
				sum.OriginalsCopied++
			case copyOutcomeSuperseded:
				sum.OriginalsCopied++
				sum.Superseded++
			}
		}
		if !l.Complete {
			a.warn("%s: not every original could be listed; no copy marked original_gone this run", s.app)
			continue
		}
		gone, err := a.markGone(s.app, seen)
		if err != nil {
			return sum, err
		}
		sum.Gone += gone
	}

	parsed, err := parsePending(a.DB, a.Dir)
	if err != nil {
		return sum, err
	}
	a.Warnings = append(a.Warnings, parsed.Warnings...)
	warnings := append([]string{}, a.Warnings...)
	seenN, copiedN := sum.OriginalsSeen, sum.OriginalsCopied
	if err := a.record(logEvent{
		Event: eventRunEnded, OriginalsSeen: &seenN, OriginalsCopied: &copiedN, Warnings: &warnings,
	}); err != nil {
		return sum, err
	}
	sum.Warnings = warnings
	return sum, nil
}

func printCopySummary(w io.Writer, s copySummary) {
	fmt.Fprintf(w, "run %s\n", s.RunID)
	fmt.Fprintf(w, "originals seen:   %d\n", s.OriginalsSeen)
	fmt.Fprintf(w, "originals copied: %d (%d superseded an older copy)\n", s.OriginalsCopied, s.Superseded)
	fmt.Fprintf(w, "modified time only: %d\n", s.ModifiedAt)
	fmt.Fprintf(w, "originals gone:   %d\n", s.Gone)
	if len(s.Warnings) > 0 {
		fmt.Fprintln(w, color.Yellow.Apply(fmt.Sprintf("warnings (%d):", len(s.Warnings))))
		ws := append([]string{}, s.Warnings...)
		sort.Strings(ws)
		for _, x := range ws {
			fmt.Fprintf(w, "  %s\n", x)
		}
	}
}
