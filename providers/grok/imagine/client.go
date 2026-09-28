package imagine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

var (
	_ media.ImageClient    = (*Client)(nil)
	_ media.VideoJobClient = (*Client)(nil)
)

// PrepareImage validates an image operation and captures the exact JSON body
// without contacting xAI. Edit inputs may contain inline bytes as data URLs.
func (c *Client) PrepareImage(
	ctx context.Context,
	input media.ImageInput,
) (media.ImageRequest, error) {
	return c.PrepareImageWithOptions(ctx, input, ImageOptions{})
}

// PrepareImageWithOptions adds xAI's response format to an image request.
// The exact body and provider options are frozen before the call.
func (c *Client) PrepareImageWithOptions(
	ctx context.Context,
	input media.ImageInput,
	options ImageOptions,
) (media.ImageRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.ImageRequest{}, err
	}
	if err := input.Validate(); err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	body, _, err := imageWireBodyWithOptions(input, options, c.files)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	var providerOptions json.RawMessage
	if options.ResponseFormat != "" || options.Storage != nil {
		providerOptions, err = json.Marshal(options)
		if err != nil {
			return media.ImageRequest{}, c.failure(media.ErrorProtocol, err)
		}
	}
	var info *media.ModelInfo
	capabilities := media.ImageCapabilities{}
	if known, ok := c.models[input.Model]; ok &&
		known.Image != nil &&
		known.Evidence.API == ImageAPI {
		copy := known.Clone()
		info = &copy
		capabilities = *copy.Image
	}
	unverified, err := capabilities.Check(input, c.unknown)
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	request, err := media.NewImageRequest(media.ImageRequestSnapshot{
		Origin:           c.origin(ImageAPI, input.Model),
		Input:            input,
		Effective:        input.Settings,
		Body:             body,
		ContentType:      "application/json",
		ProviderOptions:  providerOptions,
		ModelInfo:        info,
		CapabilityPolicy: c.unknown,
		Unverified:       unverified,
	})
	if err != nil {
		return media.ImageRequest{}, c.failure(media.ErrorProtocol, err)
	}
	return request, nil
}

// SendImage makes one image generation or edit call. It does not retry, follow
// redirects, or fetch temporary image URLs from the response.
func (c *Client) SendImage(
	ctx context.Context,
	request media.ImageRequest,
) (media.Result, error) {
	input := request.Input()
	if request.Origin() != c.origin(ImageAPI, input.Model) {
		return media.Result{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("prepared image request belongs to another destination"),
		)
	}
	path := "/images/generations"
	if input.Operation == media.ImageEdit {
		path = "/images/edits"
	}
	raw, err := c.do(
		ctx,
		http.MethodPost,
		path,
		request.Body(),
		request.ContentType(),
	)
	if err != nil {
		return media.Result{}, err
	}
	result, err := decodeImageWireWithFiles(raw, c.provider, input.Model, c.files)
	if err != nil {
		return media.Result{}, c.failure(media.ErrorProtocol, err)
	}
	return result, nil
}

// PrepareVideo validates an xAI video request and captures its exact JSON
// submission body. At most one reference frame is accepted as the first frame.
func (c *Client) PrepareVideo(
	ctx context.Context,
	input media.VideoInput,
) (media.VideoRequest, error) {
	return c.PrepareVideoWithOptions(ctx, input, VideoOptions{})
}

// PrepareVideoWithOptions adds xAI-specific reference, frame, and audio
// controls. The options and exact wire body are captured in the request.
func (c *Client) PrepareVideoWithOptions(
	ctx context.Context,
	input media.VideoInput,
	options VideoOptions,
) (media.VideoRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.VideoRequest{}, err
	}
	if err := input.Validate(); err != nil {
		return media.VideoRequest{}, c.failure(media.ErrorInvalidRequest, err)
	}
	body, err := videoWireBodyWithOptions(input, options, c.files)
	if err != nil {
		return media.VideoRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	var providerOptions json.RawMessage
	if optionsHasValues(options) {
		providerOptions, err = json.Marshal(options)
		if err != nil {
			return media.VideoRequest{}, c.failure(media.ErrorProtocol, err)
		}
	}
	var info *media.ModelInfo
	capabilities := media.VideoCapabilities{}
	if known, ok := c.models[input.Model]; ok &&
		known.Video != nil &&
		known.Evidence.API == VideoAPI {
		copy := known.Clone()
		info = &copy
		capabilities = *copy.Video
	}
	unverified, err := capabilities.Check(input, c.unknown)
	if err != nil {
		return media.VideoRequest{}, c.failure(media.ErrorUnsupported, err)
	}
	request, err := media.NewVideoRequest(media.VideoRequestSnapshot{
		Origin:           c.origin(VideoAPI, input.Model),
		Input:            input,
		Effective:        input.Settings,
		Body:             body,
		ContentType:      "application/json",
		ProviderOptions:  providerOptions,
		ModelInfo:        info,
		CapabilityPolicy: c.unknown,
		Unverified:       unverified,
	})
	if err != nil {
		return media.VideoRequest{}, c.failure(media.ErrorProtocol, err)
	}
	return request, nil
}

// SubmitVideo makes one remote submission attempt. Transport or server failure
// can leave acceptance unknown; callers must record that uncertainty.
func (c *Client) SubmitVideo(
	ctx context.Context,
	request media.VideoRequest,
) (media.Submission, error) {
	input := request.Input()
	if request.Origin() != c.origin(VideoAPI, input.Model) {
		return media.Submission{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("prepared video request belongs to another destination"),
		)
	}
	if err := ctx.Err(); err != nil {
		return media.Submission{}, err
	}
	path := "/videos/generations"
	switch input.Operation {
	case media.VideoEdit:
		path = "/videos/edits"
	case media.VideoExtend:
		path = "/videos/extensions"
	}
	raw, err := c.do(
		ctx,
		http.MethodPost,
		path,
		request.Body(),
		request.ContentType(),
	)
	if err != nil {
		state := media.SubmissionUnknown
		failure, ok := errors.AsType[*media.Error](err)
		if ok && knownRejectedStatus(failure.Status) {
			state = media.SubmissionRejected
		}
		return media.Submission{State: state}, err
	}
	var response struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(
		raw,
		&response,
	); err != nil || strings.TrimSpace(response.RequestID) == "" {
		return media.Submission{State: media.SubmissionUnknown},
			c.failure(
				media.ErrorProtocol,
				errors.New("video submission returned no usable request_id"),
			)
	}
	job := media.Job{
		ID:     response.RequestID,
		Kind:   media.KindVideo,
		Origin: request.Origin(),
	}
	return media.Submission{State: media.SubmissionAccepted, Job: &job}, nil
}

// A status that may be produced after admission leaves the job outcome
// unknown.
// xAI does not document idempotent video submission or reconciliation by
// token.
func knownRejectedStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly,
		http.StatusTooManyRequests:
		return false
	}
	return true
}

// PollVideo makes one status request for an accepted job. It does not wait or
// schedule another poll. Completed output is a temporary URL reference.
func (c *Client) PollVideo(
	ctx context.Context,
	job media.Job,
) (media.JobPoll, error) {
	if err := job.Validate(); err != nil {
		return media.JobPoll{}, c.failure(media.ErrorInvalidRequest, err)
	}
	if job.Kind != media.KindVideo || job.Origin != c.origin(
		VideoAPI,
		job.Origin.Model,
	) {
		return media.JobPoll{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("video job belongs to another destination"),
		)
	}
	raw, err := c.do(
		ctx,
		http.MethodGet,
		"/videos/"+url.PathEscape(job.ID),
		nil,
		"",
	)
	if err != nil {
		return media.JobPoll{}, err
	}
	wire, err := decodeVideoWire(raw)
	if err != nil {
		return media.JobPoll{}, c.failure(media.ErrorProtocol, err)
	}
	poll := media.JobPoll{Job: job, RawUsage: wire.Usage}
	if wire.Model != "" {
		poll.Served = c.modelRef(wire.Model)
	}
	switch wire.Status {
	case "pending":
		poll.State = media.JobPending
	case "done":
		if wire.Video.RespectModeration != nil && !*wire.Video.RespectModeration {
			poll.State = media.JobFailed
			poll.Failure = "video filtered by provider moderation"
			break
		}
		poll.State = media.JobSucceeded
		var duration time.Duration
		if wire.Video.Duration != nil {
			duration = time.Duration(*wire.Video.Duration * float64(time.Second))
		}
		block := content.VideoBlock(content.Source{
			URL:      wire.Video.URL,
			MIMEType: "video/mp4",
		})
		block.Video.Duration = duration
		poll.Content = []content.Block{
			block,
		}
		if wire.Video.FileOutput != nil {
			stored, err := wire.Video.FileOutput.storedOutput(0, "video/mp4", c.files)
			if err != nil {
				return media.JobPoll{}, c.failure(media.ErrorProtocol, err)
			}
			poll.StoredOutputs = []media.StoredOutput{stored}
		}
	case "failed":
		poll.State = media.JobFailed
		poll.Failure = providerFailure(wire.Error)
	case "expired":
		poll.State = media.JobExpired
		poll.Failure = providerFailure(wire.Error)
	}
	if err := poll.Validate(); err != nil {
		return media.JobPoll{}, c.failure(media.ErrorProtocol, err)
	}
	return poll, nil
}

// FetchVideo turns the completed status's URL into a media result. xAI returns
// output in its poll response; the host decides if and how to persist the URL.
func (c *Client) FetchVideo(
	ctx context.Context,
	poll media.JobPoll,
) (media.Result, error) {
	if err := ctx.Err(); err != nil {
		return media.Result{}, err
	}
	if err := poll.Validate(); err != nil {
		return media.Result{}, c.failure(media.ErrorInvalidRequest, err)
	}
	if poll.Job.Kind != media.KindVideo || poll.Job.Origin != c.origin(
		VideoAPI,
		poll.Job.Origin.Model,
	) {
		return media.Result{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("video job belongs to another destination"),
		)
	}
	if poll.State != media.JobSucceeded || len(poll.Content) == 0 {
		return media.Result{}, c.failure(
			media.ErrorInvalidRequest,
			errors.New("video output is not ready"),
		)
	}
	copy := poll.Clone()
	result := media.Result{
		Kind:          media.KindVideo,
		Content:       copy.Content,
		StoredOutputs: copy.StoredOutputs,
		Requested:     c.modelRef(poll.Job.Origin.Model),
		Served:        poll.Served,
		API:           VideoAPI,
		ResponseID:    poll.Job.ID,
		RawUsage:      copy.RawUsage,
	}
	if err := result.Validate(); err != nil {
		return media.Result{}, c.failure(media.ErrorProtocol, err)
	}
	return result, nil
}

func (c *Client) modelRef(name string) model.ModelRef {
	return model.ModelRef{Provider: c.provider, Model: name}
}

func (c *Client) failure(category media.ErrorCategory, cause error) error {
	return &media.Error{Provider: c.provider, Category: category, Cause: cause}
}

// do sends exactly one HTTP request and bounds the response held in memory.
func (c *Client) do(
	ctx context.Context,
	method, path string,
	body []byte,
	contentType string,
) ([]byte, error) {
	if c.noKey != nil {
		return nil, c.failure(media.ErrorAuthentication, c.noKey)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	httpClient := http.Client{Timeout: 5 * time.Minute}
	if c.http != nil {
		httpClient = *c.http
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		c.endpoint+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, c.failure(media.ErrorInvalidRequest, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, c.failure(media.ErrorTransport, err)
	}
	defer response.Body.Close()
	const maxResponseBytes = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.failure(media.ErrorTransport, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, c.failure(
			media.ErrorProtocol,
			errors.New("provider response exceeds 32 MiB"),
		)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		providerError := providerhttp.NewError(
			response.StatusCode,
			string(raw),
			providerhttp.WithErrorHeader(response.Header),
		)
		retryAfter, _ := providerError.RetryAfter()
		category := media.ErrorInvalidRequest
		switch {
		case response.StatusCode == 401 || response.StatusCode == 403:
			category = media.ErrorAuthentication
		case response.StatusCode == 429:
			category = media.ErrorRateLimited
		case response.StatusCode >= 500:
			category = media.ErrorOverloaded
		}
		return nil, &media.Error{
			Provider:   c.provider,
			Category:   category,
			Status:     response.StatusCode,
			Code:       providerError.Code(),
			Body:       raw,
			RetryAfter: retryAfter,
			Retryable:  response.StatusCode == 429 || response.StatusCode >= 500,
		}
	}
	return raw, nil
}

func providerFailure(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return message
	}
	var object struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Message != "" {
		return object.Message
	}
	return strings.TrimSpace(string(raw))
}
