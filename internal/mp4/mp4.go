// Package mp4 reads the facts that video billing needs from an MP4 file: the
// video track's length, its frame size, and whether the file has an audio
// track. It reads only the moov box headers and never decodes media.
package mp4

import (
	"encoding/binary"
	"errors"
	"time"
)

// maxBoxes bounds the boxes Probe reads, so a hostile file cannot make it
// walk millions of tiny boxes.
const maxBoxes = 10000

// maxDuration rejects lengths that cannot be a real generated video and would
// overflow time.Duration arithmetic.
const maxDuration = 24 * time.Hour

// Info describes one MP4 file.
type Info struct {
	// Duration is the first video track's length with its edit list applied.
	// A longer audio track does not extend it.
	Duration time.Duration
	// Width and Height are the video track's display size from its tkhd box.
	Width, Height int
	// Audio reports whether the file has any sound track.
	Audio bool
}

type box struct {
	kind string
	body []byte
}

// Probe reads Info from a complete MP4 file. It returns an error when the file
// has no moov box, no video track, a zero timescale, a video length that
// rounds to zero, or no video frame size.
func Probe(data []byte) (Info, error) {
	budget := maxBoxes
	top, err := children(data, &budget)
	if err != nil {
		return Info{}, err
	}
	moov, ok := find(top, "moov")
	if !ok {
		return Info{}, errors.New("mp4: no moov box")
	}
	movie, err := children(moov, &budget)
	if err != nil {
		return Info{}, err
	}
	var movieScale uint32
	if mvhd, ok := find(movie, "mvhd"); ok {
		movieScale, _, err = timing(mvhd)
		if err != nil {
			return Info{}, err
		}
	}
	var info Info
	var video bool
	for _, b := range movie {
		if b.kind != "trak" {
			continue
		}
		track, err := children(b.body, &budget)
		if err != nil {
			return Info{}, err
		}
		mdia, ok := find(track, "mdia")
		if !ok {
			continue
		}
		media, err := children(mdia, &budget)
		if err != nil {
			return Info{}, err
		}
		hdlr, ok := find(media, "hdlr")
		if !ok || len(hdlr) < 12 {
			continue
		}
		switch string(hdlr[8:12]) {
		case "soun":
			info.Audio = true
		case "vide":
			if video {
				continue
			}
			video = true
			info.Duration, err = trackDuration(track, media, movieScale, &budget)
			if err != nil {
				return Info{}, err
			}
			if info.Duration <= 0 {
				return Info{}, errors.New("mp4: video track length rounds to zero")
			}
			tkhd, ok := find(track, "tkhd")
			if !ok {
				return Info{}, errors.New("mp4: video track has no tkhd box")
			}
			info.Width, info.Height, err = size(tkhd)
			if err != nil {
				return Info{}, err
			}
			if info.Width == 0 || info.Height == 0 {
				return Info{}, errors.New("mp4: video track has no frame size")
			}
		}
	}
	if !video {
		return Info{}, errors.New("mp4: no video track")
	}
	return info, nil
}

// trackDuration sums the non-empty edits in movie timescale units. Without an
// edit list, or with one that has no non-empty edit, it uses the media
// duration from mdhd.
func trackDuration(
	track, media []box,
	movieScale uint32,
	budget *int,
) (time.Duration, error) {
	if edts, ok := find(track, "edts"); ok {
		edits, err := children(edts, budget)
		if err != nil {
			return 0, err
		}
		if elst, ok := find(edits, "elst"); ok {
			total, err := editTotal(elst)
			if err != nil {
				return 0, err
			}
			if total > 0 {
				if movieScale == 0 {
					return 0, errors.New("mp4: zero movie timescale")
				}
				return scaled(total, movieScale)
			}
		}
	}
	mdhd, ok := find(media, "mdhd")
	if !ok {
		return 0, errors.New("mp4: video track has no mdhd box")
	}
	scale, units, err := timing(mdhd)
	if err != nil {
		return 0, err
	}
	if scale == 0 {
		return 0, errors.New("mp4: zero media timescale")
	}
	if units == 0 {
		return 0, errors.New("mp4: video track has no length")
	}
	return scaled(units, scale)
}

func editTotal(elst []byte) (uint64, error) {
	if len(elst) < 8 {
		return 0, errors.New("mp4: short elst box")
	}
	version := elst[0]
	count := binary.BigEndian.Uint32(elst[4:8])
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	rest := elst[8:]
	if uint64(count)*uint64(entrySize) > uint64(len(rest)) {
		return 0, errors.New("mp4: short elst box")
	}
	var total uint64
	for i := 0; i < int(count); i++ {
		entry := rest[i*entrySize:]
		var segment uint64
		var empty bool
		if version == 1 {
			segment = binary.BigEndian.Uint64(entry[0:8])
			empty = int64(binary.BigEndian.Uint64(entry[8:16])) == -1
		} else {
			segment = uint64(binary.BigEndian.Uint32(entry[0:4]))
			empty = int32(binary.BigEndian.Uint32(entry[4:8])) == -1
		}
		if empty {
			continue
		}
		if total+segment < total {
			return 0, errors.New("mp4: edit list overflows")
		}
		total += segment
	}
	return total, nil
}

// timing reads the timescale and duration from an mvhd or mdhd box, which
// share this layout.
func timing(b []byte) (scale uint32, units uint64, err error) {
	if len(b) < 4 {
		return 0, 0, errors.New("mp4: short header box")
	}
	switch b[0] {
	case 0:
		if len(b) < 20 {
			return 0, 0, errors.New("mp4: short header box")
		}
		scale = binary.BigEndian.Uint32(b[12:16])
		units = uint64(binary.BigEndian.Uint32(b[16:20]))
		if units == 0xFFFFFFFF {
			units = 0
		}
	case 1:
		if len(b) < 32 {
			return 0, 0, errors.New("mp4: short header box")
		}
		scale = binary.BigEndian.Uint32(b[20:24])
		units = binary.BigEndian.Uint64(b[24:32])
		if units == 0xFFFFFFFFFFFFFFFF {
			units = 0
		}
	default:
		return 0, 0, errors.New("mp4: unknown header box version")
	}
	return scale, units, nil
}

// size reads the 16.16 fixed-point display width and height from tkhd.
func size(tkhd []byte) (width, height int, err error) {
	if len(tkhd) < 4 {
		return 0, 0, errors.New("mp4: short tkhd box")
	}
	offset := 76
	if tkhd[0] == 1 {
		offset = 88
	}
	if len(tkhd) < offset+8 {
		return 0, 0, errors.New("mp4: short tkhd box")
	}
	width = int(binary.BigEndian.Uint32(tkhd[offset:]) >> 16)
	height = int(binary.BigEndian.Uint32(tkhd[offset+4:]) >> 16)
	return width, height, nil
}

func scaled(units uint64, scale uint32) (time.Duration, error) {
	seconds, rest := units/uint64(scale), units%uint64(scale)
	if seconds >= uint64(maxDuration/time.Second) {
		return 0, errors.New("mp4: track is too long")
	}
	return time.Duration(seconds)*time.Second +
		time.Duration(rest*uint64(time.Second)/uint64(scale)), nil
}

// children splits a container body into its boxes. A size of zero runs to the
// end of the container; a size of one has a 64-bit size after the type.
func children(data []byte, budget *int) ([]box, error) {
	var boxes []box
	for len(data) > 0 {
		*budget--
		if *budget < 0 {
			return nil, errors.New("mp4: too many boxes")
		}
		if len(data) < 8 {
			return nil, errors.New("mp4: truncated box header")
		}
		size := uint64(binary.BigEndian.Uint32(data[0:4]))
		kind := string(data[4:8])
		header := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return nil, errors.New("mp4: truncated box header")
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, errors.New("mp4: box size out of range")
		}
		boxes = append(boxes, box{kind: kind, body: data[header:size]})
		data = data[size:]
	}
	return boxes, nil
}

func find(boxes []box, kind string) ([]byte, bool) {
	for _, b := range boxes {
		if b.kind == kind {
			return b.body, true
		}
	}
	return nil, false
}
