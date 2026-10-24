package imagine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

type imageWireResponse struct {
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
	Data  []struct {
		URL        string          `json:"url"`
		Base64     string          `json:"b64_json"`
		MIMEType   string          `json:"mime_type"`
		FileOutput *fileOutputWire `json:"file_output"`
	} `json:"data"`
}

// Inline data is embedded into one JSON body and retained in a prepared
// request. These are local memory bounds, not xAI service limits.
const (
	maxInlineImageBytes = 16 << 20
	maxPreparedBytes    = 24 << 20
)

func imageWireBody(input media.ImageInput) ([]byte, string, error) {
	return imageWireBodyWithOptions(input, ImageOptions{}, content.Origin{})
}

func imageWireBodyWithOptions(
	input media.ImageInput,
	options ImageOptions,
	fileOrigin content.Origin,
) ([]byte, string, error) {
	if err := input.Validate(); err != nil {
		return nil, "", err
	}
	format := options.ResponseFormat
	if format == "" {
		format = ImageResponseURL
	}
	if format != ImageResponseURL && format != ImageResponseBase64 {
		return nil, "", errors.New("imagine: unsupported image response format")
	}
	prompt, err := textPrompt(input.Prompt)
	if err != nil {
		return nil, "", err
	}
	if len(prompt) > maxPreparedBytes {
		return nil, "", errors.New("imagine: image prompt exceeds 24 MiB prepared-body bound")
	}
	if input.Mask != nil {
		return nil, "", errors.New("imagine: image masks are not supported by xAI's edit endpoint")
	}
	if input.Settings.Size != (media.Size{}) {
		return nil, "", errors.New("imagine: exact image size is not supported; use aspect ratio and resolution")
	}
	if input.Settings.OutputMIMEType != "" {
		return nil, "", errors.New("imagine: xAI does not accept an output MIME type")
	}
	if input.Settings.Count > 10 {
		return nil, "", errors.New("imagine: xAI accepts at most ten generated images")
	}
	if input.Operation == media.ImageEdit && input.Settings.Count > 0 {
		return nil, "", errors.New("imagine: image edit count is not documented by xAI")
	}
	if ratio := input.Settings.AspectRatio; ratio != "" &&
		!imageAspectRatio(ratio) {
		return nil, "", fmt.Errorf(
			"imagine: unsupported image aspect ratio %q",
			ratio,
		)
	}
	if quality := input.Settings.Quality; quality != "" {
		if input.Model != "grok-imagine-image-2.0" ||
			(quality != "low" && quality != "medium" && quality != "auto") {
			return nil, "", fmt.Errorf(
				"imagine: unsupported image quality %q for %q",
				quality,
				input.Model,
			)
		}
	}
	if resolution := input.Settings.Resolution; resolution != "" {
		if input.Model != "grok-imagine-image-2.0" ||
			(resolution != "1k" && resolution != "2k") {
			return nil, "", fmt.Errorf(
				"imagine: unsupported image resolution %q for %q",
				resolution,
				input.Model,
			)
		}
	}
	body := map[string]any{
		"model":           input.Model,
		"prompt":          prompt,
		"response_format": format,
	}
	if err := addStorageOptions(body, options.Storage); err != nil {
		return nil, "", err
	}
	if input.Settings.Count > 0 {
		body["n"] = input.Settings.Count
	}
	if input.Settings.AspectRatio != "" {
		body["aspect_ratio"] = input.Settings.AspectRatio
	}
	if input.Settings.Quality != "" {
		body["quality"] = input.Settings.Quality
	}
	if input.Settings.Resolution != "" {
		body["resolution"] = input.Settings.Resolution
	}
	path := "/images/generations"
	if input.Operation == media.ImageEdit {
		path = "/images/edits"
		if len(input.ReferenceImages) > 5 {
			return nil, "", errors.New("imagine: xAI accepts at most five edit images")
		}
		if len(input.ReferenceImages) > 1 &&
			input.Model != "grok-imagine-image-2.0" {
			return nil, "", errors.New("imagine: multiple edit images require grok-imagine-image-2.0")
		}
		inlineBytes := 0
		for _, block := range input.ReferenceImages {
			if len(block.Image.Source.Data) > maxInlineImageBytes-inlineBytes {
				return nil, "", errors.New("imagine: inline image data exceeds 16 MiB per request")
			}
			inlineBytes += len(block.Image.Source.Data)
		}
		images := make([]any, 0, len(input.ReferenceImages))
		for _, block := range input.ReferenceImages {
			image, err := imageInputSource(block.Image.Source, fileOrigin)
			if err != nil {
				return nil, "", err
			}
			images = append(images, image)
		}
		if len(images) == 1 {
			body["image"] = images[0]
		} else {
			body["images"] = images
		}
	} else if len(input.ReferenceImages) > 0 {
		return nil, "", errors.New("imagine: generation does not accept reference images; use edit")
	}
	raw, err := boundedJSON(body)
	return raw, path, err
}

func boundedJSON(body map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPreparedBytes {
		return nil, errors.New("imagine: prepared JSON body exceeds 24 MiB")
	}
	return raw, nil
}

func imageAspectRatio(ratio string) bool {
	switch ratio {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2",
		"19.5:9", "9:19.5", "20:9", "9:20", "21:9", "5:2", "auto":
		return true
	default:
		return false
	}
}

func imageInputSource(
	source content.Source,
	fileOrigin ...content.Origin,
) (map[string]string, error) {
	source = source.WithDetectedMIMEType("")
	if source.MIMEType != "" && !strings.HasPrefix(source.MIMEType, "image/") {
		return nil, errors.New("imagine: reference image requires image MIME type when specified")
	}
	if source.ID != "" {
		if len(fileOrigin) != 1 || source.Origin != fileOrigin[0] {
			return nil, errors.New("imagine: file ID belongs to another Files API destination")
		}
		return map[string]string{"file_id": source.ID}, nil
	}
	if source.URL != "" {
		if !validHTTPURL(source.URL) {
			return nil, errors.New("imagine: reference image URL must be HTTP(S) without user info")
		}
		return map[string]string{"type": "image_url", "url": source.URL}, nil
	}
	if source.Data != nil {
		return map[string]string{"type": "image_url", "url": source.DataURL()}, nil
	}
	if source.Ref != nil {
		return nil, fmt.Errorf(
			"imagine: content reference %q cannot be an edit image; pass the bytes",
			source.Ref.ID,
		)
	}
	return nil, errors.New("imagine: edit images require a public URL or inline bytes")
}

func decodeImageWire(
	raw []byte,
	provider, requested string,
) (media.Result, error) {
	return decodeImageWireWithFiles(raw, provider, requested, content.Origin{})
}

func decodeImageWireWithFiles(
	raw []byte,
	provider, requested string,
	files content.Origin,
) (media.Result, error) {
	var wire imageWireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return media.Result{}, fmt.Errorf("imagine: decode image response: %w", err)
	}
	if len(wire.Data) == 0 {
		return media.Result{}, errors.New("imagine: image response has no data")
	}
	result := media.Result{
		Kind:      media.KindImage,
		Requested: model.ModelRef{Provider: provider, Model: requested},
		API:       ImageAPI,
		RawUsage:  media.ReportedUsage(wire.Usage),
	}
	if wire.Model != "" {
		result.Served = model.ModelRef{Provider: provider, Model: wire.Model}
	}
	for _, item := range wire.Data {
		if (item.URL == "") == (item.Base64 == "") {
			return media.Result{}, errors.New("imagine: image data requires exactly one URL or base64 payload")
		}
		var source content.Source
		if item.URL != "" {
			if !validHTTPURL(item.URL) {
				return media.Result{}, errors.New("imagine: generated image URL must be HTTP(S) without user info")
			}
			source.URL = item.URL
		} else {
			decoded, err := base64.StdEncoding.DecodeString(item.Base64)
			if err != nil {
				return media.Result{}, fmt.Errorf("imagine: invalid base64 image: %w", err)
			}
			source.Data = decoded
		}
		source.MIMEType = item.MIMEType
		if source.MIMEType != "" && !strings.HasPrefix(source.MIMEType, "image/") {
			return media.Result{}, fmt.Errorf("imagine: image response has MIME type %q", source.MIMEType)
		}
		if source.Data != nil && source.MIMEType == "" {
			source.MIMEType = http.DetectContentType(source.Data)
			if !strings.HasPrefix(source.MIMEType, "image/") {
				return media.Result{}, errors.New("imagine: inline image response has unknown image MIME type")
			}
		}
		result.Content = append(result.Content, content.ImageBlock(source))
		if item.FileOutput != nil {
			stored, err := item.FileOutput.storedOutput(
				len(result.Content)-1,
				source.MIMEType,
				files,
			)
			if err != nil {
				return media.Result{}, err
			}
			result.StoredOutputs = append(result.StoredOutputs, stored)
		}
	}
	return result, result.Validate()
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil &&
		u.Host != "" &&
		(u.Scheme == "https" ||
			u.Scheme == "http") &&
		u.User == nil
}

func textPrompt(blocks []content.Block) (string, error) {
	if len(blocks) != 1 ||
		blocks[0].Kind != content.KindText ||
		blocks[0].Text == nil {
		return "", errors.New("imagine: requires exactly one text prompt block")
	}
	if strings.TrimSpace(blocks[0].Text.Text) == "" {
		return "", errors.New("imagine: prompt text cannot be blank")
	}
	return blocks[0].Text.Text, nil
}
