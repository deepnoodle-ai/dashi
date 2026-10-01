//go:build !darwin && !linux

package session

import (
	"fmt"
	"os"
)

type attachmentDir struct {
	file *os.File
	root *os.Root
}

func openAttachmentAnchor(string) (*attachmentDir, error) {
	return nil, errAttachmentUnsupported
}
func (d *attachmentDir) close()                               {}
func (d *attachmentDir) child(string) (*attachmentDir, error) { return openAttachmentAnchor("") }
func (d *attachmentDir) regular(string, bool) (*os.File, error) {
	return nil, errAttachmentUnsupported
}
func (d *attachmentDir) publishUnused(string, string) error {
	return fmt.Errorf("session: secure attachments are unavailable on this platform")
}
