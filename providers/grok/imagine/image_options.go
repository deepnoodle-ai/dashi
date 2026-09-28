package imagine

// ImageResponseFormat selects xAI's temporary URL or inline base64 response.
type ImageResponseFormat string

const (
	// ImageResponseURL requests a temporary public URL for each output.
	ImageResponseURL ImageResponseFormat = "url"
	// ImageResponseBase64 requests inline image bytes.
	ImageResponseBase64 ImageResponseFormat = "b64_json"
)

// ImageOptions holds xAI-specific image response controls. The zero value
// requests the default temporary URL output.
type ImageOptions struct {
	ResponseFormat ImageResponseFormat `json:"response_format,omitempty"`
	Storage        *StorageOptions     `json:"storage,omitempty"`
}
