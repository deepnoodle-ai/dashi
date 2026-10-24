package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

func (p *MediaClient) imageEvidence(
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

// PrepareImage encodes a single native image call without network I/O.
func (p *MediaClient) PrepareImage(
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
		in.Settings.OutputMIMEType != "" || in.Settings.Count > 1 {
		return media.ImageRequest{}, p.failure(
			media.ErrorUnsupported,
			"Gemini image mask, exact size, quality, output MIME, or count above one",
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
	if in.Operation == media.ImageGenerate && len(in.ReferenceImages) > 0 {
		return media.ImageRequest{}, p.failure(
			media.ErrorInvalidRequest,
			"reference images require edit operation",
		)
	}
	info, unverified, err := p.imageEvidence(in)
	if err != nil {
		return media.ImageRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	parts := make([]any, 0, len(in.Prompt)+len(in.ReferenceImages))
	for _, b := range in.Prompt {
		part, err := p.imagePart(b)
		if err != nil {
			return media.ImageRequest{}, err
		}
		parts = append(parts, part)
	}
	for _, b := range in.ReferenceImages {
		part, err := p.imagePart(b)
		if err != nil {
			return media.ImageRequest{}, err
		}
		parts = append(parts, part)
	}
	config := map[string]any{"responseModalities": []string{"IMAGE"}}
	if in.Settings.AspectRatio != "" || in.Settings.Resolution != "" {
		imageConfig := map[string]any{}
		if in.Settings.AspectRatio != "" {
			imageConfig["aspectRatio"] = in.Settings.AspectRatio
		}
		if in.Settings.Resolution != "" {
			imageConfig["imageSize"] = in.Settings.Resolution
		}
		config["imageConfig"] = imageConfig
	}
	body, err := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": config,
	})
	if err != nil {
		return media.ImageRequest{}, err
	}
	return media.NewImageRequest(media.ImageRequestSnapshot{
		Origin:           p.imageOrigin(in.Model),
		Input:            in,
		Effective:        in.Settings,
		Body:             body,
		ContentType:      "application/json",
		ModelInfo:        info,
		CapabilityPolicy: p.config.CapabilityPolicy,
		Unverified:       unverified,
	})
}

func (p *MediaClient) imagePart(b content.Block) (any, error) {
	switch b.Kind {
	case content.KindText:
		if len(b.Text.Citations) > 0 {
			return nil, p.failure(media.ErrorUnsupported, "image prompt citations")
		}
		return map[string]any{"text": b.Text.Text}, nil
	case content.KindImage:
		if b.Image.Detail != "" {
			return nil, p.failure(media.ErrorUnsupported, "image detail")
		}
		return p.inlineImage(b.Image.Source)
	default:
		return nil, p.failure(
			media.ErrorUnsupported,
			"image prompt block %q",
			b.Kind,
		)
	}
}

func (p *MediaClient) inlineImage(source content.Source) (any, error) {
	source = source.WithDetectedMIMEType("")
	if source.Data == nil || !strings.HasPrefix(source.MIMEType, "image/") {
		return nil, p.failure(
			media.ErrorUnsupported,
			"Gemini media references require inline image bytes",
		)
	}
	return map[string]any{
		"inlineData": map[string]any{
			"mimeType": source.MIMEType,
			"data":     base64.StdEncoding.EncodeToString(source.Data),
		},
	}, nil
}

// SendImage dispatches the prepared image call exactly once.
func (p *MediaClient) SendImage(
	ctx context.Context,
	prepared media.ImageRequest,
) (media.Result, error) {
	in := prepared.Input()
	if prepared.Origin() != p.imageOrigin(in.Model) ||
		prepared.ContentType() != "application/json" {
		return media.Result{}, p.failure(
			media.ErrorInvalidRequest,
			"prepared image request belongs to another destination",
		)
	}
	raw, err := p.do(
		ctx,
		http.MethodPost,
		p.imageURL(in.Model),
		prepared.Body(),
		prepared.ContentType(),
		maxImageResponseBytes,
		false,
	)
	if err != nil {
		return media.Result{}, err
	}
	var response struct {
		ResponseID    string          `json:"responseId"`
		ModelVersion  string          `json:"modelVersion"`
		UsageMetadata json.RawMessage `json:"usageMetadata"`
		Candidates    []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text       string `json:"text"`
					Thought    bool   `json:"thought"`
					InlineData *struct {
						MIMEType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return media.Result{}, p.failure(
			media.ErrorProtocol,
			"decode image response: %w",
			err,
		)
	}
	result := media.Result{
		Kind:       media.KindImage,
		Requested:  model.ModelRef{Provider: p.Provider(), Model: in.Model},
		API:        ImageAPI,
		ResponseID: response.ResponseID,
		RawUsage:   media.ReportedUsage(response.UsageMetadata),
	}
	if response.ModelVersion != "" {
		result.Served = model.ModelRef{
			Provider: p.Provider(),
			Model:    response.ModelVersion,
		}
	}
	for _, candidate := range response.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Thought {
				continue
			}
			if part.Text != "" {
				result.Content = append(result.Content, content.TextBlock(part.Text))
			}
			if part.InlineData != nil {
				if !strings.HasPrefix(part.InlineData.MIMEType, "image/") {
					return result, p.failure(
						media.ErrorProtocol,
						"non-image data in image response",
					)
				}
				data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return result, p.failure(
						media.ErrorProtocol,
						"decode generated image: %w",
						err,
					)
				}
				result.Content = append(
					result.Content,
					content.ImageBlock(content.Source{
						Data:     data,
						MIMEType: part.InlineData.MIMEType,
					}),
				)
			}
		}
	}
	if err := result.Validate(); err != nil {
		return result, p.failure(media.ErrorProtocol, "%w", err)
	}
	return result, nil
}
