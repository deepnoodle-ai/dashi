package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/media"
)

func (p *InteractionsMediaClient) imageEvidence(
	in media.ImageInput,
) (*media.ModelInfo, []string, error) {
	info, known := p.ImageModelInfo(in.Model)
	if !known {
		unverified, err := (media.ImageCapabilities{}).Check(
			in,
			p.config.CapabilityPolicy,
		)
		return nil, unverified, err
	}
	unverified, err := info.Image.Check(in, p.config.CapabilityPolicy)
	return &info, unverified, err
}

// PrepareImage encodes a stateless Interactions call with inline input images.
// The exact JSON is retained for dispatch and can be recorded before sending.
func (p *InteractionsMediaClient) PrepareImage(
	ctx context.Context,
	in media.ImageInput,
) (media.ImageRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.ImageRequest{}, err
	}
	if err := in.Validate(); err != nil {
		return media.ImageRequest{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if strings.HasPrefix(in.Model, "imagen-") {
		return media.ImageRequest{}, p.failure(
			media.ErrorUnsupported,
			"Imagen models are retired from the Gemini API",
		)
	}
	if in.Mask != nil ||
		in.Settings.Size.Width != 0 ||
		in.Settings.Quality != "" ||
		in.Settings.Count > 1 {
		return media.ImageRequest{}, p.failure(
			media.ErrorUnsupported,
			"Interactions image mask, exact size, quality, or count above one",
		)
	}
	if in.Operation == media.ImageGenerate && len(in.ReferenceImages) > 0 {
		return media.ImageRequest{}, p.failure(
			media.ErrorInvalidRequest,
			"reference images require edit operation",
		)
	}
	if (in.Model == "gemini-3.1-flash-lite-image" ||
		in.Model == "gemini-3.1-flash-image" ||
		in.Model == "gemini-3-pro-image") && len(in.ReferenceImages) > 14 {
		return media.ImageRequest{}, p.failure(
			media.ErrorUnsupported,
			"Interactions image edit supports at most 14 reference images",
		)
	}
	if err := validateImageModelResolution(
		in.Model,
		in.Settings.Resolution,
	); err != nil {
		return media.ImageRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	if err := validateImageModelAspectRatio(
		in.Model,
		in.Settings.AspectRatio,
	); err != nil {
		return media.ImageRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	if mime := in.Settings.OutputMIMEType; mime != "" &&
		mime != "image/jpeg" &&
		mime != "image/png" {
		return media.ImageRequest{}, p.failure(
			media.ErrorUnsupported,
			"Interactions image output MIME type %q",
			mime,
		)
	}
	info, unverified, err := p.imageEvidence(in)
	if err != nil {
		return media.ImageRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	input := make([]any, 0, len(in.Prompt)+len(in.ReferenceImages))
	for _, block := range in.Prompt {
		part, err := p.inputPart(block)
		if err != nil {
			return media.ImageRequest{}, err
		}
		input = append(input, part)
	}
	for _, block := range in.ReferenceImages {
		part, err := p.inputPart(block)
		if err != nil {
			return media.ImageRequest{}, err
		}
		input = append(input, part)
	}
	// Image responses are inline by default. The Interactions API rejects an
	// explicit image delivery mode, even when its value is "inline".
	format := map[string]any{"type": "image"}
	if in.Settings.OutputMIMEType != "" {
		format["mime_type"] = in.Settings.OutputMIMEType
	}
	if in.Settings.AspectRatio != "" {
		format["aspect_ratio"] = in.Settings.AspectRatio
	}
	if in.Settings.Resolution != "" {
		format["image_size"] = in.Settings.Resolution
	}
	body, err := json.Marshal(map[string]any{
		"model":           in.Model,
		"input":           input,
		"response_format": format,
		"background":      false,
		"store":           false,
		"stream":          false,
	})
	if err != nil {
		return media.ImageRequest{}, err
	}
	return media.NewImageRequest(media.ImageRequestSnapshot{
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

// SendImage dispatches one prepared Interactions image call.
func (p *InteractionsMediaClient) SendImage(
	ctx context.Context,
	request media.ImageRequest,
) (media.Result, error) {
	in := request.Input()
	if request.Origin() != p.origin(in.Model) ||
		request.ContentType() != "application/json" {
		return media.Result{}, p.failure(
			media.ErrorInvalidRequest,
			"prepared image request belongs to another destination",
		)
	}
	raw, err := p.do(ctx, request.Body())
	if err != nil {
		return media.Result{}, err
	}
	return p.decode(raw, media.KindImage, in.Model)
}

// validateImageModelResolution applies documented limits of current image
// models. Unknown models follow capability evidence and the capability policy.
func validateImageModelResolution(name, resolution string) error {
	if resolution == "" {
		return nil
	}
	switch resolution {
	case "512", "1K", "2K", "4K":
	default:
		return fmt.Errorf("Gemini image resolution %q", resolution)
	}
	switch name {
	case "gemini-3.1-flash-lite-image", "gemini-2.5-flash-image":
		if resolution != "1K" {
			return fmt.Errorf("%s supports only 1K image resolution", name)
		}
	case "gemini-3-pro-image":
		if resolution == "512" {
			return fmt.Errorf("%s does not support 512 image resolution", name)
		}
	case "gemini-3.1-flash-image":
		// All four documented sizes are available.
	default:
		// Other model names rely on scoped evidence and the capability policy.
	}
	return nil
}

func validateImageModelAspectRatio(name, ratio string) error {
	if ratio == "" {
		return nil
	}
	standard := ratio == "1:1" || ratio == "2:3" || ratio == "3:2" ||
		ratio == "3:4" || ratio == "4:3" || ratio == "4:5" ||
		ratio == "5:4" || ratio == "9:16" || ratio == "16:9" || ratio == "21:9"
	extended := ratio == "1:4" ||
		ratio == "4:1" ||
		ratio == "1:8" ||
		ratio == "8:1"
	if !standard && !extended {
		return fmt.Errorf("Gemini image aspect ratio %q", ratio)
	}
	if extended &&
		(name == "gemini-3.1-flash-lite-image" ||
			name == "gemini-3-pro-image" ||
			name == "gemini-2.5-flash-image") {
		return fmt.Errorf("%s does not support %s image aspect ratio", name, ratio)
	}
	return nil
}
