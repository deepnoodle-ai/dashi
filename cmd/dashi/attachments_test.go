package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/content/mention"
)

func draftReady(t *testing.T, d *attachmentDraft, path string, body []byte) attachmentTicket {
	t.Helper()
	ticket, _, err := d.reserve(path, true)
	if err != nil {
		t.Fatal(err)
	}
	f := mention.File{CanonicalPath: path, Attachment: mention.Attachment{Path: path, MIMEType: "text/plain", Size: int64(len(body))}, Block: content.DocumentBlock(content.Source{Data: body, MIMEType: "text/plain"}, filepath.Base(path))}
	if !d.complete(ticket, f, nil) {
		t.Fatal("completion ignored")
	}
	return ticket
}

func TestAttachmentDetachCurrentUnicodeQuotedFailedSpans(t *testing.T) {
	d := &attachmentDraft{}
	d.setText(`雪 (@"missing path":12-15), keep @ok.go:3! @"missing path":12-15.`)
	bad, _, _ := d.reserve("missing path:12-15", false)
	d.complete(bad, mention.File{}, errors.New("missing"))
	good, _, _ := d.reserve("ok.go:3", false)
	d.complete(good, mention.File{CanonicalPath: "/ok.go", LineSuffix: ":3", Attachment: mention.Attachment{Size: 1, MIMEType: "text/plain"}, Block: content.DocumentBlock(content.Source{Data: []byte("x"), MIMEType: "text/plain"}, "ok.go")}, nil)
	d.setText("prefix " + d.text)
	if err := d.detach(bad.number); err != nil {
		t.Fatal(err)
	}
	if want := "prefix 雪 (), keep @ok.go:3! ."; d.text != want {
		t.Fatalf("%q != %q", d.text, want)
	}
	if len(d.entries) != 1 || d.entries[0].number != good.number {
		t.Fatal("numbers changed")
	}
	if d.complete(bad, mention.File{}, nil) {
		t.Fatal("detached completion revived")
	}
	if err := d.detach(good.number); err != nil {
		t.Fatal(err)
	}
	if d.text != "prefix 雪 (), keep ! ." {
		t.Fatal(d.text)
	}
}

func TestAttachmentCanonicalReuseAndEditsDoNotRead(t *testing.T) {
	d := &attachmentDraft{}
	d.setText("compare @file.txt:12 @link.txt:14")
	one, _, _ := d.reserve("file.txt:12", false)
	f := mention.File{CanonicalPath: "/resolved/file.txt", LineSuffix: ":12", Attachment: mention.Attachment{Size: 3, MIMEType: "text/plain"}, Block: content.DocumentBlock(content.Source{Data: []byte("old"), MIMEType: "text/plain"}, "file.txt")}
	d.complete(one, f, nil)
	two, _, _ := d.reserve("link.txt:14", false)
	if !d.reuseCanonical(two, "/resolved/file.txt", ":14") || len(d.entries) != 1 {
		t.Fatal("duplicate not reused")
	}
	if out := d.outboundText(); !strings.Contains(out, "]:\u0031\u0032") || !strings.Contains(out, "]:14") {
		t.Fatal(out)
	}
	d.setText("ordinary text")
	if len(d.entries) != 0 {
		t.Fatal("removed mentions kept snapshot authority")
	}
	if d.complete(one, f, nil) {
		t.Fatal("edited-out entry revived")
	}
	command := draftReady(t, d, "/does/not/exist", []byte("frozen"))
	before := d.text
	if err := d.detach(command.number); err != nil || d.text != before {
		t.Fatalf("%q %v", d.text, err)
	}
}

func TestAttachmentGenerationAndAggregateProtection(t *testing.T) {
	d := &attachmentDraft{}
	old, _, _ := d.reserve("first", true)
	d.clear()
	fresh, _, _ := d.reserve("second", true)
	if old.number != fresh.number || old.generation == fresh.generation {
		t.Fatal("fixture lacks same-number stale generation")
	}
	if d.complete(old, mention.File{}, nil) {
		t.Fatal("old draft completion applied")
	}
	for i := 0; i < 4; i++ {
		ticket, _, _ := d.reserve(string(rune('a'+i)), true)
		f := mention.File{CanonicalPath: string(rune('a' + i)), Attachment: mention.Attachment{Size: 10 << 20, MIMEType: "application/pdf"}, Block: content.DocumentBlock(content.Source{Data: make([]byte, 10<<20), MIMEType: "application/pdf"}, "a.pdf")}
		d.complete(ticket, f, nil)
	}
	ready := 0
	failed := 0
	for _, e := range d.entries {
		if e.state == attachmentReady {
			ready++
		}
		if e.state == attachmentFailed {
			failed++
		}
	}
	if ready != 2 || failed != 2 {
		t.Fatalf("ready=%d failed=%d", ready, failed)
	}
	if _, err := d.prepareInput(); err == nil {
		t.Fatal("loading/error entries admitted")
	}
}

func TestAttachmentInputFreezeIdentityAndReceipt(t *testing.T) {
	d := &attachmentDraft{}
	d.setText("explain")
	draftReady(t, d, "/selected.txt", []byte("evidence"))
	in, err := d.prepareInput()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.messageForStart(); err == nil {
		t.Fatal("unpublished input admitted")
	}
	if err := d.setText("new"); err == nil {
		t.Fatal("frozen draft edited")
	}
	r := inputReceipt{in.id, in.draftRevision, inputPendingUnknown, true}
	if !d.receipt(in, r) || !in.unknown {
		t.Fatal("unknown not retained")
	}
	r.state = inputPreRecordFailure
	if d.receipt(in, r) {
		t.Fatal("unknown unblocked by generic failure")
	}
	r.state = inputRecorded
	r.ordinaryStart = false
	if d.receipt(in, r) || !d.unsaved() {
		t.Fatal("summary consumed draft")
	}
	r.state = inputConfirmedAbsent
	r.ordinaryStart = true
	if !d.receipt(in, r) || in.unknown {
		t.Fatal("absence not reconciled")
	}
	r.state = inputPreRecordFailure
	if !d.receipt(in, r) || d.frozenBy != "" {
		t.Fatal("known failure did not unfreeze")
	}
	id := in.snapshots[0].id
	if err := d.retryInput(in); err != nil || in.snapshots[0].id != id {
		t.Fatal("retry identity changed")
	}
	r.state = inputRecorded
	r.submissionID = "other"
	if d.receipt(in, r) {
		t.Fatal("other input consumed")
	}
	r.submissionID = in.id
	if !d.receipt(in, r) || d.unsaved() {
		t.Fatal("ordinary Start did not consume")
	}
	d.setText("independent draft")
	if d.receipt(in, r) || d.text != "independent draft" {
		t.Fatal("duplicate receipt consumed new draft")
	}
	if _, err := in.messageForStart(); err == nil {
		t.Fatal("recorded input replayable")
	}
}

func TestAttachmentUnknownMentionRequiresSelection(t *testing.T) {
	d := &attachmentDraft{}
	d.setText("look @new.png")
	if _, err := d.prepareInput(); err == nil {
		t.Fatal("unstaged mention admitted")
	}
}

func TestAttachmentOwnedPublicationRealisticMedia(t *testing.T) {
	if attachmentPlatformUnsupported() {
		t.Skip("secure snapshots require macOS/Linux")
	}
	st := sessionstore.NewStore(filepath.Join(t.TempDir(), "store"))
	s, err := st.Create(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock(s)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	store, err := st.Attachments(s)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	img := image.NewRGBA(image.Rect(0, 0, 640, 512))
	rng := rand.New(rand.NewSource(7))
	rng.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	body := encoded.Bytes()
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := mention.ReadFile(path, mention.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := &attachmentDraft{}
	d.setText("explain")
	ticket, _, _ := d.reserve(path, true)
	d.complete(ticket, file, nil)
	in, err := d.prepareInput()
	if err != nil {
		t.Fatal(err)
	}
	file.Block.Image.Source.Data[0] = 'X' // selection and submission own their copies
	if err := in.publish(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	message, err := in.messageForStart()
	if err != nil {
		t.Fatal(err)
	}
	source := message.Content[1].Block.Image.Source
	if len(source.Data) != 0 || source.Ref == nil || source.Ref.Size != int64(len(body)) {
		t.Fatalf("%+v", source)
	}
	got, err := store.Resolve(context.Background(), s.ID+"-2", *source.Ref)
	if err != nil || len(got) != len(body) || got[0] != 0x89 {
		t.Fatalf("len=%d %v", len(got), err)
	}
	ref := *source.Ref
	source.Ref.ID = "modified"
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := in.publish(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	message2, err := in.messageForStart()
	if err != nil || *message2.Content[1].Block.Image.Source.Ref != ref {
		t.Fatal("retry changed reference")
	}
}
