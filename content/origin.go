package content

import "errors"

// Origin records the source of provider-private data. Scope is an opaque
// namespace for the endpoint and credentials. Artifact-specific checks decide
// whether Model must match; aliases alone do not establish compatibility.
// Matching origins are a conservative prerequisite, not proof that a remote
// service still accepts an artifact. Origins are provenance, not authorization.
type Origin struct {
	// Provider names the configured destination, not the API format.
	Provider string `json:"provider"`

	// API identifies the wire format needed to replay the artifact.
	API string `json:"api"`

	// Model is the requested model name when the artifact has one. Keep known
	// provenance even when an artifact's compatibility rule ignores the model.
	Model string `json:"model,omitempty"`

	// Scope is an opaque endpoint and credential namespace used for replay
	// checks.
	Scope string `json:"scope"`
}

// Validate requires destination provenance. Model-bound artifacts such as
// private replay and citations must additionally require Model.
func (o Origin) Validate() error {
	if o.Provider == "" || o.API == "" || o.Scope == "" {
		return errors.New("content: origin requires provider, API, and scope")
	}
	return nil
}
