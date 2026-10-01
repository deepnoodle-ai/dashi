package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

const inspectionMetadataLimit = 1 << 20
const searchLimit = 50

var errInspectionMetadataTooLarge = errors.New("session: metadata exceeds 1 MiB")

// Problem names an unreadable part of a recorded snapshot. Messages never
// contain raw decoder errors or content from metadata of an unknown workspace.
type Problem struct{ SessionID, TurnID, Message string }

// MessagePreview identifies ordinary recorded text, never a projected history.
type MessagePreview struct {
	TurnID   string
	Revision int64
	Role     llm.Role
	Text     string
}

type Destination struct{ Provider, API, Model, Revision string }
type PendingObservation struct {
	Kind         dashi.PendingKind
	ID           string
	ToolStatus   dashi.ToolStatus
	UnsafeEffect bool
}

// Execution describes the latest indexed Turn, even if its record is unreadable.
// Stop.Inferred distinguishes replay's inference from a recorded run stop.
// It says nothing about a live worker or permission to execute a command.
type Execution struct {
	TurnID          string
	Revision        int64
	Readable        bool
	Summary         bool
	Destination     Destination
	Stop            dashi.Stop
	Pending         []PendingObservation
	Accounting      Accounting
	Context         ContextOccupancy
	Next            dashi.Next
	ReadyToContinue bool
}

// Inspection contains one read-only scan through the captured indexed boundary.
// A concurrently running worker can append facts after any loaded revision.
type Inspection struct {
	Metadata                                  Session
	IndexedTurns, ReadableTurns, OmittedTurns int
	RecordUnavailable                         bool
	Request                                   *MessagePreview
	RequestIncomplete                         bool
	Latest                                    *Execution
	Accounting                                Accounting
	Problems                                  []Problem
	NextDestination                           Destination
	NextDestinationUnavailable                bool
}

type SearchMatch struct {
	// Card is built in the same scan as Match. Rendering requires no reread.
	Card  Inspection
	Match *MessagePreview
}
type SearchResult struct {
	Matches   []SearchMatch
	Problems  []Problem
	Truncated bool
	// Sessions counts valid workspace metadata encountered, not total matches.
	Sessions                int
	TextMatchesMayBeMissing bool
}

// Inspect validates workspace ownership before reading any conversation text.
// It neither obtains an execution lease nor constructs a writable recorder.
func (st *Store) Inspect(ctx context.Context, selectedWorkspace, id string) (Inspection, error) {
	var empty Inspection
	if !validAttachmentSession(id) {
		return empty, errors.New("session: invalid session ID")
	}
	selected, err := workspace.New(selectedWorkspace)
	if err != nil {
		return empty, errors.New("session: selected workspace is unavailable")
	}
	anchor, err := openAttachmentAnchor(st.root)
	if errors.Is(err, os.ErrNotExist) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, errors.New("session: cannot open session storage safely")
	}
	defer anchor.close()
	metadata, dir, err := scopedMetadata(ctx, anchor, selected.Dir(), id)
	if err != nil {
		return empty, err
	}
	defer dir.close()
	result, err := scanInspection(ctx, dir, metadata, "")
	return result.Inspection, err
}

// LoadScoped reads only safe metadata before reopening. It does not inspect or
// resolve conversation content and never grants a session lease.
func (st *Store) LoadScoped(ctx context.Context, selectedWorkspace, id string) (*Session, error) {
	if !validAttachmentSession(id) {
		return nil, errors.New("session: invalid session ID")
	}
	selected, err := workspace.New(selectedWorkspace)
	if err != nil {
		return nil, errors.New("session: selected workspace is unavailable")
	}
	anchor, err := openAttachmentAnchor(st.root)
	if err != nil {
		return nil, errors.New("session: saved session unavailable")
	}
	defer anchor.close()
	metadata, dir, err := scopedMetadata(ctx, anchor, selected.Dir(), id)
	if err != nil {
		return nil, err
	}
	dir.close()
	return &metadata, nil
}

func scopedMetadata(ctx context.Context, anchor *attachmentDir, selected, id string) (Session, *attachmentDir, error) {
	var metadata Session
	if err := ctx.Err(); err != nil {
		return metadata, nil, err
	}
	dir, err := anchor.child(id)
	if errors.Is(err, os.ErrNotExist) {
		return metadata, nil, ErrNotFound
	}
	if err != nil {
		return metadata, nil, errors.New("session: cannot open session directory safely")
	}
	data, err := readInspectionMetadata(ctx, dir)
	if err == nil {
		err = json.Unmarshal(data, &metadata)
	}
	if err == nil && (metadata.ID != id || !validAttachmentSession(metadata.ID) || !filepath.IsAbs(metadata.Dir)) {
		err = errors.New("invalid metadata identity")
	}
	if err != nil {
		dir.close()
		if ctx.Err() != nil {
			return Session{}, nil, ctx.Err()
		}
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, nil, ErrNotFound
		}
		if errors.Is(err, errInspectionMetadataTooLarge) {
			return Session{}, nil, errInspectionMetadataTooLarge
		}
		return Session{}, nil, errors.New("session: metadata is unavailable or invalid")
	}
	// CLI stores the canonical workspace at launch. Do not follow a recorded
	// alias that could be retargeted to grant access to another workspace.
	if filepath.Clean(metadata.Dir) != selected {
		dir.close()
		return Session{}, nil, ErrNotFound
	}
	return metadata, dir, nil
}

func readInspectionMetadata(ctx context.Context, dir *attachmentDir) ([]byte, error) {
	file, err := dir.regular("session.json", false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var data []byte
	buf := make([]byte, 32<<10)
	for len(data) <= inspectionMetadataLimit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buf[:min(len(buf), inspectionMetadataLimit+1-len(data))])
		data = append(data, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if len(data) > inspectionMetadataLimit {
		return nil, errInspectionMetadataTooLarge
	}
	return data, ctx.Err()
}

// Search scans ordinary recorded text once per candidate. Card facts and text
// evidence share the same loaded revisions. It does not resolve content refs.
func (st *Store) Search(ctx context.Context, selectedWorkspace, query string) (SearchResult, error) {
	var out SearchResult
	selected, err := workspace.New(selectedWorkspace)
	if err != nil {
		return out, errors.New("session: selected workspace is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	anchor, err := openAttachmentAnchor(st.root)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, errors.New("session: cannot open session storage safely")
	}
	defer anchor.close()
	type candidate struct {
		metadata  Session
		directory os.FileInfo
	}
	var candidates []candidate
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		entries, err := anchor.file.ReadDir(64)
		if err != nil && err != io.EOF {
			return out, errors.New("session: cannot list session storage")
		}
		for _, entry := range entries {
			id := entry.Name()
			if !validAttachmentSession(id) {
				continue
			}
			metadata, dir, readErr := scopedMetadata(ctx, anchor, selected.Dir(), id)
			if readErr != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				if !errors.Is(readErr, ErrNotFound) {
					out.Problems = append(out.Problems, Problem{id, "", readErr.Error() + "; session excluded"})
					out.TextMatchesMayBeMissing = true
				}
				continue
			}
			info, statErr := dir.file.Stat()
			dir.close()
			if statErr != nil {
				out.Problems = append(out.Problems, Problem{id, "", "session directory unavailable"})
				out.TextMatchesMayBeMissing = true
				continue
			}
			candidates = append(candidates, candidate{metadata, info})
		}
		if err == io.EOF {
			break
		}
	}
	out.Sessions = len(candidates)
	slices.SortFunc(candidates, func(a, b candidate) int {
		if order := b.metadata.Updated.Compare(a.metadata.Updated); order != 0 {
			return order
		}
		return strings.Compare(a.metadata.ID, b.metadata.ID)
	})
	query = strings.ToLower(strings.TrimSpace(query))
	for _, candidate := range candidates {
		metadata := candidate.metadata
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// Bind the candidate's directory without reloading metadata. The listed
		// metadata belongs to this report's snapshot; the directory ID is fixed.
		dir, err := anchor.child(metadata.ID)
		if err == nil {
			info, statErr := dir.file.Stat()
			if statErr != nil || !os.SameFile(candidate.directory, info) {
				dir.close()
				err = errors.New("session directory changed after metadata read")
			}
		}
		if err != nil {
			out.Problems = append(out.Problems, Problem{metadata.ID, "", "session records unavailable"})
			out.TextMatchesMayBeMissing = true
			if query == "" || textMatches(metadata.ID, query) || textMatches(metadata.Title, query) {
				card := Inspection{Metadata: metadata, RecordUnavailable: true, RequestIncomplete: true, Accounting: Accounting{Incomplete: true}}
				card.NextDestination, card.NextDestinationUnavailable = savedDestination(metadata)
				out.Matches = append(out.Matches, SearchMatch{Card: card})
			}
		} else {
			card, scanErr := scanInspection(ctx, dir, metadata, query)
			dir.close()
			if scanErr != nil {
				return out, scanErr
			}
			out.Problems = append(out.Problems, card.Problems...)
			if card.RecordUnavailable || card.OmittedTurns > 0 {
				out.TextMatchesMayBeMissing = true
			}
			match := card.match
			card.match = nil
			if query == "" || textMatches(metadata.ID, query) || textMatches(metadata.Title, query) || match != nil {
				out.Matches = append(out.Matches, SearchMatch{card.Inspection, match})
			}
		}
		if len(out.Matches) > searchLimit {
			out.Matches = out.Matches[:searchLimit]
			out.Truncated = true
			break
		}
	}
	return out, nil
}

// scanResult's private match accumulator is discarded before publication.
type scanResult struct {
	Inspection
	match *MessagePreview
}

func scanInspection(ctx context.Context, dir *attachmentDir, metadata Session, query string) (scanResult, error) {
	out := scanResult{Inspection: Inspection{Metadata: metadata}}
	out.NextDestination, out.NextDestinationUnavailable = savedDestination(metadata)
	// The descriptor alias binds OpenReader to the already checked session,
	// even if its name is renamed or swapped while this scan is running.
	reader, err := filerecorder.OpenReader(filepath.Join(dir.root.Name(), "turns"))
	if err != nil {
		out.RecordUnavailable = true
		out.RequestIncomplete = true
		out.Accounting.Incomplete = true
		out.Problems = append(out.Problems, Problem{metadata.ID, "", "Turn storage unavailable"})
		return out, ctx.Err()
	}
	defer reader.Close()
	highest, err := reader.LastTurn(ctx, metadata.ID)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.RecordUnavailable = true
		out.RequestIncomplete = true
		out.Accounting.Incomplete = true
		out.Problems = append(out.Problems, Problem{metadata.ID, "", "Turn index unavailable"})
		return out, nil
	}
	out.IndexedTurns = highest
	if highest > 0 {
		out.Latest = &Execution{TurnID: session.TurnID(metadata.ID, highest)}
	}
	for offset := range highest {
		number := offset + 1
		if err := ctx.Err(); err != nil {
			return out, err
		}
		id := session.TurnID(metadata.ID, number)
		turn, loadErr := reader.Load(ctx, id)
		var view dashi.TurnView
		if loadErr == nil {
			view, loadErr = dashi.Replay(turn)
		}
		if loadErr != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.OmittedTurns++
			out.RequestIncomplete = true
			out.Problems = append(out.Problems, Problem{metadata.ID, id, "Turn record unreadable; facts omitted"})
			continue
		}
		out.ReadableTurns++
		accounting := turnAccounting(turn, view)
		out.Accounting = out.Accounting.add(accounting)
		if number == highest {
			execution := executionSnapshot(turn, view, accounting)
			out.Latest = &execution
		}
		if view.Origin == dashi.StartCompaction {
			continue
		}
		out.Request = &MessagePreview{TurnID: id, Revision: view.Revision, Role: llm.RoleUser, Text: "[request has no public text]"}
		out.RequestIncomplete = false
		for _, message := range ordinaryText(turn) {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			preview := MessagePreview{id, view.Revision, message.Role, previewText(message.Text, "")}
			if message.initial && message.Role == llm.RoleUser {
				out.Request = &preview
				out.RequestIncomplete = false
			}
			if query != "" && out.match == nil && textMatches(message.Text, query) {
				preview.Text = previewText(message.Text, query)
				out.match = &preview
			}
		}
	}
	out.Accounting.OmittedTurns = out.OmittedTurns
	out.Accounting.Incomplete = out.Accounting.Incomplete || out.OmittedTurns > 0
	return out, nil
}

func savedDestination(metadata Session) (Destination, bool) {
	if len(metadata.NextTurn) == 0 {
		return Destination{Model: metadata.Model}, false
	}
	var fields struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if json.Unmarshal(metadata.NextTurn, &fields) != nil {
		return Destination{}, true
	}
	return Destination{Provider: fields.Provider, Model: fields.Model}, false
}

func textMatches(text, query string) bool { return strings.Contains(strings.ToLower(text), query) }

func previewText(text, query string) string {
	runes := []rune(text)
	start := 0
	if query != "" {
		lower := strings.ToLower(text)
		if offset := strings.Index(lower, query); offset >= 0 {
			start = max(0, len([]rune(lower[:offset]))-40)
		}
	}
	const limit = 160
	end := min(len(runes), start+limit)
	result := string(runes[start:end])
	if start > 0 {
		result = "…" + result
	}
	if end < len(runes) {
		result += "…"
	}
	return workspace.PrintableText(result)
}
