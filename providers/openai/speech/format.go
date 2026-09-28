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

// hasCompleteMP3Frame checks that a synthesis response contains at least one
// complete Layer III frame after an optional ID3v2 tag. It does not decode the
// audio or claim that later frames are valid.
func hasCompleteMP3Frame(data []byte) bool {
	if len(data) >= 3 && bytes.Equal(data[:3], []byte("ID3")) {
		if len(data) < 10 {
			return false
		}
		size := 0
		for _, b := range data[6:10] {
			if b&0x80 != 0 {
				return false
			}
			size = size<<7 | int(b)
		}
		offset := 10 + size
		if data[5]&0x10 != 0 { // ID3v2 footer, when present.
			offset += 10
		}
		if offset > len(data) {
			return false
		}
		data = data[offset:]
	}
	if !validMPEGFrameHeader(data) || data[1]>>1&0x03 != 1 {
		return false
	}
	version := data[1] >> 3 & 0x03
	bitrateIndex := data[2] >> 4 & 0x0f
	sampleIndex := data[2] >> 2 & 0x03
	var bitrates []int
	var sampleRates []int
	coefficient := 72
	switch version {
	case 3: // MPEG-1 Layer III
		bitrates = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
		sampleRates = []int{44100, 48000, 32000}
		coefficient = 144
	case 2: // MPEG-2 Layer III
		bitrates = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
		sampleRates = []int{22050, 24000, 16000}
	case 0: // MPEG-2.5 Layer III
		bitrates = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
		sampleRates = []int{11025, 12000, 8000}
	default:
		return false
	}
	frameBytes := coefficient*bitrates[bitrateIndex]*1000/sampleRates[sampleIndex] +
		int(data[2]>>1&1)
	return frameBytes > 4 && len(data) >= frameBytes
}
