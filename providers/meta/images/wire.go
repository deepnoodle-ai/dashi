package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

const (
	// maxCount is the documented n range, 1 to 10.
	maxCount = 10
	// maxReferenceCount is the documented images range for edits, 1 to 10.
	maxReferenceCount = 10
	// maxAspectRatio is the documented bound: "Aspect ratios beyond 16:1
	// return 400".
	maxAspectRatio = 16
	// minSizeEdge is the shorter edge Dashi sends for an aspect ratio. Muse
	// renders at its own resolution, so the value only carries the ratio.
	minSizeEdge = 1024
	// maxImageURLBytes bounds an inline data URL. Meta documents no limit;
	// this matches the OpenAI-compatible bound.
	maxImageURLBytes = 20_971_520
)

// encodeImage returns the JSON body and, when the caller set a size or aspect
// ratio, the adjustment that says Muse uses it only as an aspect ratio.
func encodeImage(input media.ImageInput, prompt string, options Options,
	files content.Origin) ([]byte, *media.Adjustment, error) {
	if input.Mask != nil {
		return nil, nil, errors.New("images: Muse Image does not accept a mask")
	}
	if input.Operation == media.ImageGenerate && len(input.ReferenceImages) > 0 {
		return nil, nil, errors.New("images: generation does not accept reference images")
	}
	s := input.Settings
	if s.Quality != "" {
		return nil, nil, errors.New("images: Muse Image has no quality setting")
	}
	if s.Resolution != "" {
		return nil, nil, errors.New("images: Muse Image renders at a fixed resolution; set an aspect ratio")
	}
	fields := map[string]any{"model": input.Model, "prompt": prompt}
	size, adjustment, err := encodeSize(s)
	if err != nil {
		return nil, nil, err
	}
	if size != "" {
		fields["size"] = size
	}
	if s.Count > 0 {
		if s.Count > maxCount {
			return nil, nil, fmt.Errorf("images: image count must be 1 to %d", maxCount)
		}
		fields["n"] = s.Count
	}
	if s.OutputMIMEType != "" {
		format, err := outputFormat(s.OutputMIMEType)
		if err != nil {
			return nil, nil, err
		}
		fields["output_format"] = format
	}
	optionFields, err := options.fields()
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(fields, optionFields)
	if input.Operation == media.ImageEdit {
		if len(input.ReferenceImages) > maxReferenceCount {
			return nil, nil, fmt.Errorf(
				"images: at most %d edit images are supported",
				maxReferenceCount,
			)
		}
		refs := make([]map[string]string, 0, len(input.ReferenceImages))
		for _, block := range input.ReferenceImages {
			if block.Image.Detail != "" {
				return nil, nil, errors.New("images: edit images do not accept image detail")
			}
			ref, err := imageReference(block.Image.Source, files)
			if err != nil {
				return nil, nil, err
			}
			refs = append(refs, ref)
		}
		fields["images"] = refs
	}
	body, err := json.Marshal(fields)
	return body, adjustment, err
}

// encodeSize turns an exact size or a whole-number aspect ratio into Muse's
// "WxH" size string. Muse reads it only as an aspect ratio and renders at its
// own fixed resolution, so an aspect ratio is scaled to a 1024-pixel shorter
// edge and the adjustment records that the pixel size is not honored.
func encodeSize(s media.ImageSettings) (string, *media.Adjustment, error) {
	var width, height int
	requested := ""
	switch {
	case s.Size.Width > 0:
		width, height = s.Size.Width, s.Size.Height
		requested = fmt.Sprintf("%dx%d", width, height)
	case s.AspectRatio != "":
		a, b, ok := parseRatio(s.AspectRatio)
		if !ok {
			return "", nil, fmt.Errorf(
				"images: aspect ratio %q must be two whole numbers, such as 16:9",
				s.AspectRatio,
			)
		}
		scale := (minSizeEdge + min(a, b) - 1) / min(a, b)
		width, height = a*scale, b*scale
		requested = s.AspectRatio
	default:
		return "", nil, nil
	}
	larger, smaller := max(width, height), min(width, height)
	if larger > maxAspectRatio*smaller {
		return "", nil, fmt.Errorf("images: aspect ratio %s exceeds 16:1", requested)
	}
	size := strconv.Itoa(width) + "x" + strconv.Itoa(height)
	return size, &media.Adjustment{
		Setting:   "size",
		Requested: requested,
		Effective: "aspect ratio of " + size,
		Reason:    "Muse Image reads size only as an aspect ratio and renders at its own resolution",
	}, nil
}

func parseRatio(ratio string) (int, int, bool) {
	left, right, ok := strings.Cut(ratio, ":")
	if !ok {
		return 0, 0, false
	}
	a, errA := strconv.Atoi(left)
	b, errB := strconv.Atoi(right)
	if errA != nil || errB != nil || a <= 0 || b <= 0 ||
		a > minSizeEdge || b > minSizeEdge {
		return 0, 0, false
	}
	return a, b, true
}

func encodePrompt(blocks []content.Block) (string, error) {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind != content.KindText || block.Text == nil ||
			len(block.Text.Citations) > 0 {
			return "", errors.New("images: prompt requires plain text blocks")
		}
		parts = append(parts, block.Text.Text)
	}
	prompt := strings.Join(parts, "\n")
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("images: prompt requires non-empty text")
	}
	return prompt, nil
}

// imageReference encodes one edit image as image_url (an HTTP URL or a data
// URL for inline bytes) or file_id (a Meta Files API ID bound to this
// destination). Dashi never fetches a URL; Meta does.
func imageReference(source content.Source,
	files content.Origin) (map[string]string, error) {
	source = source.WithDetectedMIMEType("")
	switch {
	case source.Data != nil:
		if !supportedInputMIME(source.MIMEType) {
			return nil, fmt.Errorf("images: unsupported input MIME type %q", source.MIMEType)
		}
		if base64.StdEncoding.EncodedLen(len(source.Data))+
			len("data:"+source.MIMEType+";base64,") > maxImageURLBytes {
			return nil, errors.New("images: image data URL exceeds 20 MiB")
		}
		return map[string]string{"image_url": source.DataURL()}, nil
	case source.URL != "":
		parsed, err := url.Parse(source.URL)
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") ||
			parsed.User != nil {
			return nil, errors.New("images: image reference requires an HTTP URL")
		}
		if source.ExpiresAt != nil && !source.ExpiresAt.After(time.Now()) {
			return nil, errors.New("images: image reference has expired")
		}
		return map[string]string{"image_url": source.URL}, nil
	case source.ID != "":
		if source.Origin != files {
			return nil, errors.New("images: file ID belongs to another Files API destination")
		}
		return map[string]string{"file_id": source.ID}, nil
	case source.Ref != nil:
		return nil, fmt.Errorf(
			"images: content reference %q cannot be an image reference; pass the bytes",
			source.Ref.ID,
		)
	}
	return nil, errors.New("images: provider URI cannot be used as an image reference")
}

func supportedInputMIME(mimeType string) bool {
	return mimeType == "image/png" || mimeType == "image/jpeg" ||
		mimeType == "image/webp"
}

func outputFormat(mimeType string) (string, error) {
	switch mimeType {
	case "image/webp":
		return "webp", nil
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpeg", nil
	}
	return "", fmt.Errorf("images: unsupported output MIME type %q", mimeType)
}
