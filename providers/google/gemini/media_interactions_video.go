package gemini

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func (p *InteractionsMediaClient) videoEvidence(
	in media.VideoInput,
) (*media.ModelInfo, []string, error) {
	info, known := p.VideoModelInfo(in.Model)
	if !known {
		unverified, err := (media.VideoCapabilities{}).Check(
			in,
			p.config.CapabilityPolicy,
		)
		return nil, unverified, err
	}
	unverified, err := info.Video.Check(in, p.config.CapabilityPolicy)
	return &info, unverified, err
}

// PrepareVideo encodes a stateless synchronous Omni call. FirstFrame, with an
// optional LastFrame, makes an image-to-video request; ReferenceImages and
// ReferenceVideos make a reference-to-video request; SourceVideo with the edit
// or extend operation makes an edit or an extension.
func (p *InteractionsMediaClient) PrepareVideo(
	ctx context.Context,
	in media.VideoInput,
) (media.VideoRequest, error) {
	return p.prepareVideo(ctx, in, "inline")
}

// Omni limits from https://aistudio.google.com/docs/omni (2026-11-17).
const (
	omniMaxReferenceImages    = 6
	omniMaxReferenceVideoSpan = 3 * time.Second
	omniMaxEditSource         = 15 * time.Second
	omniMaxExtendedVideo      = 30 * time.Second
	omniDefaultDuration       = 10 * time.Second
	// The longest output with a last frame, and the most one extension adds.
	omniMaxShortDuration = 10 * time.Second
)

func (p *InteractionsMediaClient) prepareVideo(
	ctx context.Context,
	in media.VideoInput,
	delivery string,
) (media.VideoRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.VideoRequest{}, err
	}
	if err := in.Validate(); err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if in.Model == "gemini-omni-flash-preview" {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"gemini-omni-flash-preview was shut down on 2026-10-22; use gemini-omni-1.2-flash",
		)
	}
	if strings.HasPrefix(
		in.Model,
		"veo-",
	) || strings.HasPrefix(
		in.Model,
		"imagen-",
	) ||
		strings.HasSuffix(in.Model, "-image") {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"model %q is not an Omni video model",
			in.Model,
		)
	}
	// Omni always makes native audio; only the prompt can ask for quiet.
	if in.Settings.DisableAudio {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni video cannot turn audio off; describe the sound you want in the prompt",
		)
	}
	if in.Settings.Size.Width != 0 {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni exact size; use resolution and aspect ratio",
		)
	}
	// gemini-omni-1.2-flash takes 3 to 15 s; 1.1 takes up to 10 s until it
	// routes to 1.2 on 2026-12-03, and the API enforces that.
	if d := in.Settings.Duration; d != 0 &&
		(d < 3*time.Second || d > 15*time.Second || d%time.Second != 0) {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni duration %s; use whole seconds from 3 to 15",
			d,
		)
	}
	if ratio := in.Settings.AspectRatio; ratio != "" &&
		ratio != "16:9" &&
		ratio != "9:16" {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni aspect ratio %q",
			ratio,
		)
	}
	if resolution := in.Settings.Resolution; resolution != "" &&
		resolution != "360p" &&
		resolution != "720p" && resolution != "1080p" && resolution != "4k" {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni resolution %q",
			resolution,
		)
	}
	if delivery == "inline" &&
		(in.Settings.Resolution == "1080p" ||
			in.Settings.Resolution == "4k") {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Omni high resolution requires URI delivery",
		)
	}
	task, err := p.omniTask(in)
	if err != nil {
		return media.VideoRequest{}, err
	}
	info, unverified, err := p.videoEvidence(in)
	if err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	input, err := p.omniInput(in)
	if err != nil {
		return media.VideoRequest{}, err
	}
	format := map[string]any{"type": "video", "delivery": delivery}
	if in.Settings.AspectRatio != "" {
		format["aspect_ratio"] = in.Settings.AspectRatio
	}
	if in.Settings.Resolution != "" {
		format["resolution"] = in.Settings.Resolution
	}
	if in.Settings.Duration != 0 {
		format["duration"] = fmt.Sprintf("%ds", in.Settings.Duration/time.Second)
	}
	bodyValue := map[string]any{
		"model":           in.Model,
		"input":           input,
		"response_format": format,
		// URI delivery creates a provider File and requires stored interaction
		// state; inline delivery keeps the call stateless.
		"background": false,
		"store":      delivery == "uri",
		"stream":     false,
	}
	// Without task, Omni infers each image's role from the prompt. Dashi's
	// typed inputs already state the role, so task is sent whenever the input
	// carries media.
	if task != "" {
		bodyValue["generation_config"] = map[string]any{
			"video_config": map[string]any{"task": task},
		}
	}
	body, err := json.Marshal(bodyValue)
	if err != nil {
		return media.VideoRequest{}, err
	}
	return media.NewVideoRequest(media.VideoRequestSnapshot{
		Origin:           p.origin(in.Model),
		Input:            in,
		Effective:        in.Settings,
		Body:             body,
		ContentType:      "application/json",
		ModelInfo:        info,
		CapabilityPolicy: p.config.CapabilityPolicy,
		Unverified:       unverified,
	})
}

// omniTask checks the combination of inputs against Omni's documented limits
// and returns the video_config task, or "" for text-to-video.
func (p *InteractionsMediaClient) omniTask(in media.VideoInput) (string, error) {
	unsupported := func(format string, args ...any) (string, error) {
		return "", p.failure(media.ErrorUnsupported, format, args...)
	}
	if len(in.ReferenceImages) > omniMaxReferenceImages {
		return unsupported(
			"Omni takes at most %d reference images, not %d",
			omniMaxReferenceImages,
			len(in.ReferenceImages),
		)
	}
	duration := in.Settings.Duration
	if in.Operation == media.VideoEdit || in.Operation == media.VideoExtend {
		if in.SourceVideo == nil {
			return unsupported("Omni video %s requires SourceVideo", in.Operation)
		}
		if in.FirstFrame != nil || in.LastFrame != nil ||
			len(in.ReferenceVideos) > 0 {
			return unsupported(
				"Omni video %s takes reference images, not frames or reference videos",
				in.Operation,
			)
		}
		if in.Settings.AspectRatio != "" {
			return unsupported("Omni video %s keeps the source aspect ratio", in.Operation)
		}
		// Zero when the caller did not record the source length.
		source := in.SourceVideo.Video.Duration
		if in.Operation == media.VideoEdit {
			if duration != 0 {
				return unsupported("Omni video edit keeps the source duration")
			}
			if source > omniMaxEditSource {
				return unsupported(
					"Omni edits videos up to %s, not %s",
					omniMaxEditSource,
					source,
				)
			}
			return "edit", nil
		}
		// For extension, duration is the length added to the source.
		if duration > omniMaxShortDuration {
			return unsupported("Omni extension adds 3 to 10 s, not %s", duration)
		}
		added := cmp.Or(duration, omniDefaultDuration)
		if source > 0 && source+added > omniMaxExtendedVideo {
			return unsupported(
				"Omni extension of a %s video by %s exceeds %s",
				source,
				added,
				omniMaxExtendedVideo,
			)
		}
		return "extend", nil
	}
	if in.LastFrame != nil && in.FirstFrame == nil {
		return unsupported("Omni last frame requires a first frame")
	}
	frames := in.FirstFrame != nil
	references := len(in.ReferenceImages) > 0 || len(in.ReferenceVideos) > 0
	if frames && references {
		// Omni sends every image as a plain part, so it could not tell a
		// frame from a reference image.
		return unsupported("Omni cannot combine first or last frames with references")
	}
	if !frames &&
		strings.TrimSpace(resultText(media.Result{Content: in.Prompt})) == "" {
		return "", p.failure(
			media.ErrorInvalidRequest,
			"Omni requires a text prompt or first frame",
		)
	}
	if in.LastFrame != nil && duration > omniMaxShortDuration {
		return unsupported(
			"Omni first and last frames take 3 to 10 s, not %s",
			duration,
		)
	}
	if n := len(in.ReferenceVideos); n > 0 {
		limit := 3
		if cmp.Or(duration, omniDefaultDuration) > omniMaxShortDuration {
			limit = 1
		}
		if n > limit {
			return unsupported(
				"Omni takes at most %d reference videos at this duration, not %d",
				limit,
				n,
			)
		}
		for _, block := range in.ReferenceVideos {
			if block.Video.Duration > omniMaxReferenceVideoSpan {
				return unsupported(
					"Omni reference videos are up to %s, not %s",
					omniMaxReferenceVideoSpan,
					block.Video.Duration,
				)
			}
		}
	}
	switch {
	case frames:
		return "image_to_video", nil
	case references:
		return "reference_to_video", nil
	}
	return "", nil
}

// omniInput puts media before the prompt, as Google's examples do. Frame
// order matters: the first image is the start and the second the end.
func (p *InteractionsMediaClient) omniInput(in media.VideoInput) ([]any, error) {
	var input []any
	add := func(block content.Block, encode func(content.Block) (any, error)) error {
		part, err := encode(block)
		if err == nil {
			input = append(input, part)
		}
		return err
	}
	if in.SourceVideo != nil {
		if err := add(*in.SourceVideo, p.videoPart); err != nil {
			return nil, err
		}
	}
	for _, frame := range []*content.Block{in.FirstFrame, in.LastFrame} {
		if frame != nil {
			if err := add(*frame, p.inputPart); err != nil {
				return nil, err
			}
		}
	}
	for _, block := range in.ReferenceImages {
		if err := add(block, p.inputPart); err != nil {
			return nil, err
		}
	}
	for _, block := range in.ReferenceVideos {
		if err := add(block, p.videoPart); err != nil {
			return nil, err
		}
	}
	for _, block := range in.Prompt {
		if err := add(block, p.inputPart); err != nil {
			return nil, err
		}
	}
	return input, nil
}

// SendVideo dispatches one synchronous Interactions video call.
func (p *InteractionsMediaClient) SendVideo(
	ctx context.Context,
	request media.VideoRequest,
) (media.Result, error) {
	in := request.Input()
	if request.Origin() != p.origin(in.Model) ||
		request.ContentType() != "application/json" {
		return media.Result{}, p.failure(
			media.ErrorInvalidRequest,
			"prepared video request belongs to another destination",
		)
	}
	if delivery, err := preparedVideoDelivery(request.Body()); err != nil ||
		delivery != "inline" {
		return media.Result{}, p.failure(
			media.ErrorInvalidRequest,
			"prepared video request is not an inline Interactions call",
		)
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
