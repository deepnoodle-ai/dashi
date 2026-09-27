// Package model holds provider identity and evidence shared by model-backed
// operations. Task-specific capabilities, settings, and usage stay with the
// operation that owns them.
package model

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// ModelRef identifies a model at an application-configured destination.
// Provider is not an API format or a live client object.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Support distinguishes known support, known rejection, and absent knowledge.
type Support string

const (
	SupportUnknown     Support = ""
	SupportSupported   Support = "supported"
	SupportUnsupported Support = "unsupported"
)

// UnknownPolicy controls incomplete capability knowledge. It never overrides
// known rejection or an encoding the client cannot perform.
type UnknownPolicy string

const (
	UnknownReject UnknownPolicy = ""
	UnknownAllow  UnknownPolicy = "allow"
)

// EvidenceMethod says how a capability claim was established, not whether a
// later request will succeed.
type EvidenceMethod string

const (
	EvidenceLiveProbe     EvidenceMethod = "live_probe"
	EvidenceDocumentation EvidenceMethod = "documentation"
	EvidenceInference     EvidenceMethod = "inference"
	EvidenceFixture       EvidenceMethod = "fixture"
)

// ModelEvidence scopes capability claims to an exact model, API, and endpoint.
// The application owns the claims; Kernel does not upgrade their quality.
type ModelEvidence struct {
	Model       string         `json:"model"`
	API         string         `json:"api"`
	Endpoint    string         `json:"endpoint"`
	Owner       string         `json:"owner"`
	Source      string         `json:"source"`
	Method      EvidenceMethod `json:"method"`
	QualifiedAt time.Time      `json:"qualified_at"`
}

// Validate requires dated provenance and a concrete HTTP endpoint. It does
// not verify the truth, coverage, or freshness of the supplied claims.
func (e ModelEvidence) Validate() error {
	if strings.TrimSpace(e.Model) == "" || strings.TrimSpace(e.API) == "" ||
		strings.TrimSpace(e.Owner) == "" || strings.TrimSpace(e.Source) == "" ||
		e.QualifiedAt.IsZero() {
		return errors.New("model: evidence requires model, API, owner, source, and date")
	}
	u, err := url.Parse(e.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.Fragment != "" {
		return errors.New("model: evidence requires an HTTP endpoint without user info or fragment")
	}
	switch e.Method {
	case EvidenceLiveProbe, EvidenceDocumentation, EvidenceInference,
		EvidenceFixture:
		return nil
	}
	return errors.New("model: evidence requires a known method")
}
