package toolcheck

import (
	"testing"
	"time"
)

// Split and PartBytes expose the part cutter to tests.
var (
	Split     = split
	Pick      = pick
	PartBytes = partBytes
)

// SetClock makes the package read the time from *clock until the test
// ends, and clears every pause before and after.
func SetClock(t *testing.T, clock *time.Time) {
	t.Helper()
	ResetPauses()
	now = func() time.Time { return *clock }
	t.Cleanup(func() {
		now = time.Now
		ResetPauses()
	})
}

// PauseCount is how many pauses the table holds.
func PauseCount() int {
	pauses.Lock()
	defer pauses.Unlock()
	return len(pauses.m)
}

// ResetPauses clears every pause.
func ResetPauses() {
	pauses.Lock()
	defer pauses.Unlock()
	clear(pauses.m)
}
