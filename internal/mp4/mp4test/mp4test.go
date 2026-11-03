// Package mp4test builds small MP4 files for tests. The files hold only the
// boxes that mp4.Probe reads, plus an ftyp and an mdat, so they are not
// playable.
package mp4test

import "encoding/binary"

// File describes the movie to build.
type File struct {
	// MovieTimescale is the mvhd timescale, which edit lists use.
	MovieTimescale uint32
	Tracks         []Track
	// Version1 writes 64-bit mvhd, tkhd, mdhd, and elst boxes.
	Version1 bool
	// LargeSize writes the mdat box with a 64-bit size.
	LargeSize bool
}

// Track describes one trak box.
type Track struct {
	// Handler is the hdlr type, such as "vide" or "soun".
	Handler   string
	Timescale uint32
	Duration  uint64
	Width     int
	Height    int
	// Edits, when set, writes an edts/elst box.
	Edits []Edit
}

// Edit is one elst entry. A MediaTime of -1 is an empty edit.
type Edit struct {
	Segment   uint64
	MediaTime int64
}

// VideoAudio returns a 24 fps video track and a 48 kHz audio track. The audio
// runs extra past the video's end, as AAC priming often makes it do.
func VideoAudio(seconds, extraMillis uint64, width, height int) File {
	return File{
		MovieTimescale: 1000,
		Tracks: []Track{
			{
				Handler:   "vide",
				Timescale: 24,
				Duration:  seconds * 24,
				Width:     width,
				Height:    height,
			},
			{
				Handler:   "soun",
				Timescale: 48000,
				Duration:  seconds*48000 + extraMillis*48,
			},
		},
	}
}

// Build returns the encoded file.
func Build(f File) []byte {
	var moov []byte
	var longest uint64
	for _, t := range f.Tracks {
		if t.Timescale > 0 {
			longest = max(longest, t.Duration*uint64(f.MovieTimescale)/uint64(t.Timescale))
		}
	}
	moov = append(moov, encode("mvhd", header(f.Version1, f.MovieTimescale, longest, 100))...)
	for i, t := range f.Tracks {
		var trak []byte
		trak = append(trak, encode("tkhd", tkhd(f.Version1, uint32(i+1), t.Width, t.Height))...)
		if len(t.Edits) > 0 {
			trak = append(trak, encode("edts", encode("elst", elst(f.Version1, t.Edits)))...)
		}
		hdlr := make([]byte, 24)
		copy(hdlr[8:12], t.Handler)
		mdia := append(
			encode("mdhd", header(f.Version1, t.Timescale, t.Duration, 4)),
			encode("hdlr", hdlr)...,
		)
		trak = append(trak, encode("mdia", mdia)...)
		moov = append(moov, encode("trak", trak)...)
	}
	out := encode("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41"))
	mdat := []byte("not really media")
	if f.LargeSize {
		large := make([]byte, 16)
		binary.BigEndian.PutUint32(large[0:4], 1)
		copy(large[4:8], "mdat")
		binary.BigEndian.PutUint64(large[8:16], uint64(16+len(mdat)))
		out = append(out, append(large, mdat...)...)
	} else {
		out = append(out, encode("mdat", mdat)...)
	}
	return append(out, encode("moov", moov)...)
}

func encode(kind string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], kind)
	return append(out, body...)
}

// header writes the shared mvhd and mdhd prefix, then pad bytes for the
// fields Probe ignores.
func header(v1 bool, scale uint32, units uint64, pad int) []byte {
	if v1 {
		b := make([]byte, 32+pad)
		b[0] = 1
		binary.BigEndian.PutUint32(b[20:24], scale)
		binary.BigEndian.PutUint64(b[24:32], units)
		return b
	}
	b := make([]byte, 20+pad)
	binary.BigEndian.PutUint32(b[12:16], scale)
	binary.BigEndian.PutUint32(b[16:20], uint32(units))
	return b
}

func tkhd(v1 bool, id uint32, width, height int) []byte {
	offset := 76
	if v1 {
		offset = 88
	}
	b := make([]byte, offset+8)
	if v1 {
		b[0] = 1
		binary.BigEndian.PutUint32(b[20:24], id)
	} else {
		binary.BigEndian.PutUint32(b[12:16], id)
	}
	binary.BigEndian.PutUint32(b[offset:], uint32(width)<<16)
	binary.BigEndian.PutUint32(b[offset+4:], uint32(height)<<16)
	return b
}

func elst(v1 bool, edits []Edit) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(edits)))
	if v1 {
		b[0] = 1
	}
	for _, e := range edits {
		if v1 {
			entry := make([]byte, 20)
			binary.BigEndian.PutUint64(entry[0:8], e.Segment)
			binary.BigEndian.PutUint64(entry[8:16], uint64(e.MediaTime))
			binary.BigEndian.PutUint32(entry[16:20], 1<<16)
			b = append(b, entry...)
			continue
		}
		entry := make([]byte, 12)
		binary.BigEndian.PutUint32(entry[0:4], uint32(e.Segment))
		binary.BigEndian.PutUint32(entry[4:8], uint32(int32(e.MediaTime)))
		binary.BigEndian.PutUint32(entry[8:12], 1<<16)
		b = append(b, entry...)
	}
	return b
}
