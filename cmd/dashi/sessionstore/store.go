package sessionstore

import (
	"bytes"
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

	"github.com/deepnoodle-ai/dashi/filerecorder"
)

// ErrNotFound reports that no session matches the request.
var ErrNotFound = errors.New("session: not found")

// ErrExists reports that a session with the chosen ID already exists.
var ErrExists = errors.New("session: already exists")

// ErrBusy reports that another process holds the session.
var ErrBusy = errors.New("session: in use by another dashi process")

// Session is the metadata of one conversation. Its turns are the records
// of the dashi/session Session with the same ID.
type Session struct {
	ID string `json:"id"`
	// Dir is the repository path the session belongs to.
	Dir string `json:"dir"`
	// Title is the first prompt, shortened.
	Title string `json:"title"`
	// Model is the model the next new turn uses.
	NextTurn json.RawMessage `json:"next_turn,omitempty"`
	Model    string          `json:"model"`
	Created  time.Time       `json:"created"`
	Updated  time.Time       `json:"updated"`
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

// CreateWithID makes a new empty session for dir with an ID the caller
// chose, such as a launcher that must know the ID before dashi starts. The
// ID is a UUID in lowercase canonical form. It returns ErrExists when the
// store already has a folder with that ID, saved or not, so a chosen ID
// never reopens or overwrites another session. Like Create, it does not
// touch the disk.
func (st *Store) CreateWithID(dir, model, id string) (*Session, error) {
	if !ValidChosenID(id) {
		return nil, fmt.Errorf("session: %q is not a lowercase UUID", id)
	}
	if _, err := os.Lstat(filepath.Join(st.root, id)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	now := st.now()
	return &Session{ID: id, Dir: dir, Model: model, Created: now, Updated: now}, nil
}

// ValidChosenID reports whether id can name a session that the caller
// chose: a UUID in lowercase canonical form.
func ValidChosenID(id string) bool {
	return chosenSessionPattern.MatchString(id)
}

// Discard removes the directory of a session that holds no turn record,
// as a launch that ran no turn leaves it. A session with a turn log stays,
// even when its session file was never saved, so a failed save cannot
// delete recorded history.
func (st *Store) Discard(s *Session) {
	dir := filepath.Join(st.root, s.ID)
	if st.Saved(s) || st.RetainsAttachments(s) {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dir, "turns"))
	if len(entries) > 0 || err != nil && !os.IsNotExist(err) {
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
		if err != nil || s.Dir != dir || !st.HasTurns(s) {
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
// It counts a session made by dashi run like any other, so a follow-up
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

// Saved reports whether s has a session file: its first turn has started.
func (st *Store) Saved(s *Session) bool {
	_, err := os.Stat(filepath.Join(st.root, s.ID, "session.json"))
	return err == nil
}

// HasTurns reports whether the record holds turn 1 of s.
func (st *Store) HasTurns(s *Session) bool {
	rec, err := st.Recorder(s)
	if err != nil {
		return false
	}
	n, err := rec.LastTurn(context.Background(), s.ID)
	return err == nil && n > 0
}

// Recorder returns the turn recorder of s. The turn logs live in the
// session's own directory. It is a dashi/session Index that finds the last
// turn by which logs exist, so a damaged log is reported as that turn, not
// read while the session opens.
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

// Root returns the directory that holds the sessions.
func (st *Store) Root() string {
	return st.root
}

// Diff is the change list shown after a shell call. The turn record does not
// hold it, so the session keeps it beside the turns, to show again when the
// session reopens.
type Diff struct {
	Turn string `json:"turn"`
	// Seq is the sequence number of the step that finished the shell call.
	Seq   int64    `json:"seq"`
	Lines []string `json:"lines"`
}

// AppendDiff adds d to the diff log of s.
func (st *Store) AppendDiff(s *Session, d Diff) error {
	dir := filepath.Join(st.root, s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "diffs.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	return errors.Join(werr, f.Close())
}

// Diffs returns the saved diffs of s, keyed by turn ID and then by step
// sequence number. A missing log gives an empty result, and a damaged line is
// skipped.
func (st *Store) Diffs(s *Session) map[string]map[int64][]string {
	out := map[string]map[int64][]string{}
	data, err := os.ReadFile(filepath.Join(st.root, s.ID, "diffs.jsonl"))
	if err != nil {
		return out
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var d Diff
		if json.Unmarshal(line, &d) != nil || d.Turn == "" {
			continue
		}
		if out[d.Turn] == nil {
			out[d.Turn] = map[int64][]string{}
		}
		out[d.Turn][d.Seq] = d.Lines
	}
	return out
}
