package dashi

import (
	"errors"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Failure preserves safe model error facts through reload. RecordedAt is the
// original fact's time, so RecordedAt.Add(RetryAfter) does not restart a wait.
// Retryable is provider advice, never permission to resend an unknown call.
// Unclassified fields are empty. Raw provider bodies, codes, and causes are
// excluded. Applications own scheduling and credential repair.
type Failure struct {
	Category   llm.ErrorCategory `json:"category,omitempty"`
	Reason     llm.ErrorReason   `json:"reason,omitempty"`
	Status     int               `json:"status,omitempty"`
	SendStatus llm.SendStatus    `json:"send_status"`
	Retryable  bool              `json:"retryable,omitempty"`
	RetryAfter time.Duration     `json:"retry_after,omitempty"`
	RecordedAt time.Time         `json:"recorded_at"`
}

func failureOf(err error, status llm.SendStatus) *Failure {
	f := &Failure{SendStatus: status}
	if e, ok := errors.AsType[*llm.Error](err); ok && e != nil {
		if validFailureCategory(e.Category) {
			f.Category = e.Category
		}
		if validFailureReason(e.Reason) {
			f.Reason = e.Reason
		}
		if e.Status >= 100 && e.Status <= 599 {
			f.Status = e.Status
		}
		f.Retryable = e.Retryable
		f.RetryAfter = max(e.RetryAfter, 0)
	}
	return f
}

func failureSummary(summary string, f *Failure) string {
	if f.Category != "" {
		summary += ": " + string(f.Category)
	}
	if f.Reason != "" {
		summary += " (" + string(f.Reason) + ")"
	}
	if f.Status != 0 {
		summary += fmt.Sprintf("; HTTP %d", f.Status)
	}
	return summary
}

func (r *runtime) prepareFailure(err error) RunResult {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	f := failureOf(err, llm.SendStatusNotSent)
	return r.stopWith(&Stop{
		Kind:    StopFailed,
		Reason:  failureSummary("model preparation failed", f),
		Failure: f,
	})
}

func (f *Failure) validate(at time.Time) error {
	if f == nil {
		return nil
	}
	if !validFailureCategory(f.Category) || !validFailureReason(f.Reason) {
		return errors.New("invalid model failure classification")
	}
	if f.Status != 0 && (f.Status < 100 || f.Status > 599) ||
		f.RetryAfter < 0 || f.RecordedAt.IsZero() || !f.RecordedAt.Equal(at) {
		return errors.New("invalid model failure status or time")
	}
	if f.SendStatus != llm.SendStatusNotSent &&
		f.SendStatus != llm.SendStatusProviderRejected &&
		f.SendStatus != llm.SendStatusUnknown {
		return errors.New("invalid model failure send status")
	}
	return nil
}

func cloneFailure(f *Failure) *Failure {
	if f == nil {
		return nil
	}
	copy := *f
	return &copy
}

func validFailureCategory(category llm.ErrorCategory) bool {
	switch category {
	case "", llm.ErrorInvalidRequest, llm.ErrorUnsupported,
		llm.ErrorAuthentication, llm.ErrorRateLimited, llm.ErrorOverloaded,
		llm.ErrorTransport, llm.ErrorProtocol:
		return true
	}
	return false
}

func validFailureReason(reason llm.ErrorReason) bool {
	switch reason {
	case "", llm.ReasonContextWindowExceeded, llm.ReasonMediaRejected,
		llm.ReasonResponseStartTimeout, llm.ReasonStreamIdleTimeout,
		llm.ReasonCallTimeout:
		return true
	}
	return false
}
