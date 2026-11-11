package delegate

import "testing"

// SetBeforeNotice makes f run just before each notice goes to
// Config.Session, until the test ends.
func SetBeforeNotice(t *testing.T, f func()) {
	beforeNotice.Store(&f)
	t.Cleanup(func() { beforeNotice.Store(nil) })
}
