package dashi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/contentsrc"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

const (
	defaultMaxRefBytes   = 32 << 20
	defaultMaxTotalBytes = 64 << 20
)

// ContentLoader turns an application reference into bytes. The application
// owns the storage and the access rules; Dashi records only the reference.
//
// The runner calls Resolve before every model request that holds the
// reference, on every retry and every resume. A failure stops the turn as
// failed with a StopReasonContent... code and Stop.RefID. Continue calls
// Resolve again, so a host can restore the bytes or grant access and resume. It keeps no cache, so a
// reference the application has revoked stops the turn at the next call.
type ContentLoader interface {
	// Load returns the bytes of ref. ctx is the context of the Run or
	// Stream call, so the application can read its tenant and session from
	// it. turnID names the turn that asks. The application must check that
	// the turn may read ref, and return an error when it may not. The runner
	// checks the length and the SHA-256 of the result, but the application
	// must bound its own read.
	Load(
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

// hydrate returns input with each content reference replaced by verified
// bytes. Input without references is returned as is, with no copy. The
// returned input is a copy, so the recorded, reference-bearing messages stay
// unchanged. A non-nil stop ends the turn as failed: the runner never drops a
// reference or sends the request without it.
func (r *runtime) hydrate(input llm.Input) (llm.Input, *Outcome) {
	if !input.HasRef() {
		return input, nil
	}
	messages := llmclone.Messages(input.Messages)
	var sources []*content.Source
	for _, m := range messages {
		for _, c := range m.Content {
			for _, s := range contentSources(c) {
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
			return StopReasonContentTooLarge, s.Ref.ID
		}
		if maxTotal >= 0 && size > maxTotal-total {
			return StopReasonContentTooLarge, s.Ref.ID
		}
		if size > math.MaxInt64-total {
			return StopReasonContentTooLarge, s.Ref.ID
		}
		total += size
	}
	if r.runner.ContentLoader == nil {
		return StopReasonContentNoLoader, sources[0].Ref.ID
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
	data, err := r.runner.ContentLoader.Load(r.ctx, r.turn.ID, ref)
	if err != nil {
		return nil, StopReasonContentUnavailable
	}
	// Own the bytes: a resolver may reuse its buffer for the next call.
	data = bytes.Clone(data)
	if int64(len(data)) != ref.Size {
		return nil, StopReasonContentSizeMismatch
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal([]byte(hex.EncodeToString(sum[:])), []byte(ref.SHA256)) {
		return nil, StopReasonContentDigestMismatch
	}
	return data, ""
}

func (r *runtime) refStop(id, reason string) *Outcome {
	o := r.stopWith(&Stop{Kind: StopFailed, Reason: reason, RefID: id})
	return &o
}

// contentSources returns pointers to every source in c, including the blocks
// of tool results, hosted results, and hosted uses. They alias c, so the caller passes a
// clone.
func contentSources(c llm.Content) []*content.Source {
	var block, result, hosted []content.Block
	if c.Block != nil {
		block = []content.Block{*c.Block}
	}
	if c.ToolResult != nil {
		result = c.ToolResult.Output.Content
	}
	if c.HostedResult != nil {
		hosted = c.HostedResult.Output
	}
	if c.HostedUse != nil {
		hosted = append(hosted[:len(hosted):len(hosted)], c.HostedUse.Output...)
	}
	return contentsrc.Blocks(block, result, hosted)
}
