package images

import (
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/media"
)

// Options holds controls specific to OpenAI's Images API. An unset field
// leaves the provider default in effect. OutputCompression uses a pointer so
// zero can be requested explicitly.
type Options struct {
	Background        string `json:"background,omitempty"`
	Moderation        string `json:"moderation,omitempty"`
	InputFidelity     string `json:"input_fidelity,omitempty"`
	OutputCompression *int   `json:"output_compression,omitempty"`
	// Complexity is "low", "medium", or "high" and sets how much detail
	// GPT Image 2.6 plans into the image. OpenAI defaults to high.
	Complexity string `json:"complexity,omitempty"`
}

func (o Options) clone() Options {
	if o.OutputCompression != nil {
		value := *o.OutputCompression
		o.OutputCompression = &value
	}
	return o
}

func (o Options) fields(input media.ImageInput) (map[string]any, error) {
	fields := make(map[string]any)
	switch o.Background {
	case "":
	case "auto", "opaque", "transparent":
		if o.Background == "transparent" &&
			input.Settings.OutputMIMEType == "image/jpeg" {
			return nil, errors.New("images: transparent background requires PNG or WebP output")
		}
		fields["background"] = o.Background
	default:
		return nil, fmt.Errorf("images: unsupported background %q", o.Background)
	}
	switch o.Moderation {
	case "":
	case "auto", "low":
		fields["moderation"] = o.Moderation
	default:
		return nil, fmt.Errorf("images: unsupported moderation %q", o.Moderation)
	}
	switch o.InputFidelity {
	case "":
	case "high", "low":
		if input.Operation != media.ImageEdit {
			return nil, errors.New("images: input fidelity requires an image edit")
		}
		switch gptImageModels[input.Model].fidelity {
		case "fixed":
			return nil, errors.New("images: GPT Image 2 input fidelity is fixed at high")
		case "low":
			if o.InputFidelity != "low" {
				return nil, fmt.Errorf(
					"images: %s accepts only low input fidelity",
					input.Model,
				)
			}
		}
		fields["input_fidelity"] = o.InputFidelity
	default:
		return nil, fmt.Errorf(
			"images: unsupported input fidelity %q",
			o.InputFidelity,
		)
	}
	switch o.Complexity {
	case "":
	case "low", "medium", "high":
		if known, ok := gptImageModels[input.Model]; ok && !known.complexity {
			return nil, errors.New("images: complexity requires GPT Image 2.6")
		}
		fields["complexity"] = o.Complexity
	default:
		return nil, fmt.Errorf("images: unsupported complexity %q", o.Complexity)
	}
	if o.OutputCompression != nil {
		if *o.OutputCompression < 0 || *o.OutputCompression > 100 {
			return nil, errors.New("images: output compression must be between 0 and 100")
		}
		if input.Settings.OutputMIMEType != "image/jpeg" &&
			input.Settings.OutputMIMEType != "image/webp" {
			return nil, errors.New("images: output compression requires JPEG or WebP output")
		}
		fields["output_compression"] = *o.OutputCompression
	}
	return fields, nil
}
