// Package kerneltest helps a host prove its kernel.Recorder, in the spirit of
// net/http/httptest and testing/fstest.
//
// Kernel ships only an in-memory recorder. A production host writes its own,
// typically committing each step in the same database transaction as its own
// rows (credit holds, tool jobs, an outbox) under a lease fence. Run checks
// such a recorder against the contract the engine relies on. FaultRecorder
// wraps any recorder to fail one chosen commit, so a host can test how its
// workers recover a turn from the durable record.
//
// The package depends only on kernel and the standard library, so a host can
// import it from tests without pulling in a database driver.
package kerneltest

import "github.com/deepnoodle-ai/kernel"

// Store is a recorder under test together with the two host operations that
// Kernel's recovery contract assumes but kernel.Recorder does not declare.
// *kernel.MemoryRecorder satisfies Store.
//
// Load returns the authoritative record of a turn. It is also the optional
// method Engine.Apply uses to recognize a command retried with an old turn
// snapshot, so a recorder that matches this signature gets that behavior.
//
// Claim makes owner the only token whose commits the turn accepts, as a lease
// takeover would. It may be called before the turn has any steps. A host whose
// lease operation can fail should report the failure through the *testing.T
// passed to the constructor given to Run.
type Store interface {
	kernel.Recorder
	Load(turnID string) (kernel.Turn, error)
	Claim(turnID, owner string)
}
