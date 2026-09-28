package kernel

// Display events are bounded and lossy. The recorder, not Observer, is the
// authoritative path for committed steps. A broken observer cannot block work.
type emitter struct{ ch chan Event }

func newEmitter(observer Observer) *emitter {
	if observer == nil {
		return &emitter{}
	}
	e := &emitter{ch: make(chan Event, 64)}
	go func() {
		for event := range e.ch {
			func() { defer func() { _ = recover() }(); observer(event) }()
		}
	}()
	return e
}

func (e *emitter) send(event Event) {
	if e == nil || e.ch == nil {
		return
	}
	select {
	case e.ch <- event:
	default:
	}
}

func (e *emitter) close() {
	if e != nil && e.ch != nil {
		close(e.ch)
	}
}
