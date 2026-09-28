# Durable recorders

**Status:** Proposed · **Date:** 2026-09-28 · Findings F 7.1 and 7.2

Dashi ships only `MemoryRecorder`. Resuming a turn in another process means
writing a recorder; `examples/resume` needed about 150 lines, and a correct
retry check needs canonical JSON comparison, which is internal. This note
adds one public comparison, ships a file-backed recorder, states the recorder
contract where authors read it, and sketches what a SQL recorder must do.

## 1. Comparing steps by meaning (F 7.2)

A recorder must accept a retried step at an occupied sequence when it means
the same as the recorded one, and reject anything else. A store such as
Postgres `jsonb` reorders keys and respells numbers, so byte comparison fails
([decision 27](decisions.md)). The smallest surface is one method:

```go
// Equal reports whether s and other have the same JSON meaning: the
// comparison Dashi uses for recorded values (decision 27). Key order,
// whitespace, string escaping, and the spelling of equal numbers do not
// matter; a step that cannot be encoded equals nothing.
func (s Step) Equal(other Step) bool
```

A recorder's retry check becomes `if old.Equal(req.Step) { return nil }`.
`internal/canonjson` stays internal. `MemoryRecorder`, `dashitest`'s private
copy of the comparison, and the file recorder all use `Step.Equal`.

Rejected: exporting `canonjson` (a whole package for one question); a
`Step.Digest()` string that a SQL recorder could store in a column. A stored
digest makes the canonical encoding an at-rest format the store depends on,
and a retry check can load the one occupied row instead. It can be added
later if a host needs it.

## 2. A reference file recorder (F 7.1)

New package `github.com/deepnoodle-ai/dashi/filerecorder`, standard library
only, promoted from the example and hardened:

```go
type Config struct {
	Dir         string // required; created if missing
	DisableSync bool   // skip fsync: survives a process crash, not a power loss
}

func New(Config) (*Recorder, error)
func (*Recorder) Commit(context.Context, dashi.CommitRequest) error
func (*Recorder) Load(context.Context, string) (dashi.Turn, error)
func (*Recorder) Claim(ctx context.Context, turnID, owner string) error
func (*Recorder) Path(turnID string) string // the turn's log, for operators
var ErrCorrupt = errors.New("filerecorder: corrupt turn log")
```

```go
recorder, err := filerecorder.New(filerecorder.Config{Dir: "var/turns"})
runner := dashi.Runner{Agent: agent, Client: client, Recorder: recorder}
```

**Layout.** One append-only JSON Lines log per turn at
`Dir/ab/<sha256 of turn ID>.jsonl`, where `ab` is the hash's first two hex
digits (256 shards; a million turns is about 4,000 files per directory).
Hashing the ID avoids the example's two hazards: `url.PathEscape` names
collide on case-insensitive filesystems (`A` and `a` on macOS) and exceed
the 255-byte name limit for long IDs. The first line is a header,
`{"format":1,"turn_id":"…"}`, which Load checks against the requested ID.
Every later line is `{"step":{…}}` or `{"claim":{"owner":"…","at":"…"}}`,
so the log is readable with `jq` and keeps an audit trail of lease takeovers.

**Atomic appends.** Commit and Claim hold an exclusive OS file lock on the
log (`flock` on Unix, `LockFileEx` on Windows through kernel32; both
standard library only) for check-and-append, so writers in any number of
processes serialize per turn and fencing is exact. Load holds a shared lock.
Each record is one `write` of a newline-terminated line, then `fsync`
(unless `DisableSync`); creating a log also syncs its directory. Only
newline-terminated lines count as written. Other platforms: `New` fails
rather than silently allowing unsafe concurrent writers. Network
filesystems are not supported, since advisory locks there are unreliable.

**Commit** (under the lock): read the log; apply the same checks as
`MemoryRecorder` (owner equals the last claim, command-ID retry or
conflict, `ExpectedSeq` equals the step count, a retry at an occupied
sequence matches by `Step.Equal`); append. Error classes: a failed check,
or a write that fails and is truncated back, is `PersistenceRejected`; a
failed `fsync`, or a failed write that cannot be truncated, returns a plain
error, which the runner treats as unknown and resolves by loading.

**Corruption.** A final line without a newline is a torn write from a crash
before acknowledgement: Load ignores it and the next Commit truncates it. A
complete line that does not decode, a header with another turn ID, or steps
out of sequence make Load and Commit fail with `ErrCorrupt`, naming the
path and line. Run stops on such a Load error instead of acting on a
record it cannot read. There is no automatic repair: an operator inspects
the log.

**Cost.** Commit reads the whole log, so a turn of n steps costs O(n²)
bytes read. Typical turns are tens of steps and well under a megabyte; a
benchmark decides whether an in-process index (line offsets and command
IDs, validated by file size) is worth adding. Retention and deletion stay
the host's job (`Path` locates a turn's log).

**Tests.** `dashitest.Run` conformance; the file recorder added to the
fault-recovery matrix in `dashitest/recovery_test.go`; several OS processes
(the test binary re-executed) racing to commit the same sequence, exactly
one winning; torn tail ignored then repaired; corrupt middle line; header
mismatch; `A`/`a` and 1,000-byte IDs isolated; `DisableSync` conformance.

`examples/resume` then uses `filerecorder.New`, and its `recorder.go` and
`recorder_test.go` are deleted.

## 3. The recorder contract, where authors look

Today `Recorder`'s doc comment is one sentence, and the contract lives in
`dashitest.Run` and the `Loader` comment. An author cannot implement it from
the interface alone. The rewritten `Recorder` comment states the six rules
(append only at `ExpectedSeq+1`; retry of an equal step succeeds, using
`Step.Equal`; command-ID retry and conflict; owner fencing when the host
leases; `*CommitError` with `PersistenceRejected` only when certainly not
written, anything else is unknown; safe for concurrent use; preserve JSON
meaning) and points to `dashitest.Run` as the executable form and to
`filerecorder` as a reference. `CommitRequest.Owner` says it is empty
without `WithOwner`, and that a turn never claimed accepts any owner. The
`dashitest` package doc stops saying Dashi ships only an in-memory recorder.
`docs/guides/running-turns.md` names `filerecorder` where it says to bind a
durable recorder, and `docs/guides/packages.md` lists it.

## 4. What a SQL recorder must guarantee

A host's recorder, typically Postgres, commits each step in one transaction
with its own rows (budget holds, outbox, projections):

- Rows `turns(turn_id pk, owner, seq)` and `steps(turn_id, seq, step jsonb,
  command_id, primary key (turn_id, seq))`, with a unique index on
  `(turn_id, command_id)` where `command_id` is not null.
- In the transaction: lock the turn row (`SELECT … FOR UPDATE`, or insert
  it); reject if a claimed owner differs; if the command ID exists, succeed
  when kind and `command_digest` match, else `ErrCommandConflict`; if
  `seq > ExpectedSeq`, load that step and succeed only if `Step.Equal`;
  otherwise require `seq = ExpectedSeq`, insert, and advance.
- Errors: a fence, conflict, or constraint violation before commit is
  `PersistenceRejected`. A timeout or lost connection once `COMMIT` was sent
  is a plain error, never "rejected".
- Load returns `ErrTurnNotFound` for no rows and any read failure as an
  error. Storing `At` in a timestamp column is fine at microsecond precision.
- Prove it with `dashitest.Run` and the `FaultRecorder` recovery pattern.

**Should it ship?** Not now. A driver dependency does not belong in the core
module, and hosts want the step write inside their own transaction and
schema, which a packaged recorder cannot own. Revisit as a separate module
(`dashi/pgrecorder`, taking a transaction hook) when a second host would
copy the first host's recorder.

## Open questions

1. Package name `filerecorder` (type `filerecorder.Recorder`) or shorter?
2. Should an empty `Config.Dir` default to a directory, or fail as proposed?
   No default location is safe for durable data.
3. The zero-config stream may want the runner to know a recorder is
   durable. Not added here unless that stream asks.
