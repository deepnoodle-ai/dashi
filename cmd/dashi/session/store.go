package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

// ErrNotFound reports that no session matches the request.
var ErrNotFound = errors.New("session: not found")

// ErrBusy reports that another process holds the session.
var ErrBusy = errors.New("session: in use by another dashi process")

// Session is the saved state of one conversation.
type Session struct {
	ID string `json:"id"`
	// Dir is the repository path the session belongs to.
	Dir string `json:"dir"`
	// Title is the first prompt, shortened.
	Title string `json:"title"`
	Model string `json:"model"`
	// Turns lists turn IDs in order. Each ID is the session ID, a dash, and
	// the turn's number.
	Turns   []string  `json:"turns"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// NextTurnID returns the ID the next turn of s gets.
func (s *Session) NextTurnID() string {
	return fmt.Sprintf("%s-%d", s.ID, len(s.Turns)+1)
}

// Store keeps sessions under one root directory, one subdirectory each.
type Store struct {
	root string
	// now is replaced in tests.
	now func() time.Time
	// sync stays true outside tests. It makes the recorder call fsync.
	sync bool
}

// NewStore returns a store rooted at root. The directory is created on the
// first Create.
func NewStore(root string) *Store {
	return &Store{root: root, now: time.Now, sync: true}
}

// DisableSync makes recorders skip fsync. Use it in tests.
func (st *Store) DisableSync() {
	st.sync = false
}

// Create makes a new empty session for dir. It does not touch the disk: the
// session is saved when its first turn records, so a launch that runs no
// turn leaves nothing behind.
func (st *Store) Create(dir, model string) (*Session, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	now := st.now()
	s := &Session{
		ID:      now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]),
		Dir:     dir,
		Model:   model,
		Created: now,
		Updated: now,
	}
	return s, nil
}

// Discard removes the directory of a session that was never saved. A saved
// session stays.
func (st *Store) Discard(s *Session) {
	dir := filepath.Join(st.root, s.ID)
	if _, err := os.Stat(filepath.Join(dir, "session.json")); err == nil {
		return
	}
	os.RemoveAll(dir)
}

// Save writes s to disk and sets its Updated time. It replaces the file
// atomically, so a crash leaves the old or the new file.
func (st *Store) Save(s *Session) error {
	s.Updated = st.now()
	dir := filepath.Join(st.root, s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "session.json")); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Load returns the session with the given ID.
func (st *Store) Load(id string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(st.root, id, "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session: read %s: %w", id, err)
	}
	return &s, nil
}

// List returns the sessions of dir that have at least one turn, newest first.
// A session file that cannot be read is left out, so one bad file does not
// hide the others.
func (st *Store) List(dir string) ([]Session, error) {
	entries, err := os.ReadDir(st.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := st.Load(e.Name())
		if err != nil || s.Dir != dir || len(s.Turns) == 0 {
			continue
		}
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Session) int {
		return b.Updated.Compare(a.Updated)
	})
	return out, nil
}

// Latest returns the most recently updated session of dir that has a turn.
// It counts a session made by a print run (-p) like any other, so a follow-up
// can build on it.
func (st *Store) Latest(dir string) (*Session, error) {
	all, err := st.List(dir)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: no session for %s", ErrNotFound, dir)
	}
	return &all[0], nil
}

// Recorder returns the turn recorder of s. The turn logs live in the
// session's own directory.
func (st *Store) Recorder(s *Session) (*filerecorder.Recorder, error) {
	return filerecorder.New(filerecorder.Config{
		Dir:         filepath.Join(st.root, s.ID, "turns"),
		DisableSync: !st.sync,
	})
}

// Lock claims s for this process until the returned function runs. It
// returns ErrBusy when another process holds it.
func (st *Store) Lock(s *Session) (unlock func(), err error) {
	dir := filepath.Join(st.root, s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrBusy, s.ID)
	}
	// Closing the file releases the flock.
	return func() { f.Close() }, nil
}

// History is the model-visible conversation built from turn records.
type History struct {
	Messages []llm.Message
	// Notices names each turn left out, with the reason.
	Notices []string
}

// History returns the messages of every turn of s.
func (st *Store) History(
	ctx context.Context,
	rec dashi.Loader,
	s *Session,
) (History, error) {
	return st.HistoryBefore(ctx, rec, s, len(s.Turns))
}

// HistoryBefore returns the messages of the first n turns of s. It is the
// only place that builds model history. A turn that cannot be loaded or
// projected is skipped and named in Notices, and the rest still count. Only
// a canceled context stops it.
//
// A compaction summary, when the CLI has one, applies here over the
// projected messages.
func (st *Store) HistoryBefore(
	ctx context.Context,
	rec dashi.Loader,
	s *Session,
	n int,
) (History, error) {
	var h History
	for i, id := range s.Turns[:min(n, len(s.Turns))] {
		if err := ctx.Err(); err != nil {
			return History{}, err
		}
		turn, err := rec.Load(ctx, id)
		if err != nil {
			h.Notices = append(h.Notices, fmt.Sprintf(
				"Turn %d skipped: %v.", i+1, err,
			))
			continue
		}
		msgs, err := dashi.Project(turn)
		if err != nil {
			h.Notices = append(h.Notices, fmt.Sprintf(
				"Turn %d skipped: %v.", i+1, err,
			))
			continue
		}
		h.Messages = append(h.Messages, msgs...)
	}
	return h, nil
}

// Root returns the directory that holds the sessions.
func (st *Store) Root() string {
	return st.root
}
