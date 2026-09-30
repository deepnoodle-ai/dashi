package main

import (
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"net/url"
	"strings"
)

// modelInfo uses the same exact endpoint resolution as the selected adapter.
// A custom endpoint never inherits facts from a public vendor route.
func (d configuredDestination) modelInfo(model string) (llm.ModelInfo, bool) {
	endpoint := d.Definition.Endpoint
	if d.Definition.API == "google.generate_content" {
		endpoint = strings.ReplaceAll(endpoint, "{model}", url.PathEscape(model))
	}
	return catalog.Lookup(d.Definition.API, endpoint, model)
}
