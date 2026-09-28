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
	// SupportUnknown means no support claim has been established.
	SupportUnknown Support = ""
	// SupportSupported means capability support has been established.
	SupportSupported Support = "supported"
	// SupportUnsupported means known rejection for this capability.
	SupportUnsupported Support = "unsupported"
)

// Valid reports whether s is one of the three recorded support states.
func (s Support) Valid() bool {
	switch s {
	case SupportUnknown, SupportSupported, SupportUnsupported:
		return true
	}
	return false
}

// CapabilityPolicy decides what preparation does when a request goes beyond
// what is known about the model. The zero value means the default, adjust,
// because a library that fails on edge cases of uncertain meaning is harder to
// use than one that sends the nearest valid request and says so
// (design/decisions.md, entry 4). Strict keeps the older fail-closed behavior
// for applications that must not send anything unqualified. Neither policy
// overrides known rejection where no safe substitute exists, nor an encoding
// the client cannot perform.
//
// Recorded evidence always carries the resolved, non-empty value, so a record
// never depends on what the default was when it was written.
type CapabilityPolicy string

const (
	// CapabilityAdjust sends features whose support is unknown and records
	// them as unverified. A known limit is met by the nearest supported value,
	// recorded as an adjustment that carries a warning.
	CapabilityAdjust CapabilityPolicy = "adjust"
	// CapabilityStrict declines an operation with unknown required support or
	// a value outside a known limit.
	CapabilityStrict CapabilityPolicy = "strict"
)

// Valid reports whether p is the zero value or one of the defined policies.
func (p CapabilityPolicy) Valid() bool {
	switch p {
	case "", CapabilityAdjust, CapabilityStrict:
		return true
	}
	return false
}

// Resolved returns the policy that applies: CapabilityAdjust for the zero
// value, and p otherwise. Compare policies only after resolving them.
func (p CapabilityPolicy) Resolved() CapabilityPolicy {
	if p == "" {
		return CapabilityAdjust
	}
	return p
}

// EvidenceMethod says how a capability claim was established, not whether a
// later request will succeed.
type EvidenceMethod string

const (
	// EvidenceLiveProbe records a tested provider operation.
	EvidenceLiveProbe EvidenceMethod = "live_probe"
	// EvidenceDocumentation records a published provider claim.
	EvidenceDocumentation EvidenceMethod = "documentation"
	// EvidenceInference records a conclusion without direct qualification.
	EvidenceInference EvidenceMethod = "inference"
	// EvidenceFixture records behavior observed against a local fixture.
	EvidenceFixture EvidenceMethod = "fixture"
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
	if strings.TrimSpace(e.Model) == "" ||
		strings.TrimSpace(e.API) == "" ||
		strings.TrimSpace(e.Owner) == "" ||
		strings.TrimSpace(e.Source) == "" ||
		e.QualifiedAt.IsZero() {
		return errors.New("model: evidence requires model, API, owner, source, and date")
	}
	u, err := url.Parse(e.Endpoint)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "https" &&
			u.Scheme != "http") ||
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
