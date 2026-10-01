package filerecorder

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

// logFormat is the header's format version.
const logFormat = 1

// indexCapacity bounds how many turns' indexes a Recorder keeps. An evicted
// turn is rebuilt by reading its log once.
const indexCapacity = 1024

type headerRecord struct {
	Format int    `json:"format"`
	TurnID string `json:"turn_id"`
}

type claimRecord struct {
	Owner string    `json:"owner"`
	At    time.Time `json:"at"`
}

// lineRecord decodes only the fields Commit checks, so a step's payload is
// scanned but not built.
type lineRecord struct {
	Format int          `json:"format"`
	TurnID string       `json:"turn_id"`
	Step   *stepSummary `json:"step"`
	Claim  *claimRecord `json:"claim"`
}

type stepSummary struct {
	TurnID      string         `json:"turn_id"`
	Seq         int64          `json:"seq"`
	Kind        dashi.StepKind `json:"kind"`
	CommandID   string         `json:"command_id"`
	CommandHash string         `json:"command_hash"`
}

// stepEntry locates one step's line in the log.
type stepEntry struct {
	stepSummary
	off    int64
	size   int64
	number int
}

// turnLog indexes the complete lines of one log: what Commit checks, and
// where each step's line is. Because the log is append-only and a torn line
// never counts, the index stays valid as the log grows; scan reads on from
// end instead of from the start.
type turnLog struct {
	file      os.FileInfo
	hasHeader bool
	steps     []stepEntry
	claimed   bool
	owner     string
	lines     int
	// dirsSynced records that this process synced the directories that name
	// the log, which it does once before its first acknowledged write.
	dirsSynced bool
	// end is the offset just past the last complete line; size is the file
	// size when last scanned. They differ only when a torn line follows.
	end  int64
	size int64
}

// scan brings log up to date with f, reading only the bytes after log.end,
// and returns those bytes with the offset they start at. A zero log scans
// from the start. Only newline-terminated lines count: a final line without
// one is a torn write that was never acknowledged.
func scan(f *os.File, turnID string, log turnLog) (turnLog, []byte, error) {
	return scanContext(context.Background(), f, turnID, log)
}

func scanContext(ctx context.Context, f *os.File, turnID string, log turnLog) (turnLog, []byte, error) {
	if err := ctx.Err(); err != nil {
		return turnLog{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return turnLog{}, nil, fmt.Errorf("filerecorder: read log: %w", err)
	}
	if log.file == nil ||
		!os.SameFile(log.file, info) ||
		!endsLine(f, log.end) {
		// The file was replaced, shrank, or was rewritten below what was
		// indexed, which the log format never does; start over rather than
		// trust the index.
		log = turnLog{}
	}
	log.file = info
	log.steps = slices.Clip(log.steps)
	unread := io.NewSectionReader(f, log.end, info.Size()-log.end)
	data, err := io.ReadAll(contextLogReader{ctx, unread})
	if err != nil {
		return turnLog{}, nil, fmt.Errorf("filerecorder: read log: %w", err)
	}
	base := log.end
	complete := bytes.LastIndexByte(data, '\n') + 1
	log.size = base + int64(len(data))
	for rest, off := data[:complete], base; len(rest) > 0; {
		if err := ctx.Err(); err != nil {
			return turnLog{}, nil, err
		}
		n := bytes.IndexByte(rest, '\n') + 1
		line := rest[:n]
		log.lines++
		if err := log.add(line, off, turnID, f.Name()); err != nil {
			return turnLog{}, nil, err
		}
		rest, off = rest[n:], off+int64(n)
	}
	log.end = base + int64(complete)
	return log, data, nil
}

// endsLine reports whether offset end follows a newline, as the end of an
// indexed log does. It is false for a file shorter than end, and it catches
// most logs rewritten in place, which the file identity check misses.
func endsLine(f *os.File, end int64) bool {
	if end == 0 {
		return true
	}
	b := make([]byte, 1)
	_, err := f.ReadAt(b, end-1)
	return err == nil && b[0] == '\n'
}

// add indexes one complete line found at offset off.
func (log *turnLog) add(line []byte, off int64, turnID, path string) error {
	var record lineRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return corrupt(path, log.lines, err.Error())
	}
	if record.Step != nil && record.Claim != nil {
		return corrupt(path, log.lines, "line holds both a step and a claim")
	}
	switch {
	case log.lines == 1:
		if record.Format != logFormat || record.TurnID != turnID {
			return corrupt(
				path,
				log.lines,
				fmt.Sprintf(
					"header is format %d for turn %q; want format %d for %q",
					record.Format,
					record.TurnID,
					logFormat,
					turnID,
				),
			)
		}
		log.hasHeader = true
	case record.Step != nil:
		want := int64(len(log.steps) + 1)
		if record.Step.TurnID != turnID || record.Step.Seq != want {
			return corrupt(
				path,
				log.lines,
				fmt.Sprintf(
					"step is seq %d of turn %q; want seq %d",
					record.Step.Seq,
					record.Step.TurnID,
					want,
				),
			)
		}
		log.steps = append(log.steps, stepEntry{
			stepSummary: *record.Step,
			off:         off,
			size:        int64(len(line)),
			number:      log.lines,
		})
	case record.Claim != nil:
		log.claimed = true
		log.owner = record.Claim.Owner
	default:
		return corrupt(path, log.lines, "unknown record")
	}
	return nil
}

// decode returns the full step recorded on the entry's line, taken from
// data when it holds the line (data starts at offset base) or read from f.
func (e stepEntry) decode(
	f *os.File,
	data []byte,
	base int64,
) (dashi.Step, error) {
	var line []byte
	if e.off >= base && e.off+e.size <= base+int64(len(data)) {
		line = data[e.off-base : e.off-base+e.size]
	} else {
		line = make([]byte, e.size)
		if _, err := f.ReadAt(line, e.off); err != nil {
			return dashi.Step{}, fmt.Errorf("filerecorder: read log: %w", err)
		}
	}
	var record struct {
		Step dashi.Step `json:"step"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return dashi.Step{}, corrupt(f.Name(), e.number, err.Error())
	}
	return record.Step, nil
}

// indexCache keeps recently used turn indexes, least recently used first
// out. Callers hold Recorder.mu.
type indexCache struct {
	order *list.List
	items map[string]*list.Element
}

type cacheItem struct {
	turnID string
	log    turnLog
}

func newIndexCache() *indexCache {
	return &indexCache{order: list.New(), items: map[string]*list.Element{}}
}

func (c *indexCache) get(turnID string) turnLog {
	e, ok := c.items[turnID]
	if !ok {
		return turnLog{}
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheItem).log
}

func (c *indexCache) markDirsSynced(turnID string) {
	if e, ok := c.items[turnID]; ok {
		e.Value.(*cacheItem).log.dirsSynced = true
	}
}

func (c *indexCache) put(turnID string, log turnLog) {
	if e, ok := c.items[turnID]; ok {
		e.Value.(*cacheItem).log = log
		c.order.MoveToFront(e)
		return
	}
	c.items[turnID] = c.order.PushFront(&cacheItem{turnID: turnID, log: log})
	if c.order.Len() > indexCapacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheItem).turnID)
	}
}

// Chunk bounds give cancellation a chance even for a large recorded step.
type contextLogReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextLogReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p[:min(len(p), 32<<10)])
}
