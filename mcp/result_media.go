package mcp

import "bytes"

// mediaHeader checks format identity, not complete codec integrity. Keeping the
// set explicit makes conversion's preservation guarantee independent of OS MIME
// databases and provider support. MP3 includes streams without an ID3 tag.
func mediaHeader(data []byte, typ string) (valid, supported bool) {
	switch typ {
	case "image/png":
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")), true
	case "image/jpeg":
		return bytes.HasPrefix(data, []byte("\xff\xd8\xff")), true
	case "image/gif":
		return bytes.HasPrefix(data, []byte("GIF87a")) ||
			bytes.HasPrefix(data, []byte("GIF89a")), true
	case "image/webp":
		return riffHeader(data, "WEBP") && len(data) >= 14 &&
			string(data[12:14]) == "VP", true
	case "audio/wav":
		return riffHeader(data, "WAVE"), true
	case "audio/mpeg":
		if bytes.HasPrefix(data, []byte("ID3")) {
			return true, true
		}
		return len(data) >= 4 && data[0] == 0xff && data[1]&0xe0 == 0xe0 &&
			data[1]&0x18 != 0x08 && data[1]&0x06 != 0 &&
			data[2]&0xf0 != 0xf0 && data[2]&0x0c != 0x0c, true
	}
	return false, false
}

func riffHeader(data []byte, typ string) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == typ
}
