package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/mediakind"
)

const (
	// textBytesPerToken is how many bytes of encoded text count as one token.
	textBytesPerToken = 4
	// imageTokens is what one image counts, whatever its size.
	imageTokens = 1_600
	// documentPageTokens is what one page of a document that is not text counts.
	documentPageTokens = 2_000
	// documentPageBytes is the inline size of one page when no PDF pages are found.
	documentPageBytes = 100_000
	// videoSecondTokens is what one second of video of known length counts.
	videoSecondTokens = 300
	// mediaBytesPerToken is how many inline bytes of audio, or of video of
	// unknown length, count as one token.
	mediaBytesPerToken = 500
	// unknownMediaTokens is what audio or video counts when neither its bytes
	// nor its length are known.
	unknownMediaTokens = 1_600
)

// InputTokenUpperBound is a conservative size bound for a prepared request:
// one token per byte of the wire body, plus 1024. Fitting a context window
// uses it, since that must not underestimate. It overcounts text about four
// times and media far more, so budget checks use EstimatedInputTokens.
func InputTokenUpperBound(request Request) int {
	return addTokens(len(request.body), 1024)
}

// EstimatedInputTokens is a realistic estimate of a prepared request's input
// tokens, not a bound: it can run low as well as high. It is
// RequestSnapshot.EstimatedInputTokens when the preparer set one. Otherwise
// text counts one token per four bytes of the wire body, and each image,
// document page, audio, or video counts a fixed amount instead of its
// encoded bytes. The estimate never exceeds the model's known context
// window.
func EstimatedInputTokens(request Request) int {
	snapshot := request.Snapshot()
	tokens := snapshot.EstimatedInputTokens
	if tokens <= 0 {
		tokens = estimatedTokens(len(request.body), snapshot.Input.Messages)
	}
	if info := snapshot.ModelInfo; info != nil && info.Capabilities.ContextWindow > 0 {
		tokens = min(tokens, info.Capabilities.ContextWindow)
	}
	return tokens
}

// EstimatedMessageTokens estimates the input tokens messages add to a
// request before they are in one, by the rule EstimatedInputTokens uses for
// a request body, applied to their JSON encoding.
func EstimatedMessageTokens(messages []Message) int {
	if len(messages) == 0 {
		return 0
	}
	b, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return estimatedTokens(len(b), messages)
}

// estimatedTokens counts size encoded bytes holding messages: the bytes
// that are not inline media count as text, and media counts per item.
func estimatedTokens(size int, messages []Message) int {
	encoded, media := 0, 0
	visit := func(b content.Block) {
		if !mediakind.IsMedia(b) {
			return
		}
		n, inline := mediaTokens(b)
		media = addTokens(media, n)
		if inline > 0 {
			encoded = addTokens(encoded, encodedLen(inline))
		}
	}
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Block != nil {
				visit(*part.Block)
			}
			var nested []content.Block
			switch {
			case part.ToolResult != nil:
				nested = part.ToolResult.Output.Content
			case part.ProviderToolUse != nil:
				nested = part.ProviderToolUse.Output
			case part.ProviderToolResult != nil:
				nested = part.ProviderToolResult.Output
			}
			for _, b := range nested {
				visit(b)
			}
		}
	}
	text := max(size-encoded, 0)
	return addTokens(ceilDiv(text, textBytesPerToken), media)
}

// mediaTokens returns what one media block counts and the size of its
// inline bytes, zero when it has none.
func mediaTokens(b content.Block) (tokens, inline int) {
	switch {
	case b.Image != nil:
		return imageTokens, len(b.Image.Source.Data)
	case b.Document != nil:
		data := b.Document.Source.Data
		if len(data) == 0 {
			return documentPageTokens, 0
		}
		pages := pdfPages(data)
		if pages == 0 {
			pages = max(ceilDiv(len(data), documentPageBytes), 1)
		}
		return mulTokens(pages, documentPageTokens), len(data)
	case b.Audio != nil:
		return timedMediaTokens(len(b.Audio.Source.Data), 0), len(b.Audio.Source.Data)
	case b.Video != nil:
		data := b.Video.Source.Data
		return timedMediaTokens(len(data), b.Video.Duration), len(data)
	}
	return 0, 0
}

// timedMediaTokens counts audio or video: by length when known, else by
// inline bytes, else a fixed amount.
func timedMediaTokens(size int, duration time.Duration) int {
	switch {
	case duration > 0:
		seconds := int(duration / time.Second)
		if duration%time.Second != 0 {
			seconds++
		}
		return mulTokens(seconds, videoSecondTokens)
	case size > 0:
		return max(ceilDiv(size, mediaBytesPerToken), 1)
	}
	return unknownMediaTokens
}

// pdfPages counts the page objects in PDF bytes ("/Type /Page" or
// "/Type/Page", never "/Pages").
func pdfPages(data []byte) int {
	pages := 0
	for _, marker := range [][]byte{[]byte("/Type /Page"), []byte("/Type/Page")} {
		rest := data
		for {
			i := bytes.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			if len(rest) > 0 && isPDFNameByte(rest[0]) {
				continue
			}
			pages++
		}
	}
	return pages
}

// isPDFNameByte reports a byte that continues a PDF name, as the "s" of
// "/Pages" does.
func isPDFNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// encodedLen is the base64 size of n bytes, saturating.
func encodedLen(n int) int {
	if n > (math.MaxInt/4)*3-2 {
		return math.MaxInt
	}
	return base64.StdEncoding.EncodedLen(n)
}

func ceilDiv(n, d int) int {
	return n/d + min(1, n%d)
}

func addTokens(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

func mulTokens(a, b int) int {
	if a > 0 && b > math.MaxInt/a {
		return math.MaxInt
	}
	return a * b
}
