// Package dashitest helps a host prove its dashi.Recorder, in the spirit of
// net/http/httptest and testing/fstest.
//
// Dashi ships an in-memory recorder and a file-backed one (package
// filerecorder). A production host often writes its own, committing each
// step in the same database transaction as its own rows (credit holds, tool
// jobs, an outbox) under a lease fence. Run checks such a recorder against
// the contract the runner relies on, which dashi.Recorder documents.
// FaultRecorder wraps any recorder to fail one chosen commit, so a host can
// test how its workers recover a turn from the durable record.
//
// The package depends only on dashi and the standard library, so a host can
// import it from tests without pulling in a database driver.
package dashitest

import (
	"context"

	"github.com/deepnoodle-ai/dashi"
)

// Store is a recorder under test with the operations Dashi's recovery
// contract assumes: dashi.Loader, which Runner.Run also uses when present,
// and Claim, which dashi.Recorder does not declare because lease policy
// belongs to the host. *dashi.MemoryRecorder satisfies Store.
//
// Claim makes owner the only token whose commits the turn accepts, as a lease
// takeover would. It may be called before the turn has any steps. A Claim
// error fails the test.
type Store interface {
	dashi.Recorder
	dashi.Loader
	Claim(ctx context.Context, turnID, owner string) error
}
