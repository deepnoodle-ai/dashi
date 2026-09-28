// Package policyrecord reads the capability policy from recorded evidence,
// including records written before the policy had its current name. It lives
// apart from model so the legacy field does not become public vocabulary.
package policyrecord

import (
	"fmt"

	"github.com/deepnoodle-ai/kernel/model"
)

// Decode returns the policy a record was prepared under. current is the
// capability_policy field and legacy the unknown_capabilities field it
// replaced; nil means the field was absent.
//
// Legacy records spelled the policy differently and treated an absent field as
// reject, its zero value. Reading them as the current zero value would turn
// strict evidence into adjust evidence, so an absent field decodes as strict.
func Decode(
	current *model.CapabilityPolicy,
	legacy *string,
) (model.CapabilityPolicy, error) {
	switch {
	case current != nil && !current.Valid():
		return "", fmt.Errorf("invalid capability policy %q", *current)
	case current != nil:
		return current.Resolved(), nil
	case legacy == nil:
		return model.CapabilityStrict, nil
	case *legacy == "allow":
		return model.CapabilityAdjust, nil
	case *legacy == "reject":
		return model.CapabilityStrict, nil
	}
	return "", fmt.Errorf(
		"invalid legacy unknown_capabilities %q",
		*legacy,
	)
}
