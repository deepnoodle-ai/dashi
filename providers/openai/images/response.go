package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

type imageResponse struct {
	OutputFormat string          `json:"output_format"`
	Usage        json.RawMessage `json:"usage"`
	SVGUsage     json.RawMessage `json:"svg_usage"`
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
		source, err := decodeImageSource(
			item.Base64,
			item.URL,
			response.OutputFormat,
			input.Model,
		)
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
	}
	usage, err := rawUsage(response.Usage, response.SVGUsage)
	if err != nil {
		return media.Result{}, err
	}
	result.RawUsage = usage
	if err := result.Validate(); err != nil {
		return media.Result{}, err
	}
	return result, nil
}

// rawUsage keeps the response's usage object. SVG output also reports the
// SVG author's tokens in a separate svg_usage object, which is added to the
// raw usage under the same key so neither bill is lost.
func rawUsage(usage, svgUsage json.RawMessage) (json.RawMessage, error) {
	usage = media.ReportedUsage(usage)
	svgUsage = media.ReportedUsage(svgUsage)
	if svgUsage == nil {
		return usage, nil
	}
	fields := map[string]json.RawMessage{}
	if usage != nil {
		if err := json.Unmarshal(usage, &fields); err != nil {
			return nil, fmt.Errorf("images: invalid usage object: %w", err)
		}
	}
	fields["svg_usage"] = svgUsage
	return json.Marshal(fields)
}

func decodeImageSource(encoded, imageURL, outputFormat,
	modelName string) (content.Source, error) {
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
		if mimeType == svgMIMEType {
			if !looksLikeSVG(data) {
				return content.Source{}, errors.New("images: SVG response is not SVG text")
			}
			return content.Source{Data: data, MIMEType: mimeType}, nil
		}
		detected := http.DetectContentType(data)
		if !supportedInputMIME(detected) && detected != "image/gif" {
			return content.Source{}, fmt.Errorf(
				"images: unsupported response image MIME type %q",
				detected,
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
	if knownGPTImageModel(modelName) {
		return content.Source{}, errors.New("images: GPT image response requires base64 image data")
	}
	parsed, err := url.Parse(imageURL)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil {
		return content.Source{}, errors.New("images: response image requires an HTTP URL")
	}
	return content.Source{URL: imageURL, MIMEType: mimeType}, nil
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
	case "gif":
		return "image/gif", nil
	case "svg":
		return svgMIMEType, nil
	}
	return "", fmt.Errorf("images: unsupported response format %q", format)
}

// looksLikeSVG checks that SVG output is UTF-8 text with an svg element.
// Content sniffing calls SVG text/xml, so it cannot confirm the format.
func looksLikeSVG(data []byte) bool {
	return utf8.Valid(data) && bytes.Contains(data, []byte("<svg"))
}
