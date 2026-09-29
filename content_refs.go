package dashi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

const (
	defaultMaxRefBytes   = 32 << 20
	defaultMaxTotalBytes = 64 << 20
)

// ContentResolver turns an application reference into bytes. The application
// owns the storage and the access rules; Dashi records only the reference.
//
// The runner calls Resolve before every model request that holds the
// reference, on every retry and every resume. It keeps no cache, so a
// reference the application has revoked stops the turn at the next call.
type ContentResolver interface {
	// Resolve returns the bytes of ref. ctx is the context of the Run or
	// Stream call, so the application can read its tenant and session from
	// it. turnID names the turn that asks. The application must check that
	// the turn may read ref, and return an error when it may not. The runner
	// checks the length and the SHA-256 of the result, but the application
	// must bound its own read.
	Resolve(
		ctx context.Context,
		turnID string,
		ref content.Ref,
	) ([]byte, error)
}

// ContentLimits bounds the bytes that references add to one model request.
// The runner compares the declared Size of each reference, so it rejects an
// oversized reference before any read. Zero means the default. A negative
// value means no limit.
type ContentLimits struct {
	// MaxRefBytes limits one reference. Zero means 32 MiB.
	MaxRefBytes int64

	// MaxTotalBytes limits all references in one model request, counted
	// once per use. Zero means 64 MiB. Inline data is not counted.
	MaxTotalBytes int64
}

// Reasons a content reference stops a turn. The stop message names the
// reference ID and one reason, and never includes bytes.
const (
	refNoResolver     = "no_resolver"
	refUnavailable    = "unavailable"
	refTooLarge       = "too_large"
	refSizeMismatch   = "size_mismatch"
	refDigestMismatch = "digest_mismatch"
)

// hydrate returns input with each content reference replaced by verified
// bytes. Input without references is returned as is, with no copy. The
// returned input is a copy, so the recorded, reference-bearing messages stay
// unchanged. A non-nil stop ends the turn as failed: the runner never drops a
// reference or sends the request without it.
func (r *runtime) hydrate(input llm.Input) (llm.Input, *Outcome) {
	if !input.HasRef() {
		return input, nil
	}
	messages := cloneMessages(input.Messages)
	var sources []*content.Source
	for _, m := range messages {
		for _, c := range m.Content {
			for _, s := range c.Sources() {
				if s.Ref != nil {
					sources = append(sources, s)
				}
			}
		}
	}
	if reason, id := r.checkRefSizes(sources); reason != "" {
		return input, r.refStop(id, reason)
	}
	for _, s := range sources {
		data, reason := r.resolveRef(*s.Ref)
		if reason != "" {
			if r.ctx.Err() != nil {
				o := r.contextStop()
				return input, &o
			}
			return input, r.refStop(s.Ref.ID, reason)
		}
		s.Data = data
		s.Ref = nil
	}
	input.Messages = messages
	return input, nil
}

// checkRefSizes compares declared sizes with the limits. It needs no I/O and
// runs before the first Resolve call, so an oversized reference costs nothing.
func (r *runtime) checkRefSizes(sources []*content.Source) (string, string) {
	maxRef := limitOrDefault(r.runner.ContentLimits.MaxRefBytes, defaultMaxRefBytes)
	maxTotal := limitOrDefault(
		r.runner.ContentLimits.MaxTotalBytes,
		defaultMaxTotalBytes,
	)
	var total int64
	for _, s := range sources {
		size := s.Ref.Size
		if maxRef >= 0 && size > maxRef {
			return refTooLarge, s.Ref.ID
		}
		total += size
		if maxTotal >= 0 && total > maxTotal {
			return refTooLarge, s.Ref.ID
		}
	}
	if r.runner.ContentResolver == nil {
		return refNoResolver, sources[0].Ref.ID
	}
	return "", ""
}

func limitOrDefault(limit, fallback int64) int64 {
	if limit == 0 {
		return fallback
	}
	return limit
}

// resolveRef calls the resolver and verifies the result against the
// reference. It returns a reason, never the resolver's own error text.
func (r *runtime) resolveRef(ref content.Ref) ([]byte, string) {
	data, err := r.runner.ContentResolver.Resolve(r.ctx, r.turn.ID, ref)
	if err != nil {
		return nil, refUnavailable
	}
	if int64(len(data)) != ref.Size {
		return nil, refSizeMismatch
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal([]byte(hex.EncodeToString(sum[:])), []byte(ref.SHA256)) {
		return nil, refDigestMismatch
	}
	return data, ""
}

func (r *runtime) refStop(id, reason string) *Outcome {
	o := r.stop(
		StopFailed,
		fmt.Sprintf("content reference %q: %s", id, reason),
	)
	return &o
}
