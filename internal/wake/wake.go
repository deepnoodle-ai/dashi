// Package wake lets readers wait for the next commit to a turn. It is the
// in-process wake behind the recorders' dashi.StepWaiter implementations.
package wake

import "sync"

// Set holds one channel per watched turn, closed by the next Notify. An
// entry lives only while someone watches the turn. The zero value is ready
// to use.
type Set struct {
	mu    sync.Mutex
	turns map[string]*entry
}

type entry struct {
	ch       chan struct{}
	watchers int
}

// Watch returns a channel that is closed at the turn's next Notify, and a
// function to call once the caller stops waiting. Take it before checking
// whether a newer step exists, so a commit between the check and the wait
// still wakes the reader.
func (s *Set) Watch(turnID string) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turns == nil {
		s.turns = map[string]*entry{}
	}
	e, ok := s.turns[turnID]
	if !ok {
		e = &entry{ch: make(chan struct{})}
		s.turns[turnID] = e
	}
	e.watchers++
	var once sync.Once
	return e.ch, func() {
		once.Do(func() { s.unwatch(turnID, e) })
	}
}

func (s *Set) unwatch(turnID string, e *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.watchers--
	if e.watchers == 0 && s.turns[turnID] == e {
		delete(s.turns, turnID)
	}
}

// Notify wakes every reader watching the turn.
func (s *Set) Notify(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.turns[turnID]; ok {
		close(e.ch)
		delete(s.turns, turnID)
	}
}

// Len reports the watched turns, for tests.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}
