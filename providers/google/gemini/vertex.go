package gemini

import (
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

// vertexCredentials finds Application Default Credentials with the Vertex
// scope, as the GenAI SDK's UseDefaultCredentials does.
func vertexCredentials() (*auth.Credentials, error) {
	return credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{vertexScope},
	})
}
