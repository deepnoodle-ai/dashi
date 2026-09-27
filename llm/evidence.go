package llm

import "github.com/deepnoodle-ai/kernel/model"

// EvidenceMethod is retained as an alias for existing model-call clients.
type EvidenceMethod = model.EvidenceMethod

const (
	// EvidenceLiveProbe records a tested provider operation.
	EvidenceLiveProbe = model.EvidenceLiveProbe
	// EvidenceDocumentation records a published provider claim.
	EvidenceDocumentation = model.EvidenceDocumentation
	// EvidenceInference records a conclusion without direct qualification.
	EvidenceInference = model.EvidenceInference
	// EvidenceFixture records behavior observed against a local fixture.
	EvidenceFixture = model.EvidenceFixture
)

// ModelEvidence is retained as an alias for existing model-call clients.
type ModelEvidence = model.ModelEvidence
