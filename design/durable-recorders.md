# Durable recorders

**Status:** Decided · **Date:** 2026-09-28 · Findings F 7.1 and 7.2

Dashi shipped only `MemoryRecorder`, so resuming a turn in another process
meant writing a recorder, and a correct retry check needed canonical JSON
comparison, which is internal. This note adds one public comparison, ships a
file-backed recorder, and states the recorder contract where authors read it.

## 1. Comparing steps by meaning (F 7.2)

A recorder must accept a retried step at an occupied sequence when it means
the same as the recorded one, and reject anything else. Stores such as
Postgres `jsonb` reorder keys and respell numbers, so bytes do not compare
([decision 27](decisions.md)). The public surface is one method:

```go
// Equal reports whether s and other have the same JSON meaning. A step
// that cannot be encoded equals nothing.
func (s Step) Equal(other Step) bool
```

It canonicalizes both steps directly and returns false on any encoding
error; it does not reuse the digest helper, whose fallback could make two
unencodable steps hash alike. `internal/canonjson` stays internal.
`dashitest` keeps its own, independent comparison (a conformance oracle
should not share the code it checks), and a test asserts the two agree.

Rejected: exporting `canonjson` (a package for one question), and a
`Step.Digest()` string for a SQL column, which would make the canonical
encoding an at-rest format.

## 2. A reference file recorder (F 7.1)

Package `github.com/deepnoodle-ai/dashi/filerecorder`, standard library only:

```go
recorder, err := filerecorder.New(filerecorder.Config{Dir: "var/turns"})
runner := dashi.Runner{Agent: agent, Client: client, Recorder: recorder}
```

`Config{Dir, DisableSync}`: an empty `Dir` fails with an error naming the
field; `DisableSync` skips fsync (survives a process crash, not a power
loss). `*Recorder` has `Commit`, `Load`, `Claim`, and `Path(turnID)`;
`ErrCorrupt` marks an unreadable log.

**Layout.** One append-only JSON Lines log per turn at
`Dir/ab/<sha256 of turn ID>.jsonl`, `ab` being the first two hex digits.
Hashing avoids case-insensitive collisions (`A` and `a` on macOS) and the
255-byte name limit. The first line is `{"format":1,"turn_id":"…"}`, checked
by Load; each later line is `{"step":{…}}` or `{"claim":{"owner":"…"}}`, so
lease takeovers leave an audit trail. Directories are 0700, files 0600.

**Platforms.** Unix only, by explicit build tags (`linux || darwin ||
freebsd || openbsd || netbsd || android || ios`; `unix` would include
systems without `syscall.Flock`). Elsewhere `New` returns a "not supported
on GOOS" error. Locks are `flock`, which belongs to an open file description,
so two opens in one process exclude each other; `fcntl` locks belong to the
process and would not. Network filesystems are unsupported.

**Commit.** Take an exclusive lock with a non-blocking attempt retried at a
short backoff until the context ends (the runner bounds a commit after
cancellation to five seconds); a context that ends first gives
`PersistenceRejected`. Under the lock, read the log, parsing only seq, kind,
command ID and digest, and claim owners; apply `MemoryRecorder`'s checks
(owner equals the last claim; command-ID retry or conflict; `ExpectedSeq`
equals the step count; an occupied sequence matches by `Step.Equal`), then
append one newline-terminated line in one write and fsync. A failed check,
or a failed write truncated back, is rejected. A failed fsync, or a write
that cannot be truncated, is a plain error (unknown), and that log accepts no
more commits in this process, since the kernel may have dropped the data.
Whole-turn validation, which `MemoryRecorder` does with an internal replay,
is optional; the runner validates what it loads. Load takes a shared lock.

**Corruption.** A final line without a newline is a torn write from a crash
before acknowledgement: Load ignores it and the next Commit truncates it. A
complete line that does not decode, a header for another turn, or steps out
of sequence fail with `ErrCorrupt`, naming the path and line; Run stops on
that Load error. There is no automatic repair. The host must not delete a
log while its turn may still run.

**Cost.** Reading the whole log on every commit was O(n²) per turn: a
500-step turn took 0.7 s at 1 KB per step and 42 s at 100 KB. So Commit
keeps a per-turn index (offset past the last complete line, step summaries,
owner) in a 1,024-turn LRU and reads only what was appended since; the same
turns take 24 ms and 0.25 s. The index is dropped when the file is not the
same file (`os.SameFile`) or is shorter than the indexed offset; file size
alone would not do, since a truncate and equal-length append defeats it.
Load still reads the whole log.

**Tests.** Conformance; the fault-recovery matrix; processes racing for one
sequence and taking over a lease; a claimed turn with no steps; a lock wait
ended by its context; torn, corrupt, and mismatched logs; injected write and
fsync failures; `A`/`a` and long IDs. `examples/resume` switches to the
package and drops its own recorder.

## 3. The recorder contract, where authors look

`Recorder`'s doc comment states the rules: append only at `ExpectedSeq+1`; a
retry equal by `Step.Equal` succeeds; command-ID retry and conflict; owner
fencing when the host leases; `*CommitError` with `PersistenceRejected`
only when certainly not written, anything else unknown; safe for concurrent
use; preserve JSON meaning. It points to `dashitest.Run` as the executable
contract and to `filerecorder` as a reference. `CommitRequest.Owner`
explains the empty and unclaimed cases.

## 4. What a SQL recorder must guarantee

Commit each step in one transaction with the host's own rows:

- Lock the turn row; reject a different claimed owner; a known command ID
  succeeds only with the same kind and digest, else `ErrCommandConflict`; an
  occupied sequence succeeds only if the stored step `Equal`s the new one;
  otherwise require `seq = ExpectedSeq`, insert, and advance.
- The stored step JSON is the record. Never rebuild `At` from a timestamp
  column: the runner stamps nanoseconds, a microsecond column loses them, and
  a genuine retry would stop being equal and be rejected. A timestamp column
  is for indexing only.
- Postgres `jsonb` rejects `\u0000` in strings; store such steps as `json` or
  text, or reject them explicitly.
- A fence, conflict, or constraint failure before commit is rejected; a
  timeout or lost connection after `COMMIT` was sent is a plain error.
- Load returns `ErrTurnNotFound` for no rows. Prove it with `dashitest`.

**Should it ship?** Not now: a driver does not belong in the core module, and
hosts want the write inside their own transaction and schema. Revisit as a
separate module when a second host would copy the first host's recorder.
