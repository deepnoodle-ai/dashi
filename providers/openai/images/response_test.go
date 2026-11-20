package images

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// tinyGIF is a 1x1 GIF89a image.
var tinyGIF = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff" +
	"!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

func TestDecodeImageSourceAcceptsGIFOutput(t *testing.T) {
	source, err := decodeImageSource(
		base64.StdEncoding.EncodeToString(tinyGIF),
		"",
		"gif",
		"gpt-image-2.6-flare",
	)
	if err != nil || source.MIMEType != "image/gif" {
		t.Fatalf("GIF output: %#v, %v", source, err)
	}
}

func TestDecodeImageSourceAcceptsSVGOutput(t *testing.T) {
	svg := `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`
	source, err := decodeImageSource(
		base64.StdEncoding.EncodeToString([]byte(svg)),
		"",
		"svg",
		"gpt-image-2.6-flare",
	)
	if err != nil || source.MIMEType != "image/svg+xml" ||
		string(source.Data) != svg {
		t.Fatalf("SVG output: %#v, %v", source, err)
	}
	if _, err := decodeImageSource(
		base64.StdEncoding.EncodeToString(tinyGIF),
		"",
		"svg",
		"gpt-image-2.6-flare",
	); err == nil {
		t.Fatal("GIF bytes declared as SVG were accepted")
	}
	raw := []byte(`{"output_format":"svg","data":[{"b64_json":"` +
		base64.StdEncoding.EncodeToString([]byte(svg)) +
		`"}],"usage":{"total_tokens":3},"svg_usage":{"model":"gpt-5.5","total_tokens":9}}`)
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	result, err := decodeImageResponse(
		raw,
		input,
		content.Origin{Provider: "openai"},
		"req_1",
	)
	if err != nil || result.Content[0].Image.Source.MIMEType != "image/svg+xml" {
		t.Fatalf("SVG result: %#v, %v", result, err)
	}
	var usage struct {
		TotalTokens int `json:"total_tokens"`
		SVGUsage    struct {
			Model string `json:"model"`
		} `json:"svg_usage"`
	}
	if err := json.Unmarshal(result.RawUsage, &usage); err != nil ||
		usage.TotalTokens != 3 || usage.SVGUsage.Model != "gpt-5.5" {
		t.Fatalf("raw usage %s: %v", result.RawUsage, err)
	}
}
