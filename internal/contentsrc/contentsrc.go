// Package contentsrc finds the sources inside content blocks. It is internal
// because it returns pointers that alias the blocks, so a caller can change a
// recorded block. Only the runner and the request builder need that.
package contentsrc

import "github.com/deepnoodle-ai/dashi/content"

// Block returns pointers to the sources that b holds: the image, document,
// audio, or video source, or none. The pointers alias b's payload.
func Block(b content.Block) []*content.Source {
	switch {
	case b.Image != nil:
		return []*content.Source{&b.Image.Source}
	case b.Document != nil:
		return []*content.Source{&b.Document.Source}
	case b.Audio != nil:
		return []*content.Source{&b.Audio.Source}
	case b.Video != nil:
		return []*content.Source{&b.Video.Source}
	}
	return nil
}

// Blocks returns the sources in every block of blocks.
func Blocks(blocks ...[]content.Block) []*content.Source {
	var out []*content.Source
	for _, list := range blocks {
		for _, b := range list {
			out = append(out, Block(b)...)
		}
	}
	return out
}
