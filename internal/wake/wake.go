// Package wake lets readers wait for the next commit to a turn. It is the
// in-process wake behind the recorders' dashi.StepWaiter implementations.
package wake

import "sync"

// Set holds one channel per watched turn, closed by the next Notify. The zero
// value is ready to use.
type Set struct {
	mu    sync.Mutex
	turns map[string]chan struct{}
}

// Watch returns a channel that is closed at the turn's next Notify. Take it
// before checking whether a newer step exists, so a commit between the check
// and the wait still wakes the reader.
func (s *Set) Watch(turnID string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turns == nil {
		s.turns = map[string]chan struct{}{}
	}
	ch, ok := s.turns[turnID]
	if !ok {
		ch = make(chan struct{})
		s.turns[turnID] = ch
	}
	return ch
}

// Notify wakes every reader watching the turn.
func (s *Set) Notify(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.turns[turnID]; ok {
		close(ch)
		delete(s.turns, turnID)
	}
}
