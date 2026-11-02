package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/mp4/mp4test"
)

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		file mp4test.File
		want Info
	}{
		{
			"720p silent",
			mp4test.File{
				MovieTimescale: 1000,
				Tracks: []mp4test.Track{{
					Handler: "vide", Timescale: 24, Duration: 96,
					Width: 1280, Height: 720,
				}},
			},
			Info{Duration: 4 * time.Second, Width: 1280, Height: 720},
		},
		{
			"1080p with audio",
			mp4test.VideoAudio(8, 0, 1920, 1080),
			Info{Duration: 8 * time.Second, Width: 1920, Height: 1080, Audio: true},
		},
		{
			"9:16",
			mp4test.VideoAudio(6, 0, 720, 1280),
			Info{Duration: 6 * time.Second, Width: 720, Height: 1280, Audio: true},
		},
		{
			// AAC priming often runs the audio past the video. The movie
			// header holds 8.043 s; usage must report the video's 8 s.
			"audio longer than video, no edit list",
			mp4test.VideoAudio(8, 43, 1920, 1080),
			Info{Duration: 8 * time.Second, Width: 1920, Height: 1080, Audio: true},
		},
		{
			"audio longer than video, edit list",
			func() mp4test.File {
				f := mp4test.VideoAudio(8, 43, 1920, 1080)
				// One empty edit, then 8 s of media starting after a
				// two-frame offset. Media time does not add length.
				f.Tracks[0].Duration = 8*24 + 2
				f.Tracks[0].Edits = []mp4test.Edit{
					{Segment: 0, MediaTime: -1},
					{Segment: 8000, MediaTime: 2},
				}
				f.Tracks[1].Edits = []mp4test.Edit{{Segment: 8043, MediaTime: 1024}}
				return f
			}(),
			Info{Duration: 8 * time.Second, Width: 1920, Height: 1080, Audio: true},
		},
		{
			"empty edit counts no time",
			func() mp4test.File {
				f := mp4test.VideoAudio(8, 0, 1920, 1080)
				f.Tracks[0].Edits = []mp4test.Edit{
					{Segment: 500, MediaTime: -1},
					{Segment: 7958, MediaTime: 0},
				}
				return f
			}(),
			Info{Duration: 7958 * time.Millisecond, Width: 1920, Height: 1080, Audio: true},
		},
		{
			"64-bit boxes",
			func() mp4test.File {
				f := mp4test.VideoAudio(8, 20, 3840, 2160)
				f.Version1, f.LargeSize = true, true
				f.Tracks[0].Edits = []mp4test.Edit{{Segment: 8000, MediaTime: 0}}
				return f
			}(),
			Info{Duration: 8 * time.Second, Width: 3840, Height: 2160, Audio: true},
		},
		{
			"fractional length",
			mp4test.File{
				MovieTimescale: 600,
				Tracks: []mp4test.Track{{
					Handler: "vide", Timescale: 30000, Duration: 240240,
					Width: 1280, Height: 720,
				}},
			},
			Info{Duration: 8008 * time.Millisecond, Width: 1280, Height: 720},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Probe(mp4test.Build(tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Probe = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProbeRejectsBadFiles(t *testing.T) {
	good := mp4test.Build(mp4test.VideoAudio(8, 0, 1920, 1080))
	audioOnly := mp4test.Build(mp4test.File{
		MovieTimescale: 1000,
		Tracks:         []mp4test.Track{{Handler: "soun", Timescale: 48000, Duration: 48000}},
	})
	zeroScale := mp4test.Build(mp4test.File{
		MovieTimescale: 1000,
		Tracks:         []mp4test.Track{{Handler: "vide", Timescale: 0, Duration: 10}},
	})
	zeroLength := mp4test.Build(mp4test.File{
		MovieTimescale: 1000,
		Tracks:         []mp4test.Track{{Handler: "vide", Timescale: 24, Duration: 0}},
	})
	// One unit of a huge timescale is under a nanosecond.
	roundsToZero := mp4test.Build(mp4test.File{
		MovieTimescale: 1000,
		Tracks: []mp4test.Track{{
			Handler: "vide", Timescale: 0xFFFFFFFE, Duration: 1,
			Width: 1920, Height: 1080,
		}},
	})
	editRoundsToZero := mp4test.VideoAudio(8, 0, 1920, 1080)
	editRoundsToZero.MovieTimescale = 0xFFFFFFFE
	editRoundsToZero.Tracks[0].Edits = []mp4test.Edit{{Segment: 1, MediaTime: 0}}
	zeroSize := mp4test.Build(mp4test.VideoAudio(8, 0, 0, 0))
	zeroHeight := mp4test.Build(mp4test.VideoAudio(8, 0, 1920, 0))
	noTkhd := bytes.ReplaceAll(good, []byte("tkhd"), []byte("xkhd"))
	tooManyBoxes := make([]byte, 0, 8*(maxBoxes+1))
	for range maxBoxes + 1 {
		tooManyBoxes = append(tooManyBoxes, 0, 0, 0, 8, 'f', 'r', 'e', 'e')
	}
	badSize := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(badSize[0:4], 4)
	for name, data := range map[string][]byte{
		"empty":                      nil,
		"truncated":                  good[:len(good)-10],
		"no moov":                    {0, 0, 0, 8, 'f', 'r', 'e', 'e'},
		"audio only":                 audioOnly,
		"zero timescale":             zeroScale,
		"zero length":                zeroLength,
		"length rounds to zero":      roundsToZero,
		"edit length rounds to zero": mp4test.Build(editRoundsToZero),
		"zero frame size":            zeroSize,
		"zero frame height":          zeroHeight,
		"no tkhd":                    noTkhd,
		"too many boxes":             tooManyBoxes,
		"size below header":          badSize,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Probe(data); err == nil {
				t.Fatal("bad file accepted")
			}
		})
	}
}

func FuzzProbe(f *testing.F) {
	f.Add(mp4test.Build(mp4test.VideoAudio(8, 43, 1920, 1080)))
	v1 := mp4test.VideoAudio(4, 0, 1280, 720)
	v1.Version1 = true
	v1.Tracks[0].Edits = []mp4test.Edit{{Segment: 4000, MediaTime: 0}}
	f.Add(mp4test.Build(v1))
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := Probe(data)
		if err == nil && (info.Duration <= 0 || info.Duration > maxDuration) {
			t.Fatalf("accepted out-of-range duration %s", info.Duration)
		}
	})
}
