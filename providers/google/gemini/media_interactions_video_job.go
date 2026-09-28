package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

// InteractionsVideoJobClient requests Files URI delivery. Interactions accepts
// the generation request; the resulting File is a separate processing job.
// Polling that File and fetching its bytes remain explicit caller steps.
type InteractionsVideoJobClient struct {
	client *InteractionsMediaClient
}

var _ media.VideoJobClient = (*InteractionsVideoJobClient)(nil)

// NewInteractionsVideoJobs configures Omni video generation with Files URI
// delivery. The same evidence and credential scope rules as
// NewInteractionsMedia
// apply.
func NewInteractionsVideoJobs(
	config InteractionsMediaConfig,
) (*InteractionsVideoJobClient, error) {
	client, err := NewInteractionsMedia(config)
	if err != nil {
		return nil, err
	}
	return &InteractionsVideoJobClient{client: client}, nil
}

// Provider identifies the destination that owns these Files.
func (p *InteractionsVideoJobClient) Provider() string {
	return p.client.Provider()
}

// VideoModelInfo returns a detached snapshot of the configured evidence.
func (p *InteractionsVideoJobClient) VideoModelInfo(
	name string,
) (media.ModelInfo, bool) {
	return p.client.VideoModelInfo(name)
}

// PrepareVideo records a URI-delivery request, including 1080p or 4k output.
// Gemini requires stored interaction state to create the provider File.
func (p *InteractionsVideoJobClient) PrepareVideo(
	ctx context.Context,
	in media.VideoInput,
) (media.VideoRequest, error) {
	return p.client.prepareVideo(ctx, in, "uri")
}

// SubmitVideo sends the prepared generation request once. A lost response or
// unusable File handle leaves the submission outcome unknown, never retried.
func (p *InteractionsVideoJobClient) SubmitVideo(
	ctx context.Context,
	request media.VideoRequest,
) (media.Submission, error) {
	in := request.Input()
	if request.Origin() != p.client.origin(in.Model) ||
		request.ContentType() != "application/json" {
		return media.Submission{
				State: media.SubmissionRejected,
			}, p.client.failure(
				media.ErrorInvalidRequest,
				"prepared URI video request belongs to another destination",
			)
	}
	if delivery, err := preparedVideoDelivery(request.Body()); err != nil ||
		delivery != "uri" {
		return media.Submission{
				State: media.SubmissionRejected,
			}, p.client.failure(
				media.ErrorInvalidRequest,
				"prepared video request is not a URI Interactions job",
			)
	}
	raw, err := p.client.do(ctx, request.Body())
	if err != nil {
		var providerErr *media.Error
		if errors.As(err, &providerErr) && providerErr.Status >= 400 &&
			providerErr.Status < 500 &&
			providerErr.Status != 408 &&
			providerErr.Status != 409 &&
			providerErr.Status != 425 && providerErr.Status != 429 {
			return media.Submission{State: media.SubmissionRejected}, err
		}
		return media.Submission{State: media.SubmissionUnknown}, err
	}
	var response interactionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return media.Submission{
				State: media.SubmissionUnknown,
			}, p.client.failure(
				media.ErrorProtocol,
				"decode Interactions URI response: %w",
				err,
			)
	}
	submission := media.Submission{
		State:      media.SubmissionUnknown,
		ResponseID: response.ID,
		RawUsage:   slices.Clone(response.Usage),
	}
	if response.Model != "" {
		submission.Served = model.ModelRef{
			Provider: p.Provider(),
			Model:    response.Model,
		}
	}
	if response.Status == "failed" || response.Status == "cancelled" {
		// The request reached Interactions, but no Files handle exists for the
		// job contract. Preserve observed facts without claiming non-acceptance.
		return submission, p.client.failure(
			media.ErrorProtocol,
			"Interactions generation %s without Files handle",
			response.Status,
		)
	}
	if response.Status != "completed" {
		return submission, p.client.failure(
			media.ErrorProtocol,
			"Interactions URI response status %q",
			response.Status,
		)
	}
	var fileName string
	for _, step := range response.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			if part.Type != "video" {
				continue
			}
			if part.Data != "" || !strings.HasPrefix(part.MIMEType, "video/") {
				return submission, p.client.failure(
					media.ErrorProtocol,
					"Interactions URI response has invalid video delivery",
				)
			}
			name, err := p.fileNameFromURI(part.URI)
			if err != nil {
				return submission, err
			}
			if fileName != "" {
				return submission, p.client.failure(
					media.ErrorProtocol,
					"multiple URI videos need separate job handles",
				)
			}
			fileName = name
		}
	}
	if fileName == "" {
		return submission, p.client.failure(
			media.ErrorProtocol,
			"completed Interactions response has no Files URI",
		)
	}
	job := media.Job{
		ID:     fileName,
		Kind:   media.KindVideo,
		Origin: p.client.origin(in.Model),
	}
	submission.State, submission.Job = media.SubmissionAccepted, &job
	return submission, nil
}

// PollVideo reads a generated File's processing state once.
func (p *InteractionsVideoJobClient) PollVideo(
	ctx context.Context,
	job media.Job,
) (media.JobPoll, error) {
	if err := p.validateJob(job); err != nil {
		return media.JobPoll{}, err
	}
	endpoint := p.fileURL(job.ID, false)
	raw, err := p.getFile(ctx, endpoint, maxOperationBytes, false)
	if err != nil {
		var providerErr *media.Error
		if errors.As(
			err,
			&providerErr,
		) && providerErr.Status == http.StatusNotFound &&
			job.ExpiresAt != nil && !time.Now().Before(*job.ExpiresAt) {
			return media.JobPoll{Job: job.Clone(), State: media.JobExpired}, nil
		}
		return media.JobPoll{}, err
	}
	var file struct {
		Name           string `json:"name"`
		MIMEType       string `json:"mimeType"`
		Source         string `json:"source"`
		DownloadURI    string `json:"downloadUri"`
		State          string `json:"state"`
		ExpirationTime string `json:"expirationTime"`
		Error          struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		VideoMetadata struct {
			VideoDuration string `json:"videoDuration"`
		} `json:"videoMetadata"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"decode Gemini File: %w",
			err,
		)
	}
	if file.Name != job.ID {
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"Gemini File identity changed",
		)
	}
	if file.Source != "" && file.Source != "GENERATED" {
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"Gemini output File has source %q",
			file.Source,
		)
	}
	poll := media.JobPoll{Job: job.Clone(), State: media.JobPending}
	if file.ExpirationTime != "" {
		expiry, err := time.Parse(time.RFC3339Nano, file.ExpirationTime)
		if err != nil {
			return media.JobPoll{}, p.client.failure(
				media.ErrorProtocol,
				"invalid Gemini File expiration: %w",
				err,
			)
		}
		poll.Job.ExpiresAt = &expiry
	}
	switch file.State {
	case "", "STATE_UNSPECIFIED", "PROCESSING":
		return poll, nil
	case "FAILED":
		poll.State, poll.Failure = media.JobFailed, file.Error.Message
		if poll.Failure == "" {
			poll.Failure = fmt.Sprintf(
				"Gemini File processing failed with code %d",
				file.Error.Code,
			)
		}
		return poll, nil
	case "ACTIVE":
		poll.State = media.JobSucceeded
	default:
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"unknown Gemini File state %q",
			file.State,
		)
	}
	mimeType := file.MIMEType
	if mimeType == "" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"Gemini output File has non-video MIME type %q",
			mimeType,
		)
	}
	var duration time.Duration
	if file.VideoMetadata.VideoDuration != "" {
		duration, err = time.ParseDuration(file.VideoMetadata.VideoDuration)
		if err != nil || duration < 0 {
			return media.JobPoll{}, p.client.failure(
				media.ErrorProtocol,
				"invalid Gemini video duration %q",
				file.VideoMetadata.VideoDuration,
			)
		}
	}
	downloadURI := file.DownloadURI
	if downloadURI == "" {
		downloadURI = p.fileURL(job.ID, true)
	}
	if name, err := p.fileNameFromURI(downloadURI); err != nil || name != job.ID {
		return media.JobPoll{}, p.client.failure(
			media.ErrorProtocol,
			"Gemini download URI does not match output File",
		)
	}
	block := content.VideoBlock(content.Source{
		URI:      downloadURI,
		MIMEType: mimeType,
		Origin:   job.Origin,
	})
	block.Video.Duration = duration
	poll.Content = []content.Block{block}
	return poll, nil
}

// FetchVideo downloads a File observed ACTIVE by PollVideo. It never treats a
// URI returned by generation alone as proof that bytes are ready.
func (p *InteractionsVideoJobClient) FetchVideo(
	ctx context.Context,
	poll media.JobPoll,
) (media.Result, error) {
	if err := poll.Validate(); err != nil {
		return media.Result{}, p.client.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if err := p.validateJob(poll.Job); err != nil {
		return media.Result{}, err
	}
	if poll.State != media.JobSucceeded || len(poll.Content) != 1 {
		return media.Result{}, p.client.failure(
			media.ErrorInvalidRequest,
			"fetch requires one ACTIVE Gemini File output",
		)
	}
	block := poll.Content[0]
	if block.Kind != content.KindVideo ||
		block.Video.Source.Origin != poll.Job.Origin {
		return media.Result{}, p.client.failure(
			media.ErrorInvalidRequest,
			"Gemini output reference belongs to another destination",
		)
	}
	if name, err := p.fileNameFromURI(block.Video.Source.URI); err != nil ||
		name != poll.Job.ID {
		return media.Result{}, p.client.failure(
			media.ErrorInvalidRequest,
			"Gemini output reference does not match File handle",
		)
	}
	endpoint := p.fileURL(poll.Job.ID, true)
	data, err := p.getFile(ctx, endpoint, maxVideoBytes, true)
	if err != nil {
		return media.Result{}, err
	}
	output := content.VideoBlock(content.Source{
		Data:     data,
		MIMEType: block.Video.Source.MIMEType,
	})
	output.Video.Duration = block.Video.Duration
	result := media.Result{
		Kind: media.KindVideo,
		Requested: model.ModelRef{
			Provider: p.Provider(),
			Model:    poll.Job.Origin.Model,
		},
		API:     InteractionsAPI,
		Content: []content.Block{output},
	}
	if err := result.Validate(); err != nil {
		return media.Result{}, p.client.failure(media.ErrorProtocol, "%w", err)
	}
	return result, nil
}

func (p *InteractionsVideoJobClient) validateJob(job media.Job) error {
	if job.Kind != media.KindVideo ||
		job.Origin != p.client.origin(job.Origin.Model) ||
		!validGeminiFileName(job.ID) {
		return p.client.failure(
			media.ErrorInvalidRequest,
			"video job belongs to another Files destination",
		)
	}
	return nil
}

func (p *InteractionsVideoJobClient) fileURL(
	name string,
	download bool,
) string {
	base, _ := url.Parse(p.client.config.Endpoint)
	base.Path = strings.TrimSuffix(base.Path, "/interactions") + "/" + name
	if download {
		base.Path += ":download"
		base.RawQuery = "alt=media"
	}
	return base.String()
}

func (p *InteractionsVideoJobClient) fileNameFromURI(
	raw string,
) (string, error) {
	u, err := url.Parse(raw)
	base, _ := url.Parse(p.client.config.Endpoint)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawPath != "" ||
		strings.Contains(u.EscapedPath(), "%") {
		return "", p.client.failure(
			media.ErrorUnsupported,
			"invalid Gemini Files URI",
		)
	}
	path := u.Path
	if u.IsAbs() {
		if u.Scheme != base.Scheme || u.Host != base.Host {
			return "", p.client.failure(
				media.ErrorUnsupported,
				"Gemini Files URI is outside the configured destination",
			)
		}
		prefix := strings.TrimSuffix(base.Path, "/interactions") + "/"
		if !strings.HasPrefix(path, prefix) {
			return "", p.client.failure(
				media.ErrorUnsupported,
				"Gemini Files URI is outside the configured API path",
			)
		}
		path = strings.TrimPrefix(path, prefix)
	} else if u.Host != "" || strings.HasPrefix(path, "/") {
		return "", p.client.failure(
			media.ErrorUnsupported,
			"invalid relative Gemini Files URI",
		)
	}
	download := strings.HasSuffix(path, ":download")
	if download {
		path = strings.TrimSuffix(path, ":download")
	}
	if !validGeminiFileName(path) ||
		(download && u.RawQuery != "" && u.RawQuery != "alt=media") ||
		(!download && u.RawQuery != "") {
		return "", p.client.failure(
			media.ErrorUnsupported,
			"invalid Gemini Files URI",
		)
	}
	return path, nil
}

func validGeminiFileName(name string) bool {
	if !strings.HasPrefix(name, "files/") {
		return false
	}
	id := strings.TrimPrefix(name, "files/")
	if len(id) < 1 || len(id) > 40 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for _, c := range id {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (p *InteractionsVideoJobClient) getFile(
	ctx context.Context,
	endpoint string,
	maxBytes int64,
	expectVideo bool,
) ([]byte, error) {
	return doGeminiMedia(
		ctx,
		p.Provider(),
		p.client.config.APIKey,
		p.client.config.Client,
		http.MethodGet,
		endpoint,
		nil,
		"",
		maxBytes,
		expectVideo,
	)
}
