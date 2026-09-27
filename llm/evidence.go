package llm

import "github.com/deepnoodle-ai/kernel/model"

// EvidenceMethod is retained as an alias for existing model-call clients.
type EvidenceMethod = model.EvidenceMethod

const (
	EvidenceLiveProbe     = model.EvidenceLiveProbe
	EvidenceDocumentation = model.EvidenceDocumentation
	EvidenceInference     = model.EvidenceInference
	EvidenceFixture       = model.EvidenceFixture
)

// ModelEvidence is retained as an alias for existing model-call clients.
type ModelEvidence = model.ModelEvidence
