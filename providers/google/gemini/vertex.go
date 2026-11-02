package gemini

import (
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

// vertexScope is the OAuth scope Vertex AI documents for its API. It has no
// narrower scope.
const vertexScope = "https://www.googleapis.com/auth/cloud-platform"

// vertexHost returns the Vertex AI host for location. The hosts follow the
// Google GenAI SDK: global has no regional prefix, and the multi-regions us
// and eu use regional endpoints.
func vertexHost(location string) string {
	switch location {
	case "global":
		return "aiplatform.googleapis.com"
	case "us", "eu":
		return "aiplatform." + location + ".rep.googleapis.com"
	default:
		return location + "-aiplatform.googleapis.com"
	}
}

// isVertexHost reports whether host is one of the forms vertexHost returns,
// so Dashi's own ADC token goes to Vertex AI and no other Google service.
// A host with a port is refused.
func isVertexHost(host string) bool {
	switch host {
	case "aiplatform.googleapis.com",
		"aiplatform.us.rep.googleapis.com",
		"aiplatform.eu.rep.googleapis.com":
		return true
	}
	location, ok := strings.CutSuffix(host, "-aiplatform.googleapis.com")
	return ok && location != "" && !strings.ContainsFunc(location, func(c rune) bool {
		return c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9')
	})
}

// vertexCredentials finds Application Default Credentials with the Vertex
// scope, as the GenAI SDK's UseDefaultCredentials does.
func vertexCredentials() (*auth.Credentials, error) {
	return credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{vertexScope},
	})
}
