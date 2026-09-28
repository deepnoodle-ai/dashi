package kerneltest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/deepnoodle-ai/kernel"
)

// Fault selects how a FaultRecorder fails the commit it matches.
type Fault string

const (
	// Reject does not commit and returns a *kernel.CommitError with
	// kernel.PersistenceRejected, as a failed fence or constraint would.
	Reject Fault = "reject"

	// LoseWrite does not commit and returns a *kernel.CommitError with
	// kernel.PersistenceUnknown, as a timeout before the transaction
	// committed would.
	LoseWrite Fault = "lose_write"

	// LoseAck commits and then returns a plain error, which Kernel treats as
	// unknown: the transaction committed but its acknowledgement was lost.
	LoseAck Fault = "lose_ack"
)

// ErrInjected is wrapped by every error a FaultRecorder returns for an
// injected fault.
var ErrInjected = errors.New("kerneltest: injected fault")

// FaultRecorder wraps a recorder and fails the first commit whose step
// matches Kind and Seq; a zero field matches any step. The fault fires once,
// and every other commit passes through, so the same recorder can serve the
// worker that resumes the turn.
//
// Load passes through to the wrapped recorder, including its errors, so
// Runner.Run treats the wrapper as it would the recorder. The wrapped
// recorder should be a kernel.Loader; otherwise Load returns an error that is
// not kernel.ErrTurnNotFound, and Run returns it.
type FaultRecorder struct {
	Recorder kernel.Recorder
	Kind     kernel.StepKind
	Seq      int64
	Fault    Fault

	mu    sync.Mutex
	fired *kernel.Step
}

// Commit applies the fault to the first matching step and otherwise passes
// the request to the wrapped recorder.
func (f *FaultRecorder) Commit(
	ctx context.Context,
	req kernel.CommitRequest,
) error {
	if !f.take(req.Step) {
		return f.Recorder.Commit(ctx, req)
	}
	switch f.Fault {
	case Reject:
		return &kernel.CommitError{
			Persistence: kernel.PersistenceRejected,
			Err:         ErrInjected,
		}
	case LoseWrite:
		return &kernel.CommitError{
			Persistence: kernel.PersistenceUnknown,
			Err:         ErrInjected,
		}
	case LoseAck:
		if err := f.Recorder.Commit(ctx, req); err != nil {
			return err
		}
		return fmt.Errorf("%w: acknowledgement lost after commit", ErrInjected)
	}
	return fmt.Errorf("kerneltest: unknown fault %q", f.Fault)
}

// Load returns the wrapped recorder's record of the turn.
func (f *FaultRecorder) Load(
	ctx context.Context,
	turnID string,
) (kernel.Turn, error) {
	loader, ok := f.Recorder.(kernel.Loader)
	if !ok {
		return kernel.Turn{}, errors.New("kerneltest: wrapped recorder is not a kernel.Loader")
	}
	return loader.Load(ctx, turnID)
}

// Fired returns the step the fault was applied to, if it has fired.
func (f *FaultRecorder) Fired() (kernel.Step, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired == nil {
		return kernel.Step{}, false
	}
	return *f.fired, true
}

func (f *FaultRecorder) take(step kernel.Step) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired != nil ||
		(f.Kind != "" && step.Kind != f.Kind) ||
		(f.Seq != 0 && step.Seq != f.Seq) {
		return false
	}
	f.fired = &step
	return true
}
