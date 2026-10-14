package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Copy log events (decision 6).
const (
	eventRunStarted        = "run_started"
	eventRunEnded          = "run_ended"
	eventCopyWritten       = "copy_written"
	eventCopySuperseded    = "copy_superseded"
	eventModifiedAtUpdated = "modified_at_updated"
	eventOriginalGone      = "original_gone"
	eventOriginalReturned  = "original_returned"
)

// copyLogName is the append-only log beside the copies. It holds every
// fact about an original or a run that the copies themselves can't.
const copyLogName = "copy-log.jsonl"

// logEvent is one line of the copy log. Which fields are set depends on
// Event; numbers are pointers so a real zero is still written.
type logEvent struct {
	Event string    `json:"event"`
	Time  time.Time `json:"time"`
	RunID string    `json:"run_id"`

	// copy_written, copy_superseded, modified_at_updated, original_gone,
	// original_returned
	CopyPath string `json:"copy_path,omitempty"`
	// copy_superseded: the copy's path after the rename.
	SupersededPath string `json:"superseded_path,omitempty"`

	// copy_written
	OriginalPath  string     `json:"original_path,omitempty"`
	App           string     `json:"app,omitempty"`
	Kind          string     `json:"kind,omitempty"`
	OriginalBytes *int64     `json:"original_bytes,omitempty"`
	ModifiedAt    *time.Time `json:"modified_at,omitempty"` // also modified_at_updated
	Hash          string     `json:"hash,omitempty"`

	// run_ended
	OriginalsSeen   *int      `json:"originals_seen,omitempty"`
	OriginalsCopied *int      `json:"originals_copied,omitempty"`
	Warnings        *[]string `json:"warnings,omitempty"`
}

// loggedEvent is an event read from the log with the byte offset just past
// its line, which is the log position once it is applied.
type loggedEvent struct {
	logEvent
	End int64
}

// readLog reads the events that start at byte offset from. It returns the
// offset where the valid log ends: a last line with no newline or that is
// not valid JSON was torn by a crash mid-append and is not included. A bad
// line anywhere else is an error.
func readLog(path string, from int64) (events []loggedEvent, validEnd int64, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) && from == 0 {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if from > info.Size() {
		return nil, 0, fmt.Errorf("the database has applied %d bytes of the copy log but it has only %d; run rebuild", from, info.Size())
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	pos := from
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return events, pos, nil // torn: no newline
		}
		line := data[:i]
		data = data[i+1:]
		var ev logEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Event == "" {
			if len(data) == 0 {
				return events, pos, nil // torn: last line is not JSON
			}
			return nil, 0, fmt.Errorf("copy log line at byte %d is not a valid event", pos)
		}
		pos += int64(i) + 1
		events = append(events, loggedEvent{logEvent: ev, End: pos})
	}
	return events, pos, nil
}

// encodeEvent renders one log line, newline included.
func encodeEvent(ev logEvent) ([]byte, error) {
	ev.Time = ev.Time.UTC()
	if ev.ModifiedAt != nil {
		t := ev.ModifiedAt.UTC()
		ev.ModifiedAt = &t
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
