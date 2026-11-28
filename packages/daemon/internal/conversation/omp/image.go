package omp

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"strings"
)

const unrecognizedImage = "Image: unrecognised format (expected JPEG, PNG, WebP, GIF, BMP, TIFF, HEIC or AVIF)"

func imageOmission(mime, reason string) string {
	if mime == "" {
		mime = "image"
	}
	return "[image omitted: undecodable " + mime + " data (" + reason + ")]"
}

// imageURIOmission checks only data URIs. The pin's URI parser treats ;base64
// payloads as literal base64 and all other payloads as percent-encoded bytes.
func imageURIOmission(uri string) string {
	if len(uri) < 5 || !strings.EqualFold(uri[:5], "data:") {
		return ""
	}
	header, payload, ok := strings.Cut(uri[5:], ",")
	if !ok {
		return ""
	}
	const flag, octets = ";base64", "application/octet-stream"
	cut := len(header) - len(flag)
	encoded := cut >= 0 && strings.EqualFold(header[cut:], flag)
	if encoded {
		header = header[:cut]
	}
	if header == "" {
		header = octets
	}
	var reason string
	if encoded {
		reason = inlineImageFailure(header, payload)
	} else {
		body, err := url.PathUnescape(payload) // Unlike QueryUnescape, this leaves '+' alone.
		if err != nil {
			return imageOmission(octets, "empty image data")
		}
		reason = imageBytesFailure(header, []byte(body))
	}
	if reason == "" {
		return ""
	}
	return imageOmission(header, reason)
}

func unreadableComputerScreenshot(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var metadata struct {
		Type       string
		Screenshot struct {
			ImageURL string `json:"image_url"`
		}
	}
	return json.Unmarshal(raw, &metadata) == nil && metadata.Type == "computer" &&
		imageURIOmission(metadata.Screenshot.ImageURL) != ""
}

// inlineImageFailure mirrors the pin's base64 and MIME checks. Go decodes JPEG,
// PNG and GIF, but checks only the header of WebP, BMP, TIFF, HEIC and AVIF.
// With a valid header and a damaged body, those formats remain in the history.
func inlineImageFailure(mime, data string) string {
	if data == "" {
		return "empty image data"
	}
	if strings.ContainsAny(data, "\r\n") {
		return "invalid base64 image data"
	}
	encoding := base64.RawStdEncoding.Strict()
	if strings.HasSuffix(data, "=") {
		encoding = base64.StdEncoding.Strict()
	}
	body, err := encoding.DecodeString(data)
	if err != nil {
		return "invalid base64 image data"
	}
	return imageBytesFailure(mime, body)
}

func imageBytesFailure(mime string, body []byte) string {
	if len(body) == 0 {
		return "empty image data"
	}
	detected := imageHeaderType(body)
	switch detected {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if detected != strings.ToLower(mime) {
			return "declared " + mime + " but contains " + detected
		}
	}
	if strings.EqualFold(mime, "image/jpeg") && !jpegStreamIsComplete(body) {
		return "truncated JPEG data: no EOI marker"
	}
	switch detected {
	case "image/png", "image/jpeg", "image/gif":
		_, _, err := image.Decode(bytes.NewReader(body))
		if err != nil {
			return "Image: " + err.Error()
		}
	case "":
		return unrecognizedImage
	}
	return ""
}

// jpegStreamIsComplete finds the outer EOI marker, not an EOI in a thumbnail
// stored in an application segment. Bytes after the outer EOI are permitted.
func jpegStreamIsComplete(body []byte) bool {
	if len(body) < 4 || body[0] != 0xff || body[1] != 0xd8 {
		return false
	}
	for at := 2; at < len(body); {
		if body[at] != 0xff {
			return false
		}
		for at < len(body) && body[at] == 0xff {
			at++
		}
		if at == len(body) {
			return false
		}
		marker := body[at]
		at++
		switch {
		case marker == 0xd9:
			return true
		case marker == 0x01 || jpegRestartMarker(marker):
			continue
		case marker == 0xd8 || len(body)-at < 2:
			return false
		}
		length := int(binary.BigEndian.Uint16(body[at:]))
		if length < 2 || length > len(body)-at {
			return false
		}
		at += length
		if marker != 0xda {
			continue
		}
		// Inside a scan, FF00 is stuffed data and restart markers do not
		// end the scan. Any other marker returns to segment processing.
		for {
			next := bytes.IndexByte(body[at:], 0xff)
			if next < 0 {
				return false
			}
			at += next
			scan := at + 1
			for scan < len(body) && body[scan] == 0xff {
				scan++
			}
			if scan == len(body) {
				return false
			}
			if body[scan] != 0 && !jpegRestartMarker(body[scan]) {
				break
			}
			at = scan + 1
		}
	}
	return false
}

func jpegRestartMarker(marker byte) bool {
	return marker >= 0xd0 && marker <= 0xd7
}

func imageHeaderType(body []byte) string {
	switch sniffed := http.DetectContentType(body); sniffed {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return sniffed
	}
	if bytes.HasPrefix(body, []byte("II*\x00")) || bytes.HasPrefix(body, []byte("MM\x00*")) {
		return "image/tiff"
	}
	if len(body) >= 12 && string(body[4:8]) == "ftyp" {
		end := min(int(binary.BigEndian.Uint32(body)), len(body))
		for offset := 8; offset+4 <= end; {
			switch string(body[offset : offset+4]) {
			case "avif", "avis":
				return "image/avif"
			case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
				return "image/heic"
			}
			if offset == 8 {
				offset += 8 // Skip the minor version after the major brand.
			} else {
				offset += 4
			}
		}
	}
	return ""
}
