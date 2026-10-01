// Package tooldef answers questions about tool definitions that the runner
// and the session share.
package tooldef

import "github.com/deepnoodle-ai/dashi/tool"

// SafeRepeat reports whether def is annotated read-only or idempotent, so
// running a call to it again is harmless.
func SafeRepeat(def tool.Definition) bool {
	return def.Annotations.ReadOnly != nil &&
		*def.Annotations.ReadOnly ||
		def.Annotations.Idempotent != nil && *def.Annotations.Idempotent
}

// UnsafeProviderTool reports whether defs offers a provider tool that is not
// safe to repeat.
func UnsafeProviderTool(defs []tool.Definition) bool {
	for _, def := range defs {
		if def.Kind == tool.KindProviderTool && !SafeRepeat(def) {
			return true
		}
	}
	return false
}
