// Package kerneltest helps a host prove its kernel.Recorder, in the spirit of
// net/http/httptest and testing/fstest.
//
// Kernel ships only an in-memory recorder. A production host writes its own,
// typically committing each step in the same database transaction as its own
// rows (credit holds, tool jobs, an outbox) under a lease fence. Run checks
// such a recorder against the contract the runner relies on. FaultRecorder
// wraps any recorder to fail one chosen commit, so a host can test how its
// workers recover a turn from the durable record.
//
// The package depends only on kernel and the standard library, so a host can
// import it from tests without pulling in a database driver.
package kerneltest

import (
	"context"

	"github.com/deepnoodle-ai/kernel"
)

// Store is a recorder under test with the operations Kernel's recovery
// contract assumes: kernel.Loader, which Runner.Run also uses when present,
// and Claim, which kernel.Recorder does not declare because lease policy
// belongs to the host. *kernel.MemoryRecorder satisfies Store.
//
// Claim makes owner the only token whose commits the turn accepts, as a lease
// takeover would. It may be called before the turn has any steps. A Claim
// error fails the test.
type Store interface {
	kernel.Recorder
	kernel.Loader
	Claim(ctx context.Context, turnID, owner string) error
}
