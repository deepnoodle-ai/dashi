package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

type imageResponse struct {
	Created      int64           `json:"created"`
	OutputFormat string          `json:"output_format"`
	Usage        json.RawMessage `json:"usage"`
	Data         []struct {
		Base64 string `json:"b64_json"`
		URL    string `json:"url"`
	} `json:"data"`
}

func decodeImageResponse(raw []byte, input media.ImageInput,
	origin content.Origin, responseID string) (media.Result, error) {
	var response imageResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return media.Result{}, fmt.Errorf("images: invalid response JSON: %w", err)
	}
	if len(response.Data) == 0 {
		return media.Result{}, errors.New("images: response contains no images")
	}
	blocks := make([]content.Block, 0, len(response.Data))
	for _, item := range response.Data {
		source, err := decodeImageSource(item.Base64, item.URL,
			response.OutputFormat, response.Created, input.Model)
		if err != nil {
			return media.Result{}, err
		}
		blocks = append(blocks, content.ImageBlock(source))
	}
	result := media.Result{
		Kind:    media.KindImage,
		Content: blocks,
		Requested: model.ModelRef{
			Provider: origin.Provider,
			Model:    input.Model,
		},
		API:        API,
		ResponseID: responseID,
		RawUsage:   slices.Clone(response.Usage),
	}
	if err := result.Validate(); err != nil {
		return media.Result{}, err
	}
	return result, nil
}

func decodeImageSource(encoded, imageURL, outputFormat string,
	created int64, modelName string) (content.Source, error) {
	if (encoded == "") == (imageURL == "") {
		return content.Source{}, errors.New(
			"images: response image requires exactly one of base64 or URL",
		)
	}
	mimeType, err := formatMIME(outputFormat)
	if err != nil {
		return content.Source{}, err
	}
	if encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			return content.Source{}, errors.New("images: invalid base64 image data")
		}
		detected := http.DetectContentType(data)
		if !supportedInputMIME(detected) {
			return content.Source{}, fmt.Errorf(
				"images: unsupported response image MIME type %q", detected,
			)
		}
		if mimeType == "" {
			mimeType = detected
		}
		if mimeType != detected {
			return content.Source{}, errors.New(
				"images: declared image format differs from returned bytes",
			)
		}
		return content.Source{Data: data, MIMEType: mimeType}, nil
	}
	if strings.HasPrefix(modelName, "gpt-image-") ||
		modelName == "chatgpt-image-latest" {
		return content.Source{}, errors.New("images: GPT image response requires base64 image data")
	}
	parsed, err := url.Parse(imageURL)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil {
		return content.Source{}, errors.New("images: response image requires an HTTP URL")
	}
	source := content.Source{URL: imageURL, MIMEType: mimeType}
	if created > 0 && strings.HasPrefix(modelName, "dall-e-") {
		expiry := time.Unix(created, 0).Add(time.Hour)
		source.ExpiresAt = &expiry
	}
	return source, nil
}

func formatMIME(format string) (string, error) {
	switch format {
	case "":
		return "", nil
	case "png":
		return "image/png", nil
	case "jpeg":
		return "image/jpeg", nil
	case "webp":
		return "image/webp", nil
	}
	return "", fmt.Errorf("images: unsupported response format %q", format)
}
