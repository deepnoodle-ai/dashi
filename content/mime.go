package content

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

// UnknownMIMEType is what DetectMIMEType returns when neither the name nor the
// bytes identify the content.
const UnknownMIMEType = "application/octet-stream"

// extensionTypes is fixed rather than read from the system's MIME database,
// so detection gives the same answer on every machine and a recorded request
// can be prepared again identically.
var extensionTypes = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".html": "text/html",
	".htm":  "text/html",
	".json": "application/json",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// DetectMIMEType names the media type of inline bytes. A known file extension
// on name wins, since text formats such as Markdown and CSV cannot be told
// apart by content; otherwise the bytes are sniffed with
// http.DetectContentType. Parameters such as charset are removed. The result
// is UnknownMIMEType when nothing matches. Detection is deterministic.
func DetectMIMEType(data []byte, name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extensionTypes[ext]; ok {
		return t
	}
	detected := http.DetectContentType(data)
	if t, _, err := mime.ParseMediaType(detected); err == nil {
		return t
	}
	return detected
}
