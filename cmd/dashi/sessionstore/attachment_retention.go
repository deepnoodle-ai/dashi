package sessionstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var errAttachmentUnsupported = errors.New("session: secure attachments are unavailable on this platform")

// Unsupported storage can still establish absence. Check every ancestor
// without following links; existing directories and paths in an unknown
// state stay.
func attachmentPathAbsent(path string) bool {
	full, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	volume := filepath.VolumeName(full)
	current := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(full, current), string(filepath.Separator))
	for _, part := range append([]string{""}, parts...) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return false
}
