package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

func (p *MediaClient) videoEvidence(in media.VideoInput) (*media.ModelInfo, []string, error) {
	info, known := p.VideoModelInfo(in.Model)
	if !known {
		unverified, err := (media.VideoCapabilities{}).Check(in, p.config.UnknownCapabilities)
		return nil, unverified, err
	}
	unverified, err := info.Video.Check(in, p.config.UnknownCapabilities)
	return &info, unverified, err
}

// PrepareVideo encodes a Veo text or initial-frame request without dispatch.
func (p *MediaClient) PrepareVideo(ctx context.Context, in media.VideoInput) (media.VideoRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.VideoRequest{}, err
	}
	if err := in.Validate(); err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if in.Operation != "" && in.Operation != media.VideoGenerate {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo %s requires an explicit video source contract", in.Operation)
	}
	if strings.HasPrefix(in.Model, "veo-3.0-") || strings.HasPrefix(in.Model, "veo-2.0-") {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo 2 and Veo 3 models are retired from the Gemini API")
	}
	if in.Settings.Size.Width != 0 || len(in.ReferenceFrames) > 1 {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo exact size or ambiguous multiple reference frames")
	}
	if in.Settings.AspectRatio != "" && in.Settings.AspectRatio != "16:9" && in.Settings.AspectRatio != "9:16" {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo aspect ratio %q", in.Settings.AspectRatio)
	}
	if d := in.Settings.Duration; d != 0 && d != 4*time.Second && d != 6*time.Second && d != 8*time.Second {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo duration %s", d)
	}
	if resolution := in.Settings.Resolution; resolution != "" && resolution != "720p" &&
		resolution != "1080p" && resolution != "4k" {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo resolution %q", resolution)
	}
	if (in.Settings.Resolution == "1080p" || in.Settings.Resolution == "4k") &&
		in.Settings.Duration != 8*time.Second {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo high resolution requires an explicit eight-second duration")
	}
	if in.Model == "veo-3.1-lite-generate-preview" && in.Settings.Resolution == "4k" {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo 3.1 Lite does not support 4k")
	}
	info, unverified, err := p.videoEvidence(in)
	if err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	var prompt []string
	for _, b := range in.Prompt {
		if b.Kind != content.KindText || len(b.Text.Citations) > 0 {
			return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Veo prompt requires uncited text")
		}
		prompt = append(prompt, b.Text.Text)
	}
	if len(in.ReferenceFrames) == 0 && strings.TrimSpace(strings.Join(prompt, "")) == "" {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "Veo requires a nonempty text prompt or initial frame")
	}
	instance := map[string]any{}
	if len(prompt) > 0 {
		instance["prompt"] = strings.Join(prompt, "\n\n")
	}
	if len(in.ReferenceFrames) == 1 {
		frame, err := p.inlineImage(in.ReferenceFrames[0].Image.Source)
		if err != nil {
			return media.VideoRequest{}, err
		}
		instance["image"] = frame
	}
	parameters := map[string]any{}
	if in.Settings.AspectRatio != "" {
		parameters["aspectRatio"] = in.Settings.AspectRatio
	}
	if in.Settings.Duration != 0 {
		parameters["durationSeconds"] = int(in.Settings.Duration / time.Second)
	}
	if in.Settings.Resolution != "" {
		parameters["resolution"] = in.Settings.Resolution
	}
	bodyValue := map[string]any{"instances": []any{instance}}
	if len(parameters) > 0 {
		bodyValue["parameters"] = parameters
	}
	body, err := json.Marshal(bodyValue)
	if err != nil {
		return media.VideoRequest{}, err
	}
	return media.NewVideoRequest(media.VideoRequestSnapshot{Origin: p.videoOrigin(in.Model),
		Input: in, Effective: in.Settings, Body: body, ContentType: "application/json",
		ModelInfo: info, UnknownCapabilities: p.config.UnknownCapabilities, Unverified: unverified})
}

// SubmitVideo dispatches the prepared Veo job once. Any transport failure or
// server-side ambiguity returns SubmissionUnknown alongside the error.
func (p *MediaClient) SubmitVideo(ctx context.Context, prepared media.VideoRequest) (media.Submission, error) {
	in := prepared.Input()
	if prepared.Origin() != p.videoOrigin(in.Model) || prepared.ContentType() != "application/json" {
		return media.Submission{State: media.SubmissionRejected}, p.failure(media.ErrorInvalidRequest, "prepared video request belongs to another destination")
	}
	raw, err := p.do(ctx, http.MethodPost, p.videoURL(in.Model), prepared.Body(), prepared.ContentType(), maxOperationBytes, false)
	if err != nil {
		var providerErr *media.Error
		if errors.As(err, &providerErr) && providerErr.Status >= 400 &&
			providerErr.Status < 500 && providerErr.Status != 408 && providerErr.Status != 409 &&
			providerErr.Status != 425 && providerErr.Status != 429 {
			return media.Submission{State: media.SubmissionRejected}, err
		}
		return media.Submission{State: media.SubmissionUnknown}, err
	}
	var response struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Name == "" {
		return media.Submission{State: media.SubmissionUnknown}, p.failure(media.ErrorProtocol, "Veo accepted response has no operation name: %v", err)
	}
	if _, err := p.operationURL(in.Model, response.Name); err != nil {
		return media.Submission{State: media.SubmissionUnknown}, err
	}
	job := media.Job{ID: response.Name, Kind: media.KindVideo, Origin: p.videoOrigin(in.Model)}
	return media.Submission{State: media.SubmissionAccepted, Job: &job}, nil
}

// PollVideo reads one Veo operation status and never resubmits the job.
func (p *MediaClient) PollVideo(ctx context.Context, job media.Job) (media.JobPoll, error) {
	if job.Kind != media.KindVideo || job.Origin != p.videoOrigin(job.Origin.Model) {
		return media.JobPoll{}, p.failure(media.ErrorInvalidRequest, "video job belongs to another destination")
	}
	endpoint, err := p.operationURL(job.Origin.Model, job.ID)
	if err != nil {
		return media.JobPoll{}, err
	}
	raw, err := p.do(ctx, http.MethodGet, endpoint, nil, "", maxOperationBytes, false)
	if err != nil {
		return media.JobPoll{}, err
	}
	var response struct {
		Name  string `json:"name"`
		Done  bool   `json:"done"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			GenerateVideoResponse struct {
				GeneratedSamples []struct {
					Video struct {
						URI      string `json:"uri"`
						MIMEType string `json:"mimeType"`
					} `json:"video"`
				} `json:"generatedSamples"`
			} `json:"generateVideoResponse"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return media.JobPoll{}, p.failure(media.ErrorProtocol, "decode Veo operation: %w", err)
	}
	if response.Name != "" && response.Name != job.ID {
		return media.JobPoll{}, p.failure(media.ErrorProtocol, "Veo operation identity changed")
	}
	poll := media.JobPoll{Job: job.Clone(), State: media.JobPending}
	if !response.Done {
		return poll, nil
	}
	if response.Error != nil {
		poll.State = media.JobFailed
		poll.Failure = response.Error.Message
		if poll.Failure == "" {
			poll.Failure = fmt.Sprintf("Veo operation failed with code %d", response.Error.Code)
		}
		return poll, nil
	}
	poll.State = media.JobSucceeded
	for _, sample := range response.Response.GenerateVideoResponse.GeneratedSamples {
		if sample.Video.URI == "" {
			return media.JobPoll{}, p.failure(media.ErrorProtocol, "Veo output has no URI")
		}
		if _, err := p.downloadURL(sample.Video.URI); err != nil {
			return media.JobPoll{}, err
		}
		mime := sample.Video.MIMEType
		if mime == "" {
			mime = "video/mp4"
		}
		if !strings.HasPrefix(mime, "video/") {
			return media.JobPoll{}, p.failure(media.ErrorProtocol, "Veo output has non-video MIME type %q", mime)
		}
		poll.Content = append(poll.Content, content.VideoBlock(content.Source{URI: sample.Video.URI,
			MIMEType: mime, Origin: job.Origin}))
	}
	if len(poll.Content) == 0 {
		return media.JobPoll{}, p.failure(media.ErrorProtocol, "completed Veo job has no generated video")
	}
	return poll, nil
}

// FetchVideo downloads each completed Veo output into caller-owned blocks.
// The host chooses whether and where to persist those bytes.
func (p *MediaClient) FetchVideo(ctx context.Context, poll media.JobPoll) (media.Result, error) {
	if err := poll.Validate(); err != nil {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	job := poll.Job
	if poll.State != media.JobSucceeded || job.Kind != media.KindVideo ||
		job.Origin != p.videoOrigin(job.Origin.Model) {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "fetch requires a completed local Veo job")
	}
	if len(poll.Content) == 0 {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "completed Veo job has no output references")
	}
	result := media.Result{Kind: media.KindVideo, Requested: model.ModelRef{Provider: p.Provider(), Model: job.Origin.Model},
		API: VideoAPI, ResponseID: job.ID}
	for _, block := range poll.Content {
		if block.Kind != content.KindVideo || block.Video == nil || block.Video.Source.Origin != job.Origin {
			return media.Result{}, p.failure(media.ErrorInvalidRequest, "Veo output reference belongs to another destination")
		}
		source := block.Video.Source
		endpoint, err := p.downloadURL(source.URI)
		if err != nil {
			return media.Result{}, err
		}
		data, err := p.do(ctx, http.MethodGet, endpoint, nil, "", maxVideoBytes, true)
		if err != nil {
			return media.Result{}, err
		}
		result.Content = append(result.Content, content.VideoBlock(content.Source{Data: data, MIMEType: source.MIMEType}))
	}
	if err := result.Validate(); err != nil {
		return media.Result{}, p.failure(media.ErrorProtocol, "%w", err)
	}
	return result, nil
}

func (p *MediaClient) operationURL(name, operation string) (string, error) {
	if strings.Contains(operation, "..") || strings.ContainsAny(operation, "?#%\\") ||
		path.Clean(operation) != operation || strings.HasPrefix(operation, "/") {
		return "", p.failure(media.ErrorProtocol, "invalid Veo operation name")
	}
	prefix := "models/" + name + "/operations/"
	var operationID string
	switch {
	case strings.HasPrefix(operation, prefix):
		operationID = strings.TrimPrefix(operation, prefix)
	case strings.HasPrefix(operation, "operations/"):
		operationID = strings.TrimPrefix(operation, "operations/")
	default:
		return "", p.failure(media.ErrorProtocol, "Veo operation belongs to another model")
	}
	if operationID == "" || strings.Contains(operationID, "/") ||
		url.PathEscape(operationID) != operationID {
		return "", p.failure(media.ErrorProtocol, "invalid Veo operation name")
	}
	u, _ := url.Parse(p.videoURL(name))
	u.Path = strings.TrimSuffix(u.Path, "/models/"+url.PathEscape(name)+":predictLongRunning") + "/" + operation
	return u.String(), nil
}

func (p *MediaClient) downloadURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	base, _ := url.Parse(p.config.VideoEndpoint)
	filesPrefix := strings.TrimSuffix(base.Path, "/models/{model}:predictLongRunning") + "/files/"
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" ||
		!strings.HasPrefix(u.Path, filesPrefix) || u.RawQuery != "" ||
		strings.Contains(u.Path, "..") || strings.Contains(u.EscapedPath(), "%") ||
		path.Base(u.Path) == "" || strings.Contains(path.Base(u.Path), ":") {
		return "", p.failure(media.ErrorUnsupported, "Veo output URI is outside the configured Files API")
	}
	u.Path += ":download"
	u.RawQuery = "alt=media"
	return u.String(), nil
}
