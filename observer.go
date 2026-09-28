package dashi

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

// sendPreview queues a best-effort preview, such as tool progress, only while
// the buffer is at most half full, so frequent previews never crowd out steps.
func (e *emitter) sendPreview(event Event) {
	if e == nil || e.ch == nil || len(e.ch) >= cap(e.ch)/2 {
		return
	}
	e.send(event)
}
