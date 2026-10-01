package media

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/model"
)

// Job is a provider handle for polling an accepted remote operation. The ID
// is meaningful only within Origin's destination scope. Dashi does not own
// persistence or retries.
type Job struct {
	ID        string         `json:"id"`
	Kind      Kind           `json:"kind"`
	Origin    content.Origin `json:"origin"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
}

// Validate requires the handle and its destination scope.
func (j Job) Validate() error {
	if j.ID == "" || !knownKind(j.Kind) || j.Origin.Validate() != nil ||
		j.Origin.Model == "" {
		return errors.New("media: job requires ID, kind, and model origin")
	}
	return nil
}

// Clone detaches optional expiry before the caller changes a copy.
func (j Job) Clone() Job {
	if j.ExpiresAt != nil {
		expiry := *j.ExpiresAt
		j.ExpiresAt = &expiry
	}
	return j
}

// SubmissionState records whether the provider accepted a job request. An
// unknown state does not authorize resubmission.
type SubmissionState string

const (
	// SubmissionAccepted means the provider returned a usable job handle.
	SubmissionAccepted SubmissionState = "accepted"
	// SubmissionRejected means the provider is known not to have accepted it.
	SubmissionRejected SubmissionState = "rejected"
	// SubmissionUnknown means receipt could not be established.
	SubmissionUnknown SubmissionState = "unknown"
)

// Submission keeps an unknown receipt, where the provider may or may not have
// the job, distinct from a known remote handle. Rejected and unknown
// submissions have no handle; the caller records errors, and the evidence it
// needs to look the job up later, separately.
type Submission struct {
	State      SubmissionState `json:"state"`
	Job        *Job            `json:"job,omitempty"`
	ResponseID string          `json:"response_id,omitempty"`
	Served     model.ModelRef  `json:"served,omitzero"`
	// RawUsage records usage reported at submission, independently of any
	// later poll or output fetch. It may be absent even for an accepted job.
	RawUsage json.RawMessage `json:"raw_usage,omitempty"`
}

// Validate rejects invented handles for unknown or rejected submissions.
func (s Submission) Validate() error {
	if err := validateRawUsage(s.RawUsage); err != nil {
		return err
	}
	if s.Served != (model.ModelRef{}) {
		if s.Served.Provider == "" || s.Served.Model == "" {
			return errors.New("media: submitted served model requires provider and model")
		}
		if s.Job != nil && s.Served.Provider != s.Job.Origin.Provider {
			return errors.New("media: submitted served model must match the job provider")
		}
	}
	switch s.State {
	case SubmissionAccepted:
		if s.Job != nil {
			return s.Job.Validate()
		}
	case SubmissionRejected, SubmissionUnknown:
		if s.Job == nil {
			return nil
		}
	}
	return errors.New("media: submission state and job handle disagree")
}

// Clone detaches the accepted job handle and its expiry from the original.
func (s Submission) Clone() Submission {
	if s.Job != nil {
		job := s.Job.Clone()
		s.Job = &job
	}
	s.RawUsage = slices.Clone(s.RawUsage)
	return s
}

// JobState describes the remote work seen during one poll, independently of
// the submission outcome and local output retrieval.
type JobState string

const (
	// JobPending means the provider has not finished the operation.
	JobPending JobState = "pending"
	// JobSucceeded means output is available for a separate fetch step.
	JobSucceeded JobState = "succeeded"
	// JobFailed means the provider completed without usable output.
	JobFailed JobState = "failed"
	// JobExpired means the provider no longer retains the job or output.
	JobExpired JobState = "expired"
)

// JobPoll records one provider status observation. Content holds output
// references when a provider returns them; a fetch step resolves those refs or
// retrieves the output from the job ID. Failure is provider-reported detail.
type JobPoll struct {
	Job     Job             `json:"job"`
	State   JobState        `json:"state"`
	Served  model.ModelRef  `json:"served,omitzero"`
	Content []content.Block `json:"content,omitempty"`
	// StoredOutputs records optional provider-owned files corresponding to
	// generated content. The host still decides durable artifact storage.
	StoredOutputs []StoredOutput `json:"stored_outputs,omitempty"`
	Failure       string         `json:"failure,omitempty"`
	// RawUsage retains a reported usage object until output fetching completes.
	RawUsage json.RawMessage `json:"raw_usage,omitempty"`
}

// Validate prevents an incomplete or failed job from claiming output.
func (p JobPoll) Validate() error {
	if err := p.Job.Validate(); err != nil {
		return err
	}
	if p.Served != (model.ModelRef{}) &&
		(p.Served.Provider != p.Job.Origin.Provider ||
			p.Served.Model == "") {
		return errors.New("media: polled served model requires the job provider")
	}
	if err := validateRawUsage(p.RawUsage); err != nil {
		return err
	}
	switch p.State {
	case JobPending, JobFailed, JobExpired:
		if len(p.Content) > 0 || len(p.StoredOutputs) > 0 {
			return errors.New("media: unfinished job cannot carry output")
		}
	case JobSucceeded:
		if p.Failure != "" {
			return errors.New("media: successful job cannot carry failure")
		}
		for _, block := range p.Content {
			if block.Kind != content.Kind(p.Job.Kind) {
				return errors.New("media: job output requires blocks matching its kind")
			}
			if err := block.Validate(); err != nil {
				return err
			}
		}
		if err := validateStoredOutputs(
			p.StoredOutputs,
			p.Content,
			p.Job.Origin.Provider,
		); err != nil {
			return err
		}
	default:
		return errors.New("media: invalid job state")
	}
	return nil
}

// Clone detaches the job expiry and any output references.
func (p JobPoll) Clone() JobPoll {
	p.Job = p.Job.Clone()
	p.Content = slices.Clone(p.Content)
	for i := range p.Content {
		p.Content[i] = p.Content[i].Clone()
	}
	p.RawUsage = slices.Clone(p.RawUsage)
	p.StoredOutputs = cloneStoredOutputs(p.StoredOutputs)
	return p
}
