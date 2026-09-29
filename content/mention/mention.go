// Package mention turns file words in a user's message into content blocks.
// It exists for terminal front ends: a user types "explain @main.go" or drops
// a screenshot on the input, and the model must receive the file.
//
// The user picks every attachment, so this package applies no workspace
// boundary. A model tool that reads files must enforce its own boundary; this
// package must not be used for that. It depends only on the standard library
// and package content. Dive's CLI attachment code guided the path forms.
//
// Attachments are inline: the bytes go into the returned blocks. A skipped
// file never fails the message; Expand reports it as a warning.
package mention

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
)

const (
	// defaultMaxImageBytes is 5 MiB x 3/4. Anthropic applies its 5 MiB image
	// limit to the base64 text, and base64 adds one third to the file size. An
	// image that passes this limit is thus accepted by every main provider.
	defaultMaxImageBytes = 5 << 20 * 3 / 4
	defaultMaxFileBytes  = 10 << 20
	defaultMaxTextBytes  = 256 << 10
	defaultMaxTotalBytes = 32 << 20

	// attachmentOverhead is the request size added to each attachment for
	// its JSON wrapper and media type.
	attachmentOverhead = 256

	// sniffBytes is how much of a file with an unknown extension Expand reads
	// to find its type.
	sniffBytes = 512

	// trailingPunctuation ends a sentence or a bracket, not a path.
	trailingPunctuation = "?!,.:;)]}`\"'"

	openingBrackets = "([{<`"
)

// Options sets the base directory and the size limits. A zero value uses the
// default. A negative limit turns that limit off.
type Options struct {
	// Root is the base for relative paths. Zero means the current directory.
	Root string

	// MaxImageBytes limits the file size of one image. Zero means 3.75 MB.
	// Anthropic applies its 5 MB image limit to the base64 text, which is one
	// third larger than the file. Thus an image within the default limit is
	// accepted by every main provider.
	MaxImageBytes int64

	// MaxFileBytes limits one PDF. Zero means 10 MB.
	MaxFileBytes int64

	// MaxTextBytes limits one text file. Zero means 256 KB.
	MaxTextBytes int64

	// MaxTotalBytes limits the request size of all files in one message. Images
	// and PDFs count as base64 text plus a small overhead. Zero means 32 MB.
	MaxTotalBytes int64
}

// Attachment describes one attached file for the screen.
type Attachment struct {
	// Path is the path as the user wrote it, without shell escapes.
	Path string

	// MIMEType is the type of the attached block.
	MIMEType string

	// Size is the byte count of the file.
	Size int64
}

// Result is the expanded message.
type Result struct {
	// Text is the input with each attached @mention replaced by a label such
	// as "[file: main.go]". Bare paths and skipped mentions stay as typed.
	Text string

	// Blocks holds one block per attached file, in order of appearance. A
	// file that appears twice has one block.
	Blocks []content.Block

	// Attached lists the files in Blocks, in the same order.
	Attached []Attachment

	// Warnings holds one plain sentence for each skipped mention.
	Warnings []string
}

type fileKind int

const (
	kindText fileKind = iota
	kindImage
	kindPDF
)

// Expand finds file words in input and reads the files.
//
// A mention is "@path" or `@"path with spaces"`, at the start of the text or
// after a space or an opening bracket. A path may be relative to Options.Root,
// absolute, start with "~", or be a file:// URL. A word without "@" attaches
// only when it names an existing image or PDF file, so a pasted stack trace
// stays prose. Expand reads a shell-style quote or backslash escape, the form
// a terminal inserts when a user drops a file.
//
// Images are PNG, JPEG, GIF, and WebP. A PDF and a text file become document
// blocks. Any other file gets a warning. Expand never returns an error.
func Expand(input string, opt Options) Result {
	e := expander{
		opt:  opt.withDefaults(),
		seen: map[string]bool{},
	}
	return e.expand(input)
}

func (o Options) withDefaults() Options {
	if o.Root == "" {
		o.Root = "."
	}
	if o.MaxImageBytes == 0 {
		o.MaxImageBytes = defaultMaxImageBytes
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = defaultMaxFileBytes
	}
	if o.MaxTextBytes == 0 {
		o.MaxTextBytes = defaultMaxTextBytes
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = defaultMaxTotalBytes
	}
	return o
}

type expander struct {
	opt   Options
	seen  map[string]bool
	total int64
	res   Result
}

// word is one whitespace-delimited word of the input.
type word struct {
	start   int
	end     int
	mention bool

	// path is the path text without "@", quotes, or escapes.
	path string

	// pathEnd ends the path text; the rest of the word stays as prose.
	pathEnd int
}

func (e *expander) expand(input string) Result {
	var out strings.Builder
	last := 0
	for _, w := range scanWords(input) {
		label, ok := e.attach(input, &w)
		if !ok {
			continue
		}
		out.WriteString(input[last:w.start])
		out.WriteString(label)
		last = w.pathEnd
	}
	out.WriteString(input[last:])
	e.res.Text = out.String()
	return e.res
}

// attach handles one word. It returns the replacement label when the word is
// an attached mention. A bare path returns false: its text stays.
func (e *expander) attach(input string, w *word) (string, bool) {
	typed := input[w.start:w.pathEnd]
	if w.path == "" {
		return "", false
	}
	if _, known := knownExtension(w.path); !w.mention && !known {
		return "", false
	}
	full, info, err := e.lookup(w.path)
	if err != nil && w.mention {
		// A line reference such as "main.go:42" names the file "main.go".
		if m := lineSuffix.FindString(w.path); m != "" {
			if f, i, err2 := e.lookup(w.path[:len(w.path)-len(m)]); err2 == nil {
				full, info, err = f, i, nil
				w.path = w.path[:len(w.path)-len(m)]
				w.pathEnd -= len(m)
				typed = input[w.start:w.pathEnd]
			}
		}
	}
	if err != nil {
		if w.mention {
			if os.IsNotExist(err) {
				e.warn(typed, "no such file.")
			} else {
				e.warn(typed, "the path is not valid.")
			}
		}
		return "", false
	}
	if info.IsDir() {
		if w.mention {
			e.warn(typed, "it is a directory.")
		}
		return "", false
	}
	if !info.Mode().IsRegular() {
		if w.mention {
			e.warn(typed, "it is not a regular file.")
		}
		return "", false
	}
	kind, mimeType, ok := classify(full, w.path)
	if !ok {
		if w.mention {
			e.warn(typed, "not text, an image, or a PDF.")
		}
		return "", false
	}
	label := "[file: " + w.path + "]"
	if kind == kindImage {
		label = "[image: " + w.path + "]"
	}
	if e.seen[full] {
		if w.mention {
			return label, true
		}
		return "", false
	}
	block, size, ok := e.read(typed, full, w.path, kind, mimeType, info.Size())
	if !ok {
		return "", false
	}
	e.seen[full] = true
	e.total += wireSize(kind, size)
	e.res.Blocks = append(e.res.Blocks, block)
	e.res.Attached = append(e.res.Attached, Attachment{
		Path:     w.path,
		MIMEType: mimeType,
		Size:     size,
	})
	return label, w.mention
}

// lineSuffix matches a line reference at the end of a path: ":42" or ":42-50".
var lineSuffix = regexp.MustCompile(`:\d+(-\d+)?$`)

// lookup resolves a path and reads its file information.
func (e *expander) lookup(path string) (string, os.FileInfo, error) {
	full, err := e.resolve(path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(full)
	return full, info, err
}

// classify decides how a file is sent. It looks at the extension first, then
// at the first bytes. ok is false for an image type that providers do not
// accept. The text check itself happens when the file is read.
func classify(full string, name string) (fileKind, string, bool) {
	var header []byte
	if _, known := knownExtension(name); !known {
		header = readHeader(full)
	}
	mimeType := content.DetectMIMEType(header, name)
	switch {
	case mimeType == "application/pdf":
		return kindPDF, mimeType, true
	case isImageType(mimeType):
		return kindImage, mimeType, true
	case strings.HasPrefix(mimeType, "image/"):
		return kindText, mimeType, false
	}
	return kindText, "text/plain", true
}

func knownExtension(name string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf":
		return ext, true
	}
	return ext, false
}

// wireSize is the size a file adds to the provider request. Providers send
// images, PDFs, and (in the largest case, OpenAI) text documents as base64
// text, which is four bytes for every three. Count every kind that way.
func wireSize(kind fileKind, size int64) int64 {
	size = (size + 2) / 3 * 4
	return size + attachmentOverhead
}

func isImageType(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func readHeader(full string) []byte {
	f, err := os.Open(full)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, buf)
	return buf[:n]
}

// read checks the limits, reads the file, and builds its block. It warns and
// returns false when the file must be skipped.
func (e *expander) read(
	typed string,
	full string,
	name string,
	kind fileKind,
	mimeType string,
	size int64,
) (content.Block, int64, bool) {
	var block content.Block
	limit, what := e.opt.MaxTextBytes, "text"
	switch kind {
	case kindImage:
		limit, what = e.opt.MaxImageBytes, "image"
	case kindPDF:
		limit, what = e.opt.MaxFileBytes, "PDF"
	}
	if limit >= 0 && size > limit {
		e.warn(typed, "%s is over the %s %s limit.", sizeText(size), sizeText(limit), what)
		return block, 0, false
	}
	if e.opt.MaxTotalBytes >= 0 && e.total+wireSize(kind, size) > e.opt.MaxTotalBytes {
		e.warn(typed, "it would pass the %s limit for one message.", sizeText(e.opt.MaxTotalBytes))
		return block, 0, false
	}
	data, err := readFile(full, limit)
	if err != nil {
		e.warn(typed, "it could not be read.")
		return block, 0, false
	}
	if limit >= 0 && int64(len(data)) > limit {
		e.warn(typed, "%s is over the %s %s limit.", sizeText(int64(len(data))), sizeText(limit), what)
		return block, 0, false
	}
	if len(data) == 0 {
		e.warn(typed, "the file is empty.")
		return block, 0, false
	}
	source := content.Source{Data: data, MIMEType: mimeType}
	switch kind {
	case kindImage:
		block = content.ImageBlock(source)
	case kindPDF:
		block = content.DocumentBlock(source, name)
	default:
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			e.warn(typed, "not text, an image, or a PDF.")
			return block, 0, false
		}
		block = content.DocumentBlock(source, name)
	}
	return block, int64(len(data)), true
}

func readFile(full string, limit int64) ([]byte, error) {
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if limit >= 0 {
		// One extra byte shows that the file grew after the size check.
		r = io.LimitReader(f, limit+1)
	}
	return io.ReadAll(r)
}

func (e *expander) warn(typed string, format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	e.res.Warnings = append(e.res.Warnings, "Skipped "+typed+": "+reason)
}

// resolve turns the path text into an absolute file path.
func (e *expander) resolve(p string) (string, error) {
	if strings.HasPrefix(p, "file://") {
		u, err := url.Parse(p)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			return "", fmt.Errorf("mention: invalid file URL")
		}
		p = u.Path
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.opt.Root, p)
	}
	return filepath.Abs(p)
}

// sizeText formats a byte count with one unit. A fraction rounds up, so a
// warning never says "5.0 MB is over the 5 MB limit".
func sizeText(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<20:
		return roundUp(n, 1<<20) + " MB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	case n >= 1<<10:
		return roundUp(n, 1<<10) + " KB"
	}
	return fmt.Sprintf("%d bytes", n)
}

// roundUp divides n by unit, rounds up to two decimals, and drops trailing
// zeros.
func roundUp(n, unit int64) string {
	v := math.Ceil(float64(n)/float64(unit)*100) / 100
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// scanWords splits input into words on unescaped whitespace. A word that
// starts with a quote, or with "@" and a quote, ends at the closing quote.
// Elsewhere a quote is an ordinary character, so an apostrophe in prose
// cannot swallow the words after it. A word is a mention only when "@" comes
// first, after optional opening brackets.
func scanWords(input string) []word {
	var words []word
	i := 0
	for i < len(input) {
		if isSpace(input[i]) {
			i++
			continue
		}
		w := scanWord(input, i)
		words = append(words, w)
		i = max(w.end, i+1)
	}
	return words
}

func scanWord(input string, start int) word {
	p := start
	for p < len(input) && strings.IndexByte(openingBrackets, input[p]) >= 0 {
		p++
	}
	// Opening brackets are prose, so the word starts after them. A quote
	// directly before "@" is prose too.
	if p+1 < len(input) && (input[p] == '"' || input[p] == '\'') && input[p+1] == '@' {
		p++
	}
	w := word{start: p}
	if p < len(input) && input[p] == '@' {
		w.mention = true
		p++
	}
	if p < len(input) && (input[p] == '"' || input[p] == '\'') {
		quote := input[p]
		if n := strings.IndexByte(input[p+1:], quote); n >= 0 {
			w.path = input[p+1 : p+1+n]
			w.pathEnd = p + 1 + n + 1
			w.end = w.pathEnd
			for w.end < len(input) && !isSpace(input[w.end]) {
				w.end++
			}
			return w
		}
	}
	end := p
	for end < len(input) && !isSpace(input[end]) {
		if input[end] == '\\' && end+1 < len(input) {
			end++
		}
		end++
	}
	w.end = end
	trimmed := strings.TrimRight(input[p:end], trailingPunctuation)
	w.pathEnd = p + len(trimmed)
	w.path = unescape(trimmed)
	return w
}

// unescape removes shell backslash escapes.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
