package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"slices"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// Adjustment records a deliberate change to a requested setting during
// preparation. The exact wire body remains the authority for dispatch.
type Adjustment struct {
	Setting   string `json:"setting"`
	Requested string `json:"requested,omitempty"`
	Effective string `json:"effective"`
	Reason    string `json:"reason"`
}

// ImageRequestSnapshot records image preparation before any remote attempt.
// Body is the exact JSON or multipart payload, including binary uploads, and
// is encoded as base64 if this snapshot is serialized to JSON.
type ImageRequestSnapshot struct {
	Origin      content.Origin `json:"origin"`
	Input       ImageInput     `json:"input"`
	Effective   ImageSettings  `json:"effective"`
	Body        []byte         `json:"body"`
	ContentType string         `json:"content_type"`
	// ProviderOptions records adapter-specific per-call controls. Body remains
	// the dispatch authority; options must be a JSON object when present.
	ProviderOptions  json.RawMessage        `json:"provider_options,omitempty"`
	Adjustments      []Adjustment           `json:"adjustments,omitempty"`
	ModelInfo        *ModelInfo             `json:"model_info,omitempty"`
	CapabilityPolicy model.CapabilityPolicy `json:"capability_policy,omitempty"`
	Unverified       []string               `json:"unverified,omitempty"`
}

// Clone detaches source bytes, wire bytes, and capability evidence.
func (s ImageRequestSnapshot) Clone() ImageRequestSnapshot {
	s.Input = s.Input.Clone()
	s.Body = slices.Clone(s.Body)
	s.ProviderOptions = slices.Clone(s.ProviderOptions)
	s.Adjustments = slices.Clone(s.Adjustments)
	s.Unverified = slices.Clone(s.Unverified)
	if s.ModelInfo != nil {
		info := s.ModelInfo.Clone()
		s.ModelInfo = &info
	}
	return s
}

// ImageRequest owns destination-bound image intent. Its accessors detach
// mutable data; sending it is a separate attempt that the caller must record.
type ImageRequest struct {
	snapshot ImageRequestSnapshot
}

// NewImageRequest validates and owns one provider-prepared image request.
// Restoring a snapshot does not requalify its evidence or authorize dispatch.
func NewImageRequest(snapshot ImageRequestSnapshot) (ImageRequest, error) {
	if err := snapshot.Input.Validate(); err != nil {
		return ImageRequest{}, err
	}
	if err := snapshot.Effective.Validate(); err != nil {
		return ImageRequest{}, err
	}
	if err := validateProviderOptions(snapshot.ProviderOptions); err != nil {
		return ImageRequest{}, err
	}
	if snapshot.Effective != snapshot.Input.Settings &&
		len(snapshot.Adjustments) == 0 {
		return ImageRequest{}, errors.New("media: changed image settings require adjustments")
	}
	if err := validatePrepared(
		snapshot.Origin, snapshot.Input.Model, snapshot.ContentType,
		snapshot.Body, snapshot.ModelInfo, snapshot.CapabilityPolicy,
		snapshot.Unverified, snapshot.Adjustments,
	); err != nil {
		return ImageRequest{}, err
	}
	return ImageRequest{snapshot: snapshot.Clone()}, nil
}

// Snapshot returns independently owned preparation evidence.
func (r ImageRequest) Snapshot() ImageRequestSnapshot {
	return r.snapshot.Clone()
}

// Origin identifies the destination and credential scope pinned by Prepare.
func (r ImageRequest) Origin() content.Origin {
	return r.snapshot.Origin
}

// Input returns an independent copy of the caller's image input.
func (r ImageRequest) Input() ImageInput {
	return r.snapshot.Input.Clone()
}

// Body returns a copy of the exact payload prepared for dispatch.
func (r ImageRequest) Body() []byte {
	return slices.Clone(r.snapshot.Body)
}

// ContentType returns the exact media type, including any multipart boundary.
func (r ImageRequest) ContentType() string {
	return r.snapshot.ContentType
}

// VideoRequestSnapshot records video intent before any remote attempt. A job
// submission response is recorded separately from this snapshot when used.
type VideoRequestSnapshot struct {
	Origin      content.Origin `json:"origin"`
	Input       VideoInput     `json:"input"`
	Effective   VideoSettings  `json:"effective"`
	Body        []byte         `json:"body"`
	ContentType string         `json:"content_type"`
	// ProviderOptions records adapter-specific per-call controls. Body remains
	// the dispatch authority; options must be a JSON object when present.
	ProviderOptions  json.RawMessage        `json:"provider_options,omitempty"`
	Adjustments      []Adjustment           `json:"adjustments,omitempty"`
	ModelInfo        *ModelInfo             `json:"model_info,omitempty"`
	CapabilityPolicy model.CapabilityPolicy `json:"capability_policy,omitempty"`
	Unverified       []string               `json:"unverified,omitempty"`
}

// Clone detaches source bytes, wire bytes, and capability evidence.
func (s VideoRequestSnapshot) Clone() VideoRequestSnapshot {
	s.Input = s.Input.Clone()
	s.Body = slices.Clone(s.Body)
	s.ProviderOptions = slices.Clone(s.ProviderOptions)
	s.Adjustments = slices.Clone(s.Adjustments)
	s.Unverified = slices.Clone(s.Unverified)
	if s.ModelInfo != nil {
		info := s.ModelInfo.Clone()
		s.ModelInfo = &info
	}
	return s
}

// VideoRequest owns destination-bound video intent. A job submission can
// leave its outcome unknown if the provider's response is lost.
type VideoRequest struct {
	snapshot VideoRequestSnapshot
}

// NewVideoRequest validates and owns one provider-prepared video request.
func NewVideoRequest(snapshot VideoRequestSnapshot) (VideoRequest, error) {
	if err := snapshot.Input.Validate(); err != nil {
		return VideoRequest{}, err
	}
	if err := snapshot.Effective.Validate(); err != nil {
		return VideoRequest{}, err
	}
	if err := validateProviderOptions(snapshot.ProviderOptions); err != nil {
		return VideoRequest{}, err
	}
	if snapshot.Effective != snapshot.Input.Settings &&
		len(snapshot.Adjustments) == 0 {
		return VideoRequest{}, errors.New("media: changed video settings require adjustments")
	}
	if err := validatePrepared(
		snapshot.Origin, snapshot.Input.Model, snapshot.ContentType,
		snapshot.Body, snapshot.ModelInfo, snapshot.CapabilityPolicy,
		snapshot.Unverified, snapshot.Adjustments,
	); err != nil {
		return VideoRequest{}, err
	}
	return VideoRequest{snapshot: snapshot.Clone()}, nil
}

// Snapshot returns independently owned preparation evidence.
func (r VideoRequest) Snapshot() VideoRequestSnapshot {
	return r.snapshot.Clone()
}

// Origin identifies the destination and credential scope pinned by Prepare.
func (r VideoRequest) Origin() content.Origin {
	return r.snapshot.Origin
}

// Input returns an independent copy of the caller's video input.
func (r VideoRequest) Input() VideoInput {
	return r.snapshot.Input.Clone()
}

// Body returns a copy of the exact payload prepared for dispatch.
func (r VideoRequest) Body() []byte {
	return slices.Clone(r.snapshot.Body)
}

// ContentType returns the exact media type prepared for dispatch.
func (r VideoRequest) ContentType() string {
	return r.snapshot.ContentType
}

// ImageClient separates offline preparation from one remote image call.
type ImageClient interface {
	PrepareImage(context.Context, ImageInput) (ImageRequest, error)
	SendImage(context.Context, ImageRequest) (Result, error)
}

// VideoCallClient separates preparation from one synchronous video call.
// A provider may instead implement VideoJobClient for asynchronous work.
type VideoCallClient interface {
	PrepareVideo(context.Context, VideoInput) (VideoRequest, error)
	SendVideo(context.Context, VideoRequest) (Result, error)
}

// VideoJobClient separates submission, polling, and output retrieval. A
// SubmitVideo transport error may accompany SubmissionUnknown; callers must
// record that uncertainty rather than resubmit automatically.
type VideoJobClient interface {
	PrepareVideo(context.Context, VideoInput) (VideoRequest, error)
	SubmitVideo(context.Context, VideoRequest) (Submission, error)
	PollVideo(context.Context, Job) (JobPoll, error)
	FetchVideo(context.Context, JobPoll) (Result, error)
}

func validateProviderOptions(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) || len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("media: provider options require a JSON object")
	}
	return nil
}

func validatePrepared(
	origin content.Origin,
	requestedModel string,
	contentType string,
	body []byte,
	info *ModelInfo,
	unknown model.CapabilityPolicy,
	unverified []string,
	adjustments []Adjustment,
) error {
	if err := origin.Validate(); err != nil {
		return err
	}
	if origin.Model != requestedModel || len(body) == 0 {
		return errors.New("media: prepared request requires matching model and body")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return errors.New("media: prepared request requires a valid content type")
	}
	switch mediaType {
	case "application/json":
		if !json.Valid(body) {
			return errors.New("media: prepared JSON body is invalid")
		}
	case "multipart/form-data":
		if params["boundary"] == "" {
			return errors.New("media: prepared multipart body requires a boundary")
		}
	default:
		return errors.New("media: prepared request requires JSON or multipart content")
	}
	if !unknown.Valid() {
		return errors.New("media: invalid capability policy")
	}
	if len(unverified) > 0 && unknown != model.CapabilityAdjust {
		return errors.New("media: unverified capabilities require the adjust capability policy")
	}
	for i, feature := range unverified {
		if feature == "" {
			return errors.New("media: unverified capability name is required")
		}
		if slices.Contains(unverified[:i], feature) {
			return errors.New("media: duplicate unverified capability")
		}
	}
	for _, adjustment := range adjustments {
		if adjustment.Setting == "" || adjustment.Reason == "" {
			return errors.New("media: adjustment requires setting and reason")
		}
	}
	if info != nil {
		if err := info.Validate(); err != nil {
			return err
		}
		if info.Evidence.Model != origin.Model || info.Evidence.API != origin.API {
			return errors.New("media: model evidence does not match preparation")
		}
	}
	return nil
}
