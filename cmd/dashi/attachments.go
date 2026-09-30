package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/dashi/content/mention"
)

type attachmentState uint8

const (
	attachmentLoading attachmentState = iota
	attachmentReady
	attachmentFailed
)

type attachmentSpan struct {
	start, end int
	path       string
}
type attachmentEntry struct {
	number      int
	generation  uint64
	paths       map[string]bool
	suffixes    map[string]string
	requested   string
	canonical   string
	suffix      string
	commandOnly bool
	state       attachmentState
	file        mention.File
	err         error
	spans       []attachmentSpan
}
type attachmentTicket struct {
	number     int
	generation uint64
	path       string
}

// attachmentDraft is owned by the UI event loop. Selection work returns a
// ticket; only matching completions can change this draft. No lexical edit,
// recall, rendering, or completion method reads a source file.
type attachmentDraft struct {
	text       string
	revision   uint64
	generation uint64
	nextNumber int
	entries    []*attachmentEntry
	frozenBy   string
}

func (d *attachmentDraft) setText(text string) error {
	if d.frozenBy != "" {
		return fmt.Errorf("attachment submission is pending")
	}
	if d.text != text {
		d.text = text
		d.revision++
	}
	d.rescan()
	for _, e := range append([]*attachmentEntry(nil), d.entries...) {
		if !e.commandOnly && len(e.spans) == 0 {
			d.removeEntry(e.number)
		}
	}
	return nil
}

func (d *attachmentDraft) rescan() {
	for _, e := range d.entries {
		e.spans = nil
	}
	for _, m := range mention.Scan(d.text) {
		for _, e := range d.entries {
			if e.paths[m.Path] {
				e.spans = append(e.spans, attachmentSpan{m.Start, m.End, m.Path})
				break
			}
		}
	}
}

// reserve allocates a stable display number and a stale-completion guard.
// Repeated spelling reuses the existing entry without another selection.
func (d *attachmentDraft) reserve(path string, commandOnly bool) (attachmentTicket, bool, error) {
	if d.frozenBy != "" {
		return attachmentTicket{}, false, fmt.Errorf("attachment submission is pending")
	}
	for _, e := range d.entries {
		if e.paths[path] {
			if commandOnly && !e.commandOnly {
				e.commandOnly = true
				d.revision++
			}
			if e.state == attachmentFailed {
				d.generation++
				d.revision++
				e.generation = d.generation
				e.state = attachmentLoading
				e.err = nil
				return attachmentTicket{e.number, e.generation, path}, true, nil
			}
			return attachmentTicket{e.number, e.generation, path}, false, nil
		}
	}
	d.nextNumber++
	d.generation++
	d.revision++
	e := &attachmentEntry{number: d.nextNumber, generation: d.generation, paths: map[string]bool{path: true}, suffixes: map[string]string{}, requested: path, commandOnly: commandOnly, state: attachmentLoading}
	d.entries = append(d.entries, e)
	d.rescan()
	return attachmentTicket{e.number, e.generation, path}, true, nil
}

func (d *attachmentDraft) entry(t attachmentTicket) *attachmentEntry {
	for _, e := range d.entries {
		if e.number == t.number && e.generation == t.generation {
			return e
		}
	}
	return nil
}

// reuseCanonical is called after path resolution and before opening a new
// descriptor. A ready canonical duplicate transfers its lexical association
// to the first snapshot, without reading the source again.
func (d *attachmentDraft) reuseCanonical(t attachmentTicket, canonical, suffix string) bool {
	e := d.entry(t)
	if e == nil {
		return true
	} // stale work must not perform a read
	for _, old := range d.entries {
		if old != e && old.state == attachmentReady && old.canonical == canonical {
			for path := range e.paths {
				old.paths[path] = true
				old.suffixes[path] = suffix
			}
			old.commandOnly = old.commandOnly || e.commandOnly
			d.removeEntry(e.number)
			d.revision++
			d.rescan()
			return true
		}
	}
	return false
}

func (d *attachmentDraft) complete(t attachmentTicket, file mention.File, err error) bool {
	e := d.entry(t)
	if e == nil || d.frozenBy != "" {
		return false
	}
	if err == nil {
		var size int64
		var mime string
		if file.Block.Image != nil {
			size = int64(len(file.Block.Image.Source.Data))
			mime = file.Block.Image.Source.MIMEType
		}
		if file.Block.Document != nil {
			size = int64(len(file.Block.Document.Source.Data))
			mime = file.Block.Document.Source.MIMEType
		}
		if size <= 0 || size != file.Attachment.Size || mime != file.Attachment.MIMEType || file.CanonicalPath == "" {
			err = fmt.Errorf("attachment selection returned an invalid snapshot")
		}
	}
	if err == nil {
		wire, wireErr := mention.WireSize(file.Attachment.Size)
		if wireErr == nil {
			for _, other := range d.entries {
				if other != e && other.state == attachmentReady {
					size, _ := mention.WireSize(other.file.Attachment.Size)
					if size > 32<<20-wire {
						wireErr = fmt.Errorf("attachments exceed the 32 MiB message limit")
						break
					}
					wire += size
				}
			}
			if wire > 32<<20 {
				wireErr = fmt.Errorf("attachments exceed the 32 MiB message limit")
			}
		}
		err = wireErr
	}
	e.err = err
	if err != nil {
		e.state = attachmentFailed
	} else {
		e.state = attachmentReady
		e.file = file
		e.file.Block = file.Block.Clone()
		e.canonical = file.CanonicalPath
		e.suffix = file.LineSuffix
		e.suffixes[t.path] = file.LineSuffix
	}
	d.revision++
	return true
}

func (d *attachmentDraft) removeEntry(number int) {
	for i, e := range d.entries {
		if e.number == number {
			d.entries = append(d.entries[:i], d.entries[i+1:]...)
			return
		}
	}
}

// detach uses current lexical spans, never old offsets or string replacement.
func (d *attachmentDraft) detach(number int) error {
	if d.frozenBy != "" {
		return fmt.Errorf("attachment submission is pending")
	}
	d.rescan()
	for _, e := range d.entries {
		if e.number != number {
			continue
		}
		sort.Slice(e.spans, func(i, j int) bool { return e.spans[i].start > e.spans[j].start })
		for _, s := range e.spans {
			d.text = d.text[:s.start] + d.text[s.end:]
		}
		d.removeEntry(number)
		d.revision++
		d.rescan()
		return nil
	}
	return fmt.Errorf("attachment %d does not exist", number)
}

func (d *attachmentDraft) unsaved() bool {
	return d.text != "" || len(d.entries) > 0 || d.frozenBy != ""
}
func (d *attachmentDraft) clear() {
	d.text = ""
	d.entries = nil
	d.frozenBy = ""
	d.revision++
	d.generation++
	d.nextNumber = 0
}

// outboundText uses bounded recorded labels. Source paths remain draft-only.
func (d *attachmentDraft) outboundText() string {
	type replacement struct {
		attachmentSpan
		label string
	}
	var replacements []replacement
	for _, e := range d.entries {
		if e.state != attachmentReady {
			continue
		}
		for _, s := range e.spans {
			replacements = append(replacements, replacement{s, attachmentLabel(e, e.suffixes[s.path])})
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	text := d.text
	for _, r := range replacements {
		text = text[:r.start] + r.label + text[r.end:]
	}
	for _, e := range d.entries {
		if e.commandOnly && len(e.spans) == 0 {
			text += "\n" + attachmentLabel(e, e.suffix)
		}
	}
	return text
}

func attachmentLabel(e *attachmentEntry, suffix string) string {
	name := e.file.Block.Document
	basename := filepath.Base(e.canonical)
	if name != nil {
		basename = name.Title
	}
	// Terminal controls and delimiters cannot become recorded label syntax.
	basename = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '[' || r == ']' {
			return '_'
		}
		return r
	}, basename)
	if len(basename) > 256 {
		basename = string([]rune(basename)[:min(64, len([]rune(basename)))])
	}
	return fmt.Sprintf("[file: %s; %s; %d bytes]%s", basename, e.file.Attachment.MIMEType, e.file.Attachment.Size, suffix)
}
