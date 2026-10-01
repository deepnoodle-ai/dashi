package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content/mention"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type attachmentUI interface {
	AttachmentCommand(string, string)
	AttachmentInput(string) bool
	AttachmentReceipt(*submittedInput, inputReceipt)
	AttachmentUnsaved() bool
}

func attachmentPaths(text string) []string {
	var paths []string
	for _, m := range mention.Scan(text) {
		paths = append(paths, m.Path)
	}
	return paths
}

func printAttachmentInput(ctx context.Context, dir, text string, flags []string) (*submittedInput, error) {
	d := &attachmentDraft{}
	d.setText(text)
	for _, group := range []struct {
		paths   []string
		command bool
	}{{flags, true}, {attachmentPaths(text), false}} {
		for _, path := range group.paths {
			ticket, fresh, err := d.reserve(path, group.command)
			if err != nil {
				return nil, err
			}
			if !fresh {
				continue
			}
			canonical, suffix, err := mention.ResolvePath(path, mention.Options{Root: dir})
			if err == nil && d.reuseCanonical(ticket, canonical, suffix) {
				continue
			}
			var file mention.File
			if err == nil {
				file, err = mention.ReadFileContext(ctx, path, mention.Options{Root: dir})
			}
			d.complete(ticket, file, err)
			if err != nil {
				return nil, fmt.Errorf("attachment %d could not be selected: %w", ticket.number, attachmentSelectionError(path, err))
			}
		}
	}
	return d.prepareInput()
}
func selectedArgument(arg string) string {
	arg = strings.TrimSpace(arg)
	// Like mention paths, wholly quoted paths preserve their contents and
	// unquoted paths decode backslash escapes. The command has one path
	// argument, so literal spaces do not split it into multiple selections.
	if len(arg) > 1 && (arg[0] == '\'' || arg[0] == '"') && arg[len(arg)-1] == arg[0] {
		return arg[1 : len(arg)-1]
	}
	var path strings.Builder
	for i := 0; i < len(arg); i++ {
		if arg[i] == '\\' && i+1 < len(arg) {
			i++
		}
		path.WriteByte(arg[i])
	}
	return path.String()
}
func attachmentWorkspace(dir string) string {
	canonical, err := filepath.EvalSymlinks(dir)
	if err == nil {
		return canonical
	}
	return dir
}

// Error labels contain printable, bounded filenames, never terminal controls.
func attachmentSelectionError(path string, err error) error {
	name := []rune(workspace.PrintableText(filepath.Base(path)))
	if len(name) > 128 {
		name = append(name[:127], '…')
	}
	return fmt.Errorf("%s: %w", string(name), err)
}

// The footer uses single-line names; /attachments retains full source details.
func attachmentOutsideWorkspace(e *attachmentEntry, dir string) bool {
	if e.state != attachmentReady {
		return false
	}
	rel, err := filepath.Rel(dir, e.canonical)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func attachmentPreviewRows(d *attachmentDraft, dir string) []string {
	var rows []string
	for _, e := range d.entries[:min(2, len(d.entries))] {
		state := "loading"
		detail := ""
		if e.state == attachmentFailed {
			state = "error"
		} else if e.state == attachmentReady {
			state = "ready"
			detail = fmt.Sprintf(" · %s · %d bytes", e.file.Attachment.MIMEType, e.file.Attachment.Size)
		}
		location := ""
		if attachmentOutsideWorkspace(e, dir) {
			// Place the disclosure before a potentially truncated filename.
			location = " · outside workspace ·"
		}
		rows = append(rows, workspace.PrintableText(fmt.Sprintf("%d. %s%s %s%s", e.number, state, location, filepath.Base(e.requested), detail)))
	}
	if len(d.entries) > 0 {
		more := ""
		if len(d.entries) > 2 {
			more = fmt.Sprintf("%d more selections · ", len(d.entries)-2)
		}
		outside := 0
		for _, e := range d.entries {
			if attachmentOutsideWorkspace(e, dir) {
				outside++
			}
		}
		if outside > 0 {
			rows = append(rows, more+fmt.Sprintf("%d outside workspace · /attachments · /detach N", outside))
		} else {
			rows = append(rows, more+"/attachments lists all · /detach N removes a selection")
		}
	}
	return rows
}

func attachmentRows(d *attachmentDraft, dir string) []string {
	var rows []string
	for _, e := range d.entries {
		state := "loading"
		detail := ""
		if e.state == attachmentFailed {
			state = "error"
			detail = fmt.Sprintf(" — %v; correct the selection or /detach %d", attachmentSelectionError(e.requested, e.err), e.number)
		}
		if e.state == attachmentReady {
			state = "ready"
			detail = fmt.Sprintf(" — %s, %d bytes", e.file.Attachment.MIMEType, e.file.Attachment.Size)
			if attachmentOutsideWorkspace(e, dir) {
				detail += "; outside workspace"
			}
		}
		path := e.requested
		if e.canonical != "" {
			path = e.canonical
		}
		rows = append(rows, workspace.PrintableText(fmt.Sprintf("%d. %s %s%s", e.number, state, path, detail)))
	}
	if len(rows) > 0 {
		rows = append(rows, "/detach N removes a selection. Enter sends when all selections are ready.")
	}
	return rows
}
func prepareDraftSubmission(d *attachmentDraft, retained *submittedInput) (*submittedInput, error) {
	if retained != nil && !retained.acknowledged && retained.draftRevision == d.revision && !retained.unknown {
		if d.frozenBy == "" {
			if err := d.retryInput(retained); err != nil {
				return nil, err
			}
		}
		return retained, nil
	}
	return d.prepareInput()
}

type attachmentEvent struct {
	at                time.Time
	command, arg      string
	ticket            attachmentTicket
	canonical, suffix string
	resolved          chan bool
	file              mention.File
	err               error
	input             *submittedInput
	receipt           inputReceipt
}

func (e attachmentEvent) Timestamp() time.Time { return e.at }

func (s *screen) AttachmentCommand(name, arg string) {
	s.post(attachmentEvent{at: time.Now(), command: name, arg: arg})
}
func (s *screen) AttachmentReceipt(in *submittedInput, r inputReceipt) {
	s.post(attachmentEvent{at: time.Now(), input: in, receipt: r})
}

// Controller invokes this only on its event-loop submission path.
func (s *screen) AttachmentUnsaved() bool { return s.attachmentUnsaved.Load() }
func (s *screen) updateAttachmentUnsaved() {
	s.attachmentUnsaved.Store(len(s.attachments.entries) > 0 || s.attachments.frozenBy != "")
}
func (s *screen) attachmentEvent(e attachmentEvent) {
	defer s.updateAttachmentUnsaved()
	if e.command != "" {
		s.attachmentCommand(e.command, e.arg)
		return
	}
	if e.resolved != nil {
		e.resolved <- s.attachments.reuseCanonical(e.ticket, e.canonical, e.suffix)
		return
	}
	if e.input != nil {
		if s.attachments.receipt(e.input, e.receipt) && e.receipt.state == inputRecorded {
			if s.promptActive {
				s.savedDraft = ""
			} else {
				s.input = ""
			}
			s.attachmentInput = nil
		}
		return
	}
	s.attachments.complete(e.ticket, e.file, e.err)
}
func (s *screen) attachmentCommand(name, arg string) {
	defer s.updateAttachmentUnsaved()
	if name == "/attachments" {
		rows := attachmentRows(&s.attachments, s.ctrl.attachmentRoot)
		if len(rows) == 0 {
			s.ctrl.notice("No selected attachments.")
		} else {
			s.ctrl.notice("%s", strings.Join(rows, "\n"))
		}
		return
	}
	if s.ctrl.Mode() != ModeIdle || s.attachments.frozenBy != "" {
		s.Emit(warnBlock("Attachment changes need a fresh idle submission; the draft was kept."))
		return
	}
	switch name {
	case "/attach":
		path := selectedArgument(arg)
		if path == "" {
			s.Emit(warnBlock("Usage: /attach PATH"))
			return
		}
		s.stageAttachments([]string{path}, true)
	case "/detach":
		n, err := strconv.Atoi(arg)
		if err == nil {
			err = s.attachments.detach(n)
		}
		if err != nil {
			s.Emit(warnBlock("/detach needs an existing attachment number: %v", err))
		} else {
			s.input = s.attachments.text
		}
	}
}
func (s *screen) stageAttachments(paths []string, command bool) bool {
	var tickets []attachmentTicket
	for _, path := range paths {
		ticket, fresh, err := s.attachments.reserve(path, command)
		if err != nil {
			s.Emit(warnBlock("%v", err))
			continue
		}
		if fresh {
			tickets = append(tickets, ticket)
		}
	}
	if len(tickets) == 0 {
		return false
	}
	if s.attachmentCancel == nil {
		s.attachmentContext, s.attachmentCancel = context.WithCancel(s.ctrl.base)
	}
	ctx := s.attachmentContext
	s.ctrl.goWork(func() {
		s.attachmentSelectionMu.Lock()
		defer s.attachmentSelectionMu.Unlock()
		for _, ticket := range tickets {
			if ctx.Err() != nil {
				return
			}
			canonical, suffix, err := mention.ResolvePath(ticket.path, mention.Options{Root: s.dir})
			if err == nil {
				reply := make(chan bool, 1)
				s.post(attachmentEvent{at: time.Now(), ticket: ticket, canonical: canonical, suffix: suffix, resolved: reply})
				select {
				case reused := <-reply:
					if reused {
						continue
					}
				case <-ctx.Done():
					return
				}
			}
			var file mention.File
			if err == nil {
				file, err = mention.ReadFileContext(ctx, ticket.path, mention.Options{Root: s.dir})
			}
			s.post(attachmentEvent{at: time.Now(), ticket: ticket, file: file, err: err})
		}
	})
	return true
}
func (s *screen) AttachmentInput(raw string) bool {
	defer s.updateAttachmentUnsaved()
	if len(s.attachments.entries) == 0 && len(mention.Scan(raw)) == 0 {
		return false
	}
	if raw == "" && s.attachments.text != "" {
		raw = s.attachments.text
	}
	s.input = raw
	if s.ctrl.Mode() != ModeIdle {
		s.Emit(warnBlock("Attachments need a fresh idle submission; the draft was kept."))
		return true
	}
	if s.attachments.frozenBy != "" {
		if s.eng.HasPending() {
			s.ctrl.attachmentResume()
		} else {
			s.Emit(warnBlock("Attachment submission is pending; the draft was kept."))
		}
		return true
	}
	if err := s.attachments.setText(raw); err != nil {
		s.Emit(warnBlock("%v", err))
		return true
	}
	if s.stageAttachments(attachmentPaths(raw), false) {
		s.Emit(Block{Kind: KindNotice, Text: "Review selected attachments, then press Enter again to send.", Done: true})
		return true
	}
	in, err := prepareDraftSubmission(&s.attachments, s.attachmentInput)
	if err != nil {
		s.Emit(warnBlock("%v; use /detach N to remove failed selections.", err))
		return true
	}
	s.attachmentInput = in
	s.recordSubmittedInput(raw)
	s.ctrl.attachmentPrompt(in)
	return true
}
func (s *screen) cancelAttachmentSelections() {
	if s.attachmentCancel != nil {
		s.attachmentCancel()
	}
}

func (u *lineUI) AttachmentUnsaved() bool {
	u.attachmentMu.Lock()
	defer u.attachmentMu.Unlock()
	u.drainAttachmentReceiptsLocked()
	return u.attachments.unsaved()
}
func (u *lineUI) AttachmentReceipt(in *submittedInput, r inputReceipt) {
	u.receiptMu.Lock()
	u.receiptQueue = append(u.receiptQueue, attachmentEvent{input: in, receipt: r})
	if !u.receiptsRunning {
		u.receiptsRunning = true
		go u.applyAttachmentReceipts()
	}
	u.receiptMu.Unlock()
}
func (u *lineUI) applyAttachmentReceipts() {
	for {
		u.attachmentMu.Lock()
		u.receiptMu.Lock()
		items := u.receiptQueue
		u.receiptQueue = nil
		if len(items) == 0 {
			u.receiptsRunning = false
			u.receiptMu.Unlock()
			u.attachmentMu.Unlock()
			return
		}
		u.receiptMu.Unlock()
		for _, e := range items {
			if u.attachments.receipt(e.input, e.receipt) && e.receipt.state == inputRecorded {
				u.attachmentInput = nil
			}
		}
		u.attachmentMu.Unlock()
	}
}
func (u *lineUI) drainAttachmentReceiptsLocked() {
	u.receiptMu.Lock()
	items := u.receiptQueue
	u.receiptQueue = nil
	u.receiptMu.Unlock()
	for _, e := range items {
		if u.attachments.receipt(e.input, e.receipt) && e.receipt.state == inputRecorded {
			u.attachmentInput = nil
		}
	}
}
func (u *lineUI) attachmentPreview() {
	rows := attachmentRows(&u.attachments, u.ctrl.attachmentRoot)
	for i, row := range rows {
		if i < len(u.attachments.entries) && u.attachments.entries[i].state == attachmentFailed {
			u.AttachmentWarning(row)
		} else {
			u.ctrl.notice("%s", row)
		}
	}
}
func (u *lineUI) selectAttachments(paths []string, command bool) bool {
	freshAny := false
	for _, path := range paths {
		ticket, fresh, err := u.attachments.reserve(path, command)
		if err != nil {
			u.AttachmentWarning(fmt.Sprint(err))
			continue
		}
		if !fresh {
			continue
		}
		freshAny = true
		canonical, suffix, err := mention.ResolvePath(path, mention.Options{Root: u.ctrl.dir})
		if err == nil && u.attachments.reuseCanonical(ticket, canonical, suffix) {
			continue
		}
		var file mention.File
		if err == nil {
			file, err = mention.ReadFileContext(u.ctrl.base, path, mention.Options{Root: u.ctrl.dir})
		}
		u.attachments.complete(ticket, file, err)
	}
	return freshAny
}
func (u *lineUI) AttachmentCommand(name, arg string) {
	u.attachmentMu.Lock()
	defer u.attachmentMu.Unlock()
	u.drainAttachmentReceiptsLocked()
	if name == "/attachments" {
		u.attachmentPreview()
		return
	}
	if u.ctrl.Mode() != ModeIdle || u.attachments.frozenBy != "" {
		u.AttachmentWarning("Attachment changes need a fresh idle submission; the draft was kept.")
		return
	}
	if name == "/attach" {
		if path := selectedArgument(arg); path != "" {
			u.selectAttachments([]string{path}, true)
		} else {
			u.AttachmentWarning("Usage: /attach PATH")
		}
	}
	if name == "/detach" {
		n, err := strconv.Atoi(arg)
		if err == nil {
			err = u.attachments.detach(n)
		}
		if err != nil {
			u.AttachmentWarning(fmt.Sprintf("/detach needs an existing attachment number: %v", err))
		}
	}
	u.attachmentPreview()
}
func (u *lineUI) attachmentLineInput(raw string) bool { return u.AttachmentInput(raw) }

func (u *lineUI) AttachmentInput(raw string) bool {
	u.attachmentMu.Lock()
	defer u.attachmentMu.Unlock()
	u.drainAttachmentReceiptsLocked()
	if len(u.attachments.entries) == 0 && len(mention.Scan(raw)) == 0 && !(raw == "" && u.attachments.text != "") && u.attachments.frozenBy == "" {
		return false
	}
	if u.attachments.frozenBy != "" {
		if u.ctrl.eng.HasPending() {
			u.ctrl.attachmentResume()
		} else {
			u.ctrl.notice("Attachment submission is pending.")
		}
		return true
	}
	// A blank line confirms the displayed line-mode draft.
	if raw != "" {
		u.attachments.setText(raw)
	}
	if u.selectAttachments(attachmentPaths(u.attachments.text), false) {
		u.attachmentPreview()
		u.ctrl.notice("Review the selections; submit the same prompt or a blank line to send.")
		return true
	}
	in, err := prepareDraftSubmission(&u.attachments, u.attachmentInput)
	if err != nil {
		u.AttachmentWarning(fmt.Sprintf("%v; /detach N removes failed selections.", err))
		return true
	}
	u.attachmentInput = in
	u.ctrl.attachmentPrompt(in)
	return true
}

type attachmentFailure struct{ err error }

func (e *attachmentFailure) Error() string { return e.err.Error() }
func (e *attachmentFailure) Unwrap() error { return e.err }
func (c *Controller) attachmentPrompt(in *submittedInput) {
	c.startRun(func(ctx context.Context) (Result, error) {
		res, err := c.eng.PromptInput(ctx, in)
		if err != nil {
			err = &attachmentFailure{err}
		}
		return res, err
	})
}
func (s *screen) AttachmentWarning(text string) { s.Emit(warnBlock("%s", text)) }
func (u *lineUI) AttachmentWarning(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	writer := u.attachmentErrors
	if writer == nil {
		writer = u.out
	}
	fmt.Fprintln(writer, workspace.PrintableText(text))
}

func (c *Controller) attachmentResume() {
	c.startRun(func(ctx context.Context) (Result, error) {
		res, err := c.eng.Resume(ctx)
		if err != nil {
			err = &attachmentFailure{err}
		}
		return res, err
	})
}

func (c *Controller) queuedPrompt(text string) {
	if len(attachmentPaths(text)) > 0 {
		c.notice("Attachments need a fresh idle submission; /queue cannot select or send files.")
		return
	}
	c.prompt(text)
}
