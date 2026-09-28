package content

import (
	"testing"
	"time"
)

func TestSourceCloneCopiesBytesAndExpiry(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	inline := Source{
		Data:     []byte{1},
		MIMEType: "audio/wav",
	}
	inlineClone := inline.Clone()
	inlineClone.Data[0] = 9
	if inline.Data[0] != 1 {
		t.Fatal("source bytes were shared")
	}
	remote := Source{
		URL:       "https://example.test/audio",
		ExpiresAt: &expires,
	}
	remoteClone := remote.Clone()
	*remoteClone.ExpiresAt = expires.Add(time.Hour)
	if !remote.ExpiresAt.Equal(expires) {
		t.Fatal("source expiry was shared")
	}
}

func TestSourceValidateRejectsInlineExpiry(t *testing.T) {
	source := Source{
		Data:      []byte{1},
		MIMEType:  "audio/wav",
		ExpiresAt: new(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
	}
	if err := source.Validate(); err == nil {
		t.Fatal("accepted an expiration on inline bytes")
	}
}

func TestVideoBlockCloneCopiesSourceBytes(t *testing.T) {
	block := VideoBlock(Source{
		Data:     []byte{1, 2, 3},
		MIMEType: "video/mp4",
	})
	if err := block.Validate(); err != nil {
		t.Fatalf("valid video block rejected: %v", err)
	}
	clone := block.Clone()
	clone.Video.Source.Data[0] = 9
	if block.Video.Source.Data[0] != 1 {
		t.Fatal("video source bytes were shared")
	}
}

func TestVideoBlockRejectsNegativeOutputDuration(t *testing.T) {
	block := VideoBlock(Source{Data: []byte{1}, MIMEType: "video/mp4"})
	block.Video.Duration = -time.Second
	if err := block.Validate(); err == nil {
		t.Fatal("accepted negative output duration")
	}
}

func TestAudioBlockCloneCopiesFrameFormat(t *testing.T) {
	block := Block{
		Kind: KindAudio,
		Audio: &Audio{
			Source: Source{Data: []byte{1}, MIMEType: "audio/pcm"},
			Format: &AudioFormat{
				Encoding:     "pcm_s16le",
				SampleRateHz: 16000,
				Channels:     1,
			},
		},
	}
	if err := block.Validate(); err != nil {
		t.Fatalf("valid audio block rejected: %v", err)
	}
	clone := block.Clone()
	clone.Audio.Format.SampleRateHz = 24000
	if block.Audio.Format.SampleRateHz != 16000 {
		t.Fatal("audio frame format was shared")
	}
}
