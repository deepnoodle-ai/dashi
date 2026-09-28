package imagine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// Local preparation bound, not a provider limit. Base64 expands this inside
// the 24 MiB prepared JSON body bound.
const maxInlineVideoBytes = 16 << 20

// VideoOptions holds xAI generation controls that do not belong to Dashi's
// provider-independent VideoInput. ReferenceImages guide appearance; unlike
// VideoInput.ReferenceFrames, they do not pin the first frame.
type VideoOptions struct {
	SourceVideo     *content.Block  `json:"source_video,omitempty"`
	Storage         *StorageOptions `json:"storage,omitempty"`
	ReferenceImages []content.Block `json:"reference_images,omitempty"`
	LastFrame       *content.Block  `json:"last_frame,omitempty"`
	Keyframes       []VideoKeyframe `json:"keyframes,omitempty"`
	ReferenceVoices []string        `json:"reference_voices,omitempty"`
	GenerateAudio   *bool           `json:"generate_audio,omitempty"`
}

// VideoKeyframe pins an image at a moment strictly inside the clip. Timestamp
// must be on xAI's one-third-second grid, within one nanosecond of a grid
// point.
type VideoKeyframe struct {
	Image     content.Block `json:"image"`
	Timestamp time.Duration `json:"timestamp"`
}

func optionsHasValues(options VideoOptions) bool {
	return options.SourceVideo != nil || options.Storage != nil ||
		len(options.ReferenceImages) > 0 || options.LastFrame != nil ||
		len(options.Keyframes) > 0 || len(options.ReferenceVoices) > 0 ||
		options.GenerateAudio != nil
}

func videoTransformWireBody(
	input media.VideoInput,
	options VideoOptions,
	files content.Origin,
) ([]byte, error) {
	if input.Operation != media.VideoEdit &&
		input.Operation != media.VideoExtend {
		return nil, errors.New("imagine: unsupported video mode")
	}
	if options.SourceVideo == nil {
		return nil, errors.New("imagine: edit and extension require a source video")
	}
	if len(input.ReferenceFrames) > 0 || len(options.ReferenceImages) > 0 ||
		options.LastFrame != nil || len(options.Keyframes) > 0 ||
		len(options.ReferenceVoices) > 0 || options.GenerateAudio != nil {
		return nil, errors.New("imagine: video edit and extension cannot combine generation inputs")
	}
	if input.Settings.Size != (media.Size{}) || input.Settings.Resolution != "" ||
		input.Settings.AspectRatio != "" {
		return nil, errors.New("imagine: video edit and extension inherit source size and aspect ratio")
	}
	if input.Operation == media.VideoEdit && input.Settings.Duration != 0 {
		return nil, errors.New("imagine: video edit inherits source duration")
	}
	if input.Operation == media.VideoExtend && input.Settings.Duration != 0 &&
		(input.Settings.Duration < 2*time.Second ||
			input.Settings.Duration > 10*time.Second ||
			input.Settings.Duration%time.Second != 0) {
		return nil, errors.New("imagine: video extension duration must be whole seconds from 2 to 10")
	}
	prompt, err := textPrompt(input.Prompt)
	if err != nil {
		return nil, err
	}
	video, err := videoInputSource(*options.SourceVideo, files)
	if err != nil {
		return nil, err
	}
	if knownDuration := options.SourceVideo.Video.Duration; knownDuration > 0 {
		if input.Operation == media.VideoEdit &&
			knownDuration > 8700*time.Millisecond {
			return nil, errors.New("imagine: source video for editing exceeds 8.7 seconds")
		}
		if input.Operation == media.VideoExtend &&
			(knownDuration < 2*time.Second ||
				knownDuration > 15*time.Second) {
			return nil, errors.New("imagine: source video for extension must be 2 to 15 seconds")
		}
	}
	body := map[string]any{"model": input.Model, "prompt": prompt, "video": video}
	if err := addStorageOptions(body, options.Storage); err != nil {
		return nil, err
	}
	if input.Operation == media.VideoExtend && input.Settings.Duration > 0 {
		body["duration"] = int(input.Settings.Duration / time.Second)
	}
	return boundedJSON(body)
}

func videoInputSource(
	block content.Block,
	files content.Origin,
) (map[string]string, error) {
	if block.Kind != content.KindVideo ||
		block.Video == nil ||
		block.Validate() != nil {
		return nil, errors.New("imagine: source video requires a valid video block")
	}
	source := block.Video.Source
	if source.MIMEType != "" && source.MIMEType != "video/mp4" {
		return nil, errors.New("imagine: source video requires video/mp4 MIME type when specified")
	}
	if source.ID != "" {
		if source.Origin != files {
			return nil, errors.New("imagine: file ID belongs to another Files API destination")
		}
		return map[string]string{"file_id": source.ID}, nil
	}
	if source.URL != "" {
		if !validHTTPURL(source.URL) {
			return nil, errors.New("imagine: source video URL must be HTTP(S) without user info")
		}
		return map[string]string{"url": source.URL}, nil
	}
	if source.Data != nil {
		if source.MIMEType != "video/mp4" {
			return nil, errors.New("imagine: inline source video requires video/mp4 MIME type")
		}
		if len(source.Data) > maxInlineVideoBytes {
			return nil, errors.New("imagine: inline source video exceeds 16 MiB per request")
		}
		return map[string]string{"url": source.DataURL()}, nil
	}
	return nil, errors.New("imagine: source video requires a public URL or inline bytes")
}

func addVideoOptions(
	body map[string]any,
	input media.VideoInput,
	options VideoOptions,
	files content.Origin,
) error {
	hasRefs := len(options.ReferenceImages) > 0 || options.LastFrame != nil ||
		len(options.Keyframes) > 0 || len(options.ReferenceVoices) > 0
	if len(options.ReferenceImages) > 7 {
		return errors.New("imagine: video accepts at most seven reference images")
	}
	if len(options.ReferenceVoices) > 3 {
		return errors.New("imagine: video accepts at most three reference voices")
	}
	if len(options.Keyframes) > 4 {
		return errors.New("imagine: video accepts at most four keyframes")
	}
	if (options.LastFrame != nil || len(options.Keyframes) > 0 ||
		len(options.ReferenceVoices) > 0 ||
		options.GenerateAudio != nil) &&
		!videoModel15(input.Model) {
		return errors.New("imagine: last frame, keyframes, voices, and audio control require grok-imagine-video-1.5")
	}
	if input.Model == "grok-imagine-video" && len(input.ReferenceFrames) > 0 &&
		len(options.ReferenceImages) > 0 {
		return errors.New("imagine: grok-imagine-video cannot combine a first frame with references")
	}
	if hasRefs && input.Settings.Resolution == "1080p" {
		return errors.New("imagine: reference-to-video is limited to 720p")
	}
	inlineBytes := 0
	for _, block := range input.ReferenceFrames {
		inlineBytes += len(block.Image.Source.Data)
	}
	references := make([]map[string]string, 0, len(options.ReferenceImages))
	for _, block := range options.ReferenceImages {
		image, err := videoImageSource(block, files)
		if err != nil {
			return fmt.Errorf("imagine: reference image: %w", err)
		}
		inlineBytes += len(block.Image.Source.Data)
		if inlineBytes > maxInlineImageBytes {
			return errors.New("imagine: inline image data exceeds 16 MiB per request")
		}
		references = append(references, image)
	}
	if len(references) > 0 {
		body["reference_images"] = references
	}
	if options.LastFrame != nil {
		image, err := videoImageSource(*options.LastFrame, files)
		if err != nil {
			return fmt.Errorf("imagine: last frame: %w", err)
		}
		inlineBytes += len(options.LastFrame.Image.Source.Data)
		if inlineBytes > maxInlineImageBytes {
			return errors.New("imagine: inline image data exceeds 16 MiB per request")
		}
		body["last_frame"] = image
	}
	if len(options.Keyframes) > 0 {
		if input.Settings.Duration == 0 {
			return errors.New("imagine: keyframes require an explicit video duration")
		}
		keyframes := make([]map[string]any, 0, len(options.Keyframes))
		lastSlot := int64(0)
		for _, keyframe := range options.Keyframes {
			image, err := videoImageSource(keyframe.Image, files)
			if err != nil {
				return fmt.Errorf("imagine: keyframe: %w", err)
			}
			inlineBytes += len(keyframe.Image.Image.Source.Data)
			if inlineBytes > maxInlineImageBytes {
				return errors.New("imagine: inline image data exceeds 16 MiB per request")
			}
			if keyframe.Timestamp <= 0 ||
				keyframe.Timestamp >= input.Settings.Duration {
				return errors.New("imagine: keyframe timestamps must fall strictly inside the clip")
			}
			slot := int64(math.Round(float64(keyframe.Timestamp) * 3 / float64(time.Second)))
			gridTime := float64(slot) * float64(time.Second) / 3
			if math.Abs(float64(keyframe.Timestamp)-gridTime) > 1 || slot <= lastSlot {
				return errors.New("imagine: keyframe timestamps must increase on a one-third-second grid")
			}
			lastSlot = slot
			keyframes = append(
				keyframes,
				map[string]any{
					"image":       image,
					"timestamp_s": float64(slot) / 3,
				},
			)
		}
		body["keyframes"] = keyframes
	}
	if len(options.ReferenceVoices) > 0 {
		voices := make([]map[string]string, 0, len(options.ReferenceVoices))
		for _, voice := range options.ReferenceVoices {
			if strings.TrimSpace(voice) != voice || voice == "" {
				return errors.New("imagine: reference voice ID must be nonempty without surrounding spaces")
			}
			voices = append(voices, map[string]string{"voice_id": voice})
		}
		body["reference_audios"] = voices
	}
	if options.GenerateAudio != nil {
		body["generate_audio"] = *options.GenerateAudio
	}
	return nil
}

func videoImageSource(
	block content.Block,
	files content.Origin,
) (map[string]string, error) {
	if block.Kind != content.KindImage ||
		block.Image == nil ||
		block.Validate() != nil {
		return nil, errors.New("imagine: video frame requires a valid image block")
	}
	if len(block.Image.Source.Data) > maxInlineImageBytes {
		return nil, errors.New("imagine: inline image data exceeds 16 MiB per request")
	}
	image, err := imageInputSource(block.Image.Source, files)
	if err != nil {
		return nil, err
	}
	delete(image, "type")
	return image, nil
}
