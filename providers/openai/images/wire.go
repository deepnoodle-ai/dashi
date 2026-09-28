package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

const maxReferenceCount = 16

func encodeImage(input media.ImageInput, prompt string,
	options Options) ([]byte, string, error) {
	if input.Operation == media.ImageGenerate &&
		(len(input.ReferenceImages) > 0 || input.Mask != nil) {
		return nil, "", errors.New("images: generation does not accept reference images or a mask")
	}
	for _, block := range input.ReferenceImages {
		if block.Image.Detail != "" {
			return nil, "", errors.New("images: edit references do not accept image detail")
		}
	}
	if input.Mask != nil && input.Mask.Image.Detail != "" {
		return nil, "", errors.New("images: edit mask does not accept image detail")
	}
	if input.Mask != nil {
		first := input.ReferenceImages[0].Image.Source
		mask := input.Mask.Image.Source
		if first.MIMEType != "" && mask.MIMEType != "" &&
			first.MIMEType != mask.MIMEType {
			return nil, "", errors.New("images: mask format must match the first image")
		}
	}
	if err := validateModelOperation(input.Model, input.Operation); err != nil {
		return nil, "", err
	}
	if err := validatePromptLength(input.Model, prompt); err != nil {
		return nil, "", err
	}
	fields, err := imageFields(input, prompt)
	if err != nil {
		return nil, "", err
	}
	optionFields, err := options.fields(input)
	if err != nil {
		return nil, "", err
	}
	maps.Copy(fields, optionFields)
	if input.Operation == media.ImageGenerate {
		body, err := json.Marshal(fields)
		return body, "application/json", err
	}
	if knownGPTImageModel(input.Model) &&
		len(input.ReferenceImages) > maxReferenceCount {
		return nil, "", errors.New("images: at most 16 reference images are supported")
	}
	if allInline(input.ReferenceImages, input.Mask) {
		return encodeMultipart(fields, input.ReferenceImages, input.Mask)
	}
	refs := make([]map[string]string, 0, len(input.ReferenceImages))
	for _, block := range input.ReferenceImages {
		ref, err := jsonReference(block)
		if err != nil {
			return nil, "", err
		}
		refs = append(refs, ref)
	}
	fields["images"] = refs
	if input.Mask != nil {
		ref, err := jsonReference(*input.Mask)
		if err != nil {
			return nil, "", err
		}
		fields["mask"] = ref
	}
	body, err := json.Marshal(fields)
	return body, "application/json", err
}

func encodePrompt(blocks []content.Block) (string, error) {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind != content.KindText || block.Text == nil ||
			len(block.Text.Citations) > 0 {
			return "", errors.New("images: prompt requires plain text blocks")
		}
		parts = append(parts, block.Text.Text)
	}
	prompt := strings.Join(parts, "\n")
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("images: prompt requires non-empty text")
	}
	return prompt, nil
}

func validateModelOperation(model string, _ media.ImageOperation) error {
	if model == "dall-e-2" || model == "dall-e-3" {
		return errors.New("images: DALL-E 2 and DALL-E 3 were retired from the API on 2026-05-12")
	}
	return nil
}

func validatePromptLength(model, prompt string) error {
	limit := 0
	if knownGPTImageModel(model) {
		limit = 32000
	}
	if limit > 0 && utf8.RuneCountInString(prompt) > limit {
		return fmt.Errorf(
			"images: prompt exceeds %d characters for %s",
			limit,
			model,
		)
	}
	return nil
}

func imageFields(
	input media.ImageInput,
	prompt string,
) (map[string]any, error) {
	s := input.Settings
	fields := map[string]any{"model": input.Model, "prompt": prompt}
	if s.AspectRatio != "" {
		return nil, errors.New("images: aspect ratio cannot be encoded by the Images API")
	}
	if s.Resolution != "" {
		if s.Resolution != "auto" {
			return nil, errors.New("images: unsupported image resolution")
		}
		fields["size"] = s.Resolution
	}
	if s.Size.Width > 0 {
		if !validSize(input.Model, s.Size) {
			return nil, errors.New("images: unsupported image size for model")
		}
		fields["size"] = strconv.Itoa(s.Size.Width) + "x" + strconv.Itoa(s.Size.Height)
	}
	if s.Count > 0 {
		if s.Count > 10 {
			return nil, errors.New("images: image count is unsupported by this model")
		}
		fields["n"] = s.Count
	}
	if s.Quality != "" {
		if !validQuality(input.Model, s.Quality) {
			return nil, errors.New("images: image quality is unsupported by this model")
		}
		fields["quality"] = s.Quality
	}
	if s.OutputMIMEType != "" {
		format, err := outputFormat(s.OutputMIMEType)
		if err != nil {
			return nil, err
		}
		fields["output_format"] = format
	}
	return fields, nil
}

func validSize(model string, size media.Size) bool {
	width, height := size.Width, size.Height
	if knownFlexibleSizeModel(model) {
		return width%16 == 0 && height%16 == 0 &&
			width <= 3840 && height <= 3840 &&
			width*height >= 655_360 && width*height <= 8_294_400 &&
			width <= 3*height && height <= 3*width
	}
	switch model {
	default:
		if model == "gpt-image-1" || model == "gpt-image-1-mini" ||
			model == "gpt-image-1.5" {
			return width == 1024 && (height == 1024 || height == 1536) ||
				width == 1536 && height == 1024
		}
		return true
	}
}

func validQuality(model, quality string) bool {
	switch quality {
	case "auto", "low", "medium", "high":
		return true
	case "standard", "hd":
		return !knownGPTImageModel(model)
	case "xhigh", "max":
		if strings.HasPrefix(model, "gpt-image-2.5-") {
			return true
		}
		return !knownGPTImageModel(model)
	}
	return false
}

func outputFormat(mimeType string) (string, error) {
	switch mimeType {
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpeg", nil
	case "image/webp":
		return "webp", nil
	}
	return "", fmt.Errorf("images: unsupported output MIME type %q", mimeType)
}

func allInline(refs []content.Block, mask *content.Block) bool {
	for _, ref := range refs {
		if ref.Image.Source.Data == nil {
			return false
		}
	}
	return mask == nil || mask.Image.Source.Data != nil
}

func encodeMultipart(fields map[string]any, refs []content.Block,
	mask *content.Block) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, key := range []string{
		"model",
		"prompt",
		"size",
		"n",
		"quality",
		"output_format",
		"background",
		"moderation",
		"input_fidelity",
		"output_compression",
	} {
		if value, ok := fields[key]; ok {
			if err := writer.WriteField(key, fmt.Sprint(value)); err != nil {
				return nil, "", err
			}
		}
	}
	imageField := "image"
	if len(refs) > 1 {
		imageField = "image[]"
	}
	for i, ref := range refs {
		if err := writeImagePart(
			writer,
			imageField,
			i,
			ref.Image.Source,
		); err != nil {
			return nil, "", err
		}
	}
	if mask != nil {
		if err := writeImagePart(writer, "mask", 0, mask.Image.Source); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func writeImagePart(writer *multipart.Writer, name string, index int,
	source content.Source) error {
	if !supportedInputMIME(source.MIMEType) {
		return fmt.Errorf("images: unsupported input MIME type %q", source.MIMEType)
	}
	if len(source.Data) >= 50_000_000 {
		return errors.New("images: uploaded image must be smaller than 50 MB")
	}
	extension := inputExtension(source.MIMEType)
	header := make(textproto.MIMEHeader)
	header.Set(
		"Content-Disposition",
		fmt.Sprintf(
			`form-data; name=%q; filename=%q`,
			name,
			fmt.Sprintf("image-%d%s", index, extension),
		),
	)
	header.Set("Content-Type", source.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(source.Data)
	return err
}

func jsonReference(block content.Block) (map[string]string, error) {
	source := block.Image.Source
	switch {
	case source.Data != nil:
		if !supportedInputMIME(source.MIMEType) {
			return nil, fmt.Errorf(
				"images: unsupported input MIME type %q",
				source.MIMEType,
			)
		}
		if base64.StdEncoding.EncodedLen(len(source.Data))+
			len("data:"+source.MIMEType+";base64,") > 20_971_520 {
			return nil, errors.New("images: image data URL exceeds API limit")
		}
		return map[string]string{"image_url": source.DataURL()}, nil
	case source.URL != "":
		if len(source.URL) > 20_971_520 {
			return nil, errors.New("images: image reference URL exceeds API limit")
		}
		parsed, err := url.Parse(source.URL)
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") ||
			parsed.User != nil {
			return nil, errors.New("images: image reference requires an HTTP URL")
		}
		if source.ExpiresAt != nil && !source.ExpiresAt.After(time.Now()) {
			return nil, errors.New("images: image reference has expired")
		}
		return map[string]string{"image_url": source.URL}, nil
	case source.ID != "":
		return nil, errors.New("images: provider file ID requires a qualified Files API scope")
	}
	return nil, errors.New("images: provider URI cannot be used as an image reference")
}

func inputExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".png"
}

func supportedInputMIME(mimeType string) bool {
	return mimeType == "image/png" || mimeType == "image/jpeg" ||
		mimeType == "image/webp"
}

func knownGPTImageModel(name string) bool {
	switch name {
	case "gpt-image-1", "gpt-image-1-mini", "gpt-image-1.5",
		"chatgpt-image-latest",
		"gpt-image-2", "gpt-image-2-2026-04-21",
		"gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst-2026-09-08",
		"gpt-image-2.5-flare", "gpt-image-2.5-flare-2026-09-08":
		return true
	}
	return false
}

func knownFlexibleSizeModel(name string) bool {
	switch name {
	case "gpt-image-2", "gpt-image-2-2026-04-21",
		"gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst-2026-09-08",
		"gpt-image-2.5-flare", "gpt-image-2.5-flare-2026-09-08":
		return true
	}
	return false
}

func knownImage2Model(name string) bool {
	return name == "gpt-image-2" || name == "gpt-image-2-2026-04-21"
}
