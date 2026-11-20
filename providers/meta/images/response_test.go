package images

import (
	"encoding/base64"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func TestDecodeImageResponse(t *testing.T) {
	origin := content.Origin{Provider: "meta", API: API, Scope: "s"}
	png := base64.StdEncoding.EncodeToString(tinyPNG)
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"base64":         {`{"output_format":"png","data":[{"b64_json":"` + png + `"}]}`, true},
		"no format":      {`{"data":[{"b64_json":"` + png + `"}]}`, true},
		"url":            {`{"output_format":"webp","data":[{"url":"https://cdn.example/a.webp"}]}`, true},
		"null usage":     {`{"data":[{"b64_json":"` + png + `"}],"usage":null}`, true},
		"format differs": {`{"output_format":"jpeg","data":[{"b64_json":"` + png + `"}]}`, false},
		"no images":      {`{"data":[]}`, false},
		"both":           {`{"data":[{"b64_json":"` + png + `","url":"https://cdn.example/a"}]}`, false},
		"bad url":        {`{"data":[{"url":"file:///etc/passwd"}]}`, false},
		"bad format":     {`{"output_format":"gif","data":[{"b64_json":"` + png + `"}]}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeImageResponse([]byte(tc.body), generateInput(), origin, "")
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, want ok %v", err, tc.ok)
			}
		})
	}
}
