package speech

import (
	"bytes"
	"errors"

	"github.com/deepnoodle-ai/dashi/content"
)

type audioFormat struct {
	mimeType  string
	extension string
}

func identifyAudio(data []byte) (audioFormat, error) {
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) &&
		bytes.Equal(data[8:12], []byte("WAVE")) {
		return audioFormat{"audio/wav", "wav"}, nil
	}
	if content.DetectMIMEType(data, "") == "audio/mpeg" ||
		validMPEGFrameHeader(data) {
		return audioFormat{"audio/mpeg", "mp3"}, nil
	}
	return audioFormat{}, errors.New("audio must be identifiable MP3 or WAV")
}

// Go's MIME sniffer recognizes ID3 but not tagless MPEG audio frames. A valid
// first-frame header is enough to identify a candidate; this is not a codec
// decoder or proof that the whole file is playable.
func validMPEGFrameHeader(data []byte) bool {
	if len(data) < 4 || data[0] != 0xff || data[1]&0xe0 != 0xe0 {
		return false
	}
	version := data[1] >> 3 & 0x03
	layer := data[1] >> 1 & 0x03
	bitrate := data[2] >> 4 & 0x0f
	sampleRate := data[2] >> 2 & 0x03
	return version != 0x01 && layer != 0 && bitrate != 0 &&
		bitrate != 0x0f && sampleRate != 0x03
}
