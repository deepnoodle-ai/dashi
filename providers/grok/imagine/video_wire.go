package imagine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
)

type videoWireResponse struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Video  struct {
		URL               string          `json:"url"`
		Duration          *float64        `json:"duration"`
		RespectModeration *bool           `json:"respect_moderation"`
		FileOutput        *fileOutputWire `json:"file_output"`
	} `json:"video"`
	Error json.RawMessage `json:"error"`
	Usage json.RawMessage `json:"usage"`
}

func videoWireBody(input media.VideoInput) ([]byte, error) {
	return videoWireBodyWithOptions(input, VideoOptions{})
}

func videoWireBodyWithOptions(input media.VideoInput, options VideoOptions, fileOrigin ...content.Origin) ([]byte, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	var files content.Origin
	if len(fileOrigin) == 1 {
		files = fileOrigin[0]
	}
	if input.Operation == media.VideoEdit || input.Operation == media.VideoExtend {
		return videoTransformWireBody(input, options, files)
	}
	if options.SourceVideo != nil {
		return nil, errors.New("imagine: source video requires edit or extend operation")
	}
	if input.Settings.Size != (media.Size{}) {
		return nil, errors.New("imagine: exact video size is not supported; use aspect ratio and resolution")
	}
	if ratio := input.Settings.AspectRatio; ratio != "" && !videoAspectRatio(ratio) {
		return nil, fmt.Errorf("imagine: unsupported video aspect ratio %q", ratio)
	}
	if resolution := input.Settings.Resolution; resolution != "" {
		if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
			return nil, fmt.Errorf("imagine: unsupported video resolution %q", resolution)
		}
		if resolution == "1080p" && !videoModel15(input.Model) {
			return nil, errors.New("imagine: 1080p requires grok-imagine-video-1.5")
		}
	}
	if input.Settings.Duration > 0 {
		if input.Settings.Duration < time.Second || input.Settings.Duration > 15*time.Second ||
			input.Settings.Duration%time.Second != 0 {
			return nil, errors.New("imagine: video duration must be whole seconds from 1 to 15")
		}
	}
	prompt := ""
	if len(input.Prompt) > 0 {
		var err error
		prompt, err = textPrompt(input.Prompt)
		if err != nil {
			return nil, err
		}
	}
	if len(prompt) > maxPreparedBytes {
		return nil, errors.New("imagine: video prompt exceeds 24 MiB prepared-body bound")
	}
	if prompt == "" && len(input.ReferenceFrames) == 0 && len(options.ReferenceImages) == 0 &&
		options.LastFrame == nil && len(options.Keyframes) == 0 && len(options.ReferenceVoices) == 0 {
		return nil, errors.New("imagine: video requires text, a frame, or a reference")
	}
	if len(input.ReferenceFrames) > 1 {
		return nil, errors.New("imagine: only one initial video frame is supported")
	}
	body := map[string]any{"model": input.Model}
	if prompt != "" {
		body["prompt"] = prompt
	}
	if len(input.ReferenceFrames) == 1 {
		frame, err := videoImageSource(input.ReferenceFrames[0], files)
		if err != nil {
			return nil, err
		}
		body["image"] = frame
	}
	if err := addVideoOptions(body, input, options, files); err != nil {
		return nil, err
	}
	if err := addStorageOptions(body, options.Storage); err != nil {
		return nil, err
	}
	if input.Settings.Duration > 0 {
		body["duration"] = int(input.Settings.Duration / time.Second)
	}
	if input.Settings.AspectRatio != "" {
		body["aspect_ratio"] = input.Settings.AspectRatio
	}
	if input.Settings.Resolution != "" {
		body["resolution"] = input.Settings.Resolution
	}
	return boundedJSON(body)
}

func videoModel15(name string) bool {
	switch name {
	case "grok-imagine-video-1.5", "grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30":
		return true
	default:
		return false
	}
}

func videoAspectRatio(ratio string) bool {
	switch ratio {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}

func decodeVideoWire(raw []byte) (videoWireResponse, error) {
	var wire videoWireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return videoWireResponse{}, fmt.Errorf("imagine: decode video status: %w", err)
	}
	switch wire.Status {
	case "pending", "done", "failed", "expired":
	default:
		return videoWireResponse{}, fmt.Errorf("imagine: unknown video status %q", wire.Status)
	}
	if wire.Status == "done" {
		if wire.Video.RespectModeration != nil && !*wire.Video.RespectModeration {
			return wire, nil
		}
		if !validHTTPURL(wire.Video.URL) {
			return videoWireResponse{}, errors.New("imagine: completed video URL must be HTTP(S) without user info")
		}
		if seconds := wire.Video.Duration; seconds != nil {
			// Stay well inside time.Duration's nanosecond range before conversion.
			if math.IsNaN(*seconds) || math.IsInf(*seconds, 0) || *seconds < 0 ||
				*seconds > float64(1<<62)/float64(time.Second) {
				return videoWireResponse{}, errors.New("imagine: completed video duration is invalid")
			}
		}
		block := content.VideoBlock(content.Source{URL: wire.Video.URL, MIMEType: "video/mp4"})
		if err := block.Validate(); err != nil {
			return videoWireResponse{}, fmt.Errorf("imagine: completed video has no output URL: %w", err)
		}
	}
	return wire, nil
}
