package session

import "time"

// withClock is used by tests through the exported name below.
func setClock(st *Store, now func() time.Time) *Store {
	st.now = now
	return st
}

var WithClock = setClock
