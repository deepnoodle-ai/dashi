package images

import (
	"encoding/base64"
	"testing"
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
