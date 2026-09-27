package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/deepnoodle-ai/kernel/media"
)

func (p *InteractionsMediaClient) videoEvidence(in media.VideoInput) (*media.ModelInfo, []string, error) {
	info, known := p.VideoModelInfo(in.Model)
	if !known {
		unverified, err := (media.VideoCapabilities{}).Check(in, p.config.UnknownCapabilities)
		return nil, unverified, err
	}
	unverified, err := info.Video.Check(in, p.config.UnknownCapabilities)
	return &info, unverified, err
}

// PrepareVideo encodes a stateless synchronous Omni generation call. A single
// reference frame means an initial image for image-to-video; other frame roles
// need explicit provider options and are not inferred from slice order.
func (p *InteractionsMediaClient) PrepareVideo(ctx context.Context, in media.VideoInput) (media.VideoRequest, error) {
	return p.prepareVideo(ctx, in, "inline")
}

func (p *InteractionsMediaClient) prepareVideo(ctx context.Context, in media.VideoInput, delivery string) (media.VideoRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.VideoRequest{}, err
	}
	if err := in.Validate(); err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if in.Operation != "" && in.Operation != media.VideoGenerate {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Interactions video %s requires an explicit video source contract", in.Operation)
	}
	if strings.HasPrefix(in.Model, "veo-") || strings.HasPrefix(in.Model, "imagen-") ||
		strings.HasSuffix(in.Model, "-image") {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "model %q is not an Omni video model", in.Model)
	}
	if in.Settings.Size.Width != 0 || in.Settings.Duration != 0 || len(in.ReferenceFrames) > 1 {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Omni exact size, explicit duration, or ambiguous multiple reference frames")
	}
	if ratio := in.Settings.AspectRatio; ratio != "" && ratio != "16:9" && ratio != "9:16" {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Omni aspect ratio %q", ratio)
	}
	if resolution := in.Settings.Resolution; resolution != "" && resolution != "360p" &&
		resolution != "720p" && resolution != "1080p" && resolution != "4k" {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Omni resolution %q", resolution)
	}
	if delivery == "inline" && (in.Settings.Resolution == "1080p" || in.Settings.Resolution == "4k") {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "Omni high resolution requires URI delivery")
	}
	if len(in.Prompt) == 0 && len(in.ReferenceFrames) == 0 {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "Omni requires a prompt or initial image")
	}
	info, unverified, err := p.videoEvidence(in)
	if err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	input := make([]any, 0, len(in.Prompt)+len(in.ReferenceFrames))
	for _, block := range in.Prompt {
		part, err := p.inputPart(block)
		if err != nil {
			return media.VideoRequest{}, err
		}
		input = append(input, part)
	}
	for _, block := range in.ReferenceFrames {
		part, err := p.inputPart(block)
		if err != nil {
			return media.VideoRequest{}, err
		}
		input = append(input, part)
	}
	format := map[string]any{"type": "video", "delivery": delivery}
	if in.Settings.AspectRatio != "" {
		format["aspect_ratio"] = in.Settings.AspectRatio
	}
	if in.Settings.Resolution != "" {
		format["resolution"] = in.Settings.Resolution
	}
	bodyValue := map[string]any{
		"model": in.Model, "input": input, "response_format": format,
		// URI delivery creates a provider File and requires stored interaction
		// state; inline delivery keeps the call stateless.
		"background": false, "store": delivery == "uri", "stream": false,
	}
	if len(in.ReferenceFrames) == 1 {
		bodyValue["generation_config"] = map[string]any{"video_config": map[string]any{"task": "image_to_video"}}
	}
	body, err := json.Marshal(bodyValue)
	if err != nil {
		return media.VideoRequest{}, err
	}
	return media.NewVideoRequest(media.VideoRequestSnapshot{Origin: p.origin(in.Model),
		Input: in, Effective: in.Settings, Body: body, ContentType: "application/json",
		ModelInfo: info, UnknownCapabilities: p.config.UnknownCapabilities, Unverified: unverified})
}

// SendVideo dispatches one synchronous Interactions video call.
func (p *InteractionsMediaClient) SendVideo(ctx context.Context, request media.VideoRequest) (media.Result, error) {
	in := request.Input()
	if request.Origin() != p.origin(in.Model) || request.ContentType() != "application/json" {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "prepared video request belongs to another destination")
	}
	if delivery, err := preparedVideoDelivery(request.Body()); err != nil || delivery != "inline" {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "prepared video request is not an inline Interactions call")
	}
	raw, err := p.do(ctx, request.Body())
	if err != nil {
		return media.Result{}, err
	}
	return p.decode(raw, media.KindVideo, in.Model)
}

func preparedVideoDelivery(body []byte) (string, error) {
	var request struct {
		ResponseFormat struct {
			Delivery string `json:"delivery"`
			Type     string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return "", err
	}
	if request.ResponseFormat.Type != "video" {
		return "", errors.New("prepared Interactions request is not video")
	}
	return request.ResponseFormat.Delivery, nil
}
