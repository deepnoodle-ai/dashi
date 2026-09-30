package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/content/mention"
	"github.com/deepnoodle-ai/dashi/llm"
)

type inputReceiptState uint8

const (
	inputPreRecordFailure inputReceiptState = iota
	inputPendingUnknown
	inputConfirmedAbsent
	inputRecorded
)

type inputReceipt struct {
	submissionID  string
	draftRevision uint64
	state         inputReceiptState
	ordinaryStart bool
}

// submittedInput owns one frozen request and stable publication IDs. It is
// retained with pending Start, never flattened into a text queue or steer.
type submittedInput struct {
	id            string
	draftRevision uint64
	message       llm.Message
	displayText   string
	snapshots     []inputSnapshot
	unknown       bool
	acknowledged  bool
}
type inputSnapshot struct {
	id, name, mime string
	data           []byte
	ref            content.Ref
	published      bool
}

func (d *attachmentDraft) prepareInput() (*submittedInput, error) {
	if d.frozenBy != "" {
		return nil, fmt.Errorf("attachment submission is pending")
	}
	for _, e := range d.entries {
		if e.state != attachmentReady {
			return nil, fmt.Errorf("attachment %d is not ready", e.number)
		}
	}
	id, err := session.NewAttachmentID()
	if err != nil {
		return nil, err
	}
	d.rescan()
	for _, m := range mention.Scan(d.text) {
		found := false
		for _, e := range d.entries {
			if e.paths[m.Path] {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("mention requires attachment selection")
		}
	}
	in := &submittedInput{id: id, draftRevision: d.revision, displayText: d.outboundText()}
	parts := []llm.Content{llm.Text(in.displayText)}
	for _, e := range d.entries {
		snapshotID, err := session.NewAttachmentID()
		if err != nil {
			return nil, err
		}
		block := e.file.Block.Clone()
		var source *content.Source
		name := filepath.Base(e.canonical)
		if block.Image != nil {
			source = &block.Image.Source
		}
		if block.Document != nil {
			source = &block.Document.Source
			name = block.Document.Title
		}
		if source == nil || len(source.Data) == 0 {
			return nil, fmt.Errorf("attachment %d has no snapshot", e.number)
		}
		digest := sha256.Sum256(source.Data)
		ref := content.Ref{ID: snapshotID, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(source.Data))}
		in.snapshots = append(in.snapshots, inputSnapshot{id: snapshotID, name: name, mime: source.MIMEType, data: source.Data, ref: ref})
		*source = content.Source{MIMEType: source.MIMEType, Ref: &ref}
		parts = append(parts, llm.Block(block))
	}
	in.message = llm.UserMessage(parts...)
	d.frozenBy = id
	return in, nil
}

func (in *submittedInput) publish(ctx context.Context, store *session.AttachmentStore) error {
	if in.acknowledged {
		return fmt.Errorf("attachment submission is already recorded")
	}
	if in.unknown {
		return fmt.Errorf("attachment submission needs reconciliation")
	}
	for i := range in.snapshots {
		s := &in.snapshots[i]
		ref, err := store.Publish(ctx, s.id, s.name, s.mime, s.data)
		if err != nil {
			return err
		}
		if ref != s.ref {
			return fmt.Errorf("attachment publication changed snapshot identity")
		}
		s.published = true
		block := in.message.Content[i+1].Block
		if block.Image != nil {
			block.Image.Source = content.Source{MIMEType: s.mime, Ref: &s.ref}
		}
		if block.Document != nil {
			block.Document.Source = content.Source{MIMEType: s.mime, Ref: &s.ref}
		}
	}
	return nil
}

func (in *submittedInput) messageForStart() (llm.Message, error) {
	if in.acknowledged {
		return llm.Message{}, fmt.Errorf("attachment submission is already recorded")
	}
	if in.unknown {
		return llm.Message{}, fmt.Errorf("attachment submission needs reconciliation")
	}
	for _, s := range in.snapshots {
		if !s.published {
			return llm.Message{}, fmt.Errorf("attachment snapshot has not been published")
		}
	}
	return in.message.Clone(), nil
}

// receipt changes only the matching frozen draft. Summary Starts, Continue,
// and acknowledgement for other revisions cannot consume it.
func (d *attachmentDraft) receipt(in *submittedInput, r inputReceipt) bool {
	if in == nil || r.submissionID != in.id || r.draftRevision != in.draftRevision || d.frozenBy != in.id || d.revision != in.draftRevision || in.acknowledged {
		return false
	}
	switch r.state {
	case inputPendingUnknown:
		in.unknown = true
	case inputConfirmedAbsent:
		in.unknown = false
	case inputPreRecordFailure:
		if in.unknown {
			return false
		}
		d.frozenBy = ""
	case inputRecorded:
		if !r.ordinaryStart {
			return false
		}
		in.unknown = false
		in.acknowledged = true
		d.clear()
	default:
		return false
	}
	return true
}

// retryInput reuses publication and submission identity only while the unchanged
// draft still describes the retained, known-unrecorded request.
func (d *attachmentDraft) retryInput(in *submittedInput) error {
	if in == nil || in.unknown || in.acknowledged || d.revision != in.draftRevision || d.frozenBy != "" {
		return fmt.Errorf("attachment retry does not match the unchanged draft")
	}
	d.frozenBy = in.id
	return nil
}
