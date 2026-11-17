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
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
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
		first := input.ReferenceImages[0].Image.Source.WithDetectedMIMEType("")
		mask := input.Mask.Image.Source.WithDetectedMIMEType("")
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
	if message, ok := retiredImageModels[model]; ok {
		return errors.New(message)
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
		if format == "gif" {
			if known, ok := gptImageModels[input.Model]; ok && !known.gif {
				return nil, errors.New("images: GIF output requires GPT Image 2.6")
			}
			if input.Mask != nil {
				return nil, errors.New("images: GIF output does not accept a mask")
			}
		}
		fields["output_format"] = format
	}
	return fields, nil
}

func validSize(model string, size media.Size) bool {
	width, height := size.Width, size.Height
	known := gptImageModels[model]
	if known.flexibleSize {
		return width%16 == 0 && height%16 == 0 &&
			width <= 3840 && height <= 3840 &&
			width*height >= 655_360 && width*height <= 8_294_400 &&
			width <= 3*height && height <= 3*width
	}
	if known.fixedSizes {
		return width == 1024 && (height == 1024 || height == 1536) ||
			width == 1536 && height == 1024
	}
	return true
}

func validQuality(model, quality string) bool {
	switch quality {
	case "auto", "low", "medium", "high":
		return true
	case "standard", "hd":
		return !knownGPTImageModel(model)
	case "xhigh", "max":
		known, ok := gptImageModels[model]
		return !ok || known.topQuality
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
	case "image/gif":
		return "gif", nil
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
	var keys, values []string
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
		"complexity",
	} {
		if value, ok := fields[key]; ok {
			keys = append(keys, key)
			values = append(values, fmt.Sprint(value))
		}
	}
	// A boundary derived from the form's values keeps the body the same for
	// the same input.
	var parts [][]byte
	for _, value := range values {
		parts = append(parts, []byte(value))
	}
	for _, ref := range refs {
		parts = append(parts, ref.Image.Source.Data)
	}
	if mask != nil {
		parts = append(parts, mask.Image.Source.Data)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(providerhttp.MultipartBoundary(parts...)); err != nil {
		return nil, "", err
	}
	for i, key := range keys {
		if err := writer.WriteField(key, values[i]); err != nil {
			return nil, "", err
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
	source = source.WithDetectedMIMEType("")
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
	source := block.Image.Source.WithDetectedMIMEType("")
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
	case source.Ref != nil:
		return nil, fmt.Errorf(
			"images: content reference %q cannot be an image reference; pass the bytes",
			source.Ref.ID,
		)
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

// gptImageModel records the documented limits of one known GPT Image model
// (https://developers.openai.com/api/docs/guides/image-generation, read
// 2026-11-17). Models not listed here are sent as asked, and the capability
// policy decides about unknown support.
type gptImageModel struct {
	// fixedSizes limits sizes to 1024x1024, 1024x1536, and 1536x1024.
	fixedSizes bool
	// flexibleSize allows any size within the GPT Image 2 bounds.
	flexibleSize bool
	// topQuality adds the xhigh and max quality levels.
	topQuality bool
	// fidelity is "" when input_fidelity accepts high and low, "fixed" when
	// the API does not accept the parameter, and "low" when only low is
	// accepted.
	fidelity string
	// complexity accepts the complexity option.
	complexity bool
	// gif accepts GIF output.
	gif bool
}

var (
	image1Fixed = gptImageModel{fixedSizes: true}
	image2      = gptImageModel{flexibleSize: true, fidelity: "fixed"}
	image25     = gptImageModel{flexibleSize: true, topQuality: true}
	image26     = gptImageModel{
		flexibleSize: true,
		topQuality:   true,
		complexity:   true,
		gif:          true,
	}
)

// gptImageModels lists the GPT Image models OpenAI serves. gpt-image-1-mini,
// gpt-image-1.5, and chatgpt-image-latest shut down on 2026-12-01 and
// gpt-image-2 on 2027-02-10 (https://developers.openai.com/api/docs/deprecations);
// they work until then, so Dashi still sends them.
var gptImageModels = map[string]gptImageModel{
	"gpt-image-1-mini":                  {fixedSizes: true, fidelity: "low"},
	"gpt-image-1.5":                     image1Fixed,
	"gpt-image-1.5-2025-12-16":          image1Fixed,
	"chatgpt-image-latest":              {},
	"gpt-image-2":                       image2,
	"gpt-image-2-2026-04-21":            image2,
	"gpt-image-2.5-sunburst":            image25,
	"gpt-image-2.5-sunburst-2026-09-08": image25,
	"gpt-image-2.5-flare":               image25,
	"gpt-image-2.5-flare-2026-09-08":    image25,
	"gpt-image-2.6-sunburst":            image26,
	"gpt-image-2.6-flare":               image26,
}

// retiredImageModels maps each image model OpenAI has shut down to the
// error Dashi returns instead of sending it.
var retiredImageModels = map[string]string{
	"dall-e-2":    "images: DALL-E 2 and DALL-E 3 were retired from the API on 2026-05-12",
	"dall-e-3":    "images: DALL-E 2 and DALL-E 3 were retired from the API on 2026-05-12",
	"gpt-image-1": "images: gpt-image-1 was retired from the API on 2026-10-23; use gpt-image-2.6-sunburst or gpt-image-2.6-flare",
}

func knownGPTImageModel(name string) bool {
	_, ok := gptImageModels[name]
	return ok
}
