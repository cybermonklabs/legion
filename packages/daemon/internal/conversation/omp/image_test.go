package omp

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestReadOmitsUnreadableNativeResponsesImages(t *testing.T) {
	const top = `{"type":"input_image","image_url":"data:image/png;base64,@@","detail":"low"}`
	const valid = `{"type":"input_image","image_url":"data:image/png;base64,` + attachmentFixturePNG + `"}`
	const withoutComma = `{"type":"input_image","image_url":"data:image/png%GG"}`
	const url = `{"type":"input_image","image_url":"https://example.invalid/image.png"}`
	const file = `{"type":"input_image","file_id":"file_picture","detail":"high"}`
	const invalid = `{"type":"input_image","image_url":"DaTa:image/png,%GG"}`
	const percent = `{"type":"input_image","image_url":"data:image/png,%70lum"}`
	const invalidBase64Note = `{"type":"input_text","text":"[image omitted: undecodable image/png data (invalid base64 image data)]"}`
	const malformedNote = `{"type":"input_text","text":"[image omitted: undecodable application/octet-stream data (empty image data)]"}`
	const percentNote = `{"type":"input_text","text":"[image omitted: undecodable image/png data (Image: unrecognised format (expected JPEG, PNG, WebP, GIF, BMP, TIFF, HEIC or AVIF))]"}`
	for _, role := range []string{"user", "developer"} {
		t.Run(role, func(t *testing.T) {
			f := newFixture()
			prefix := `{"type":"message","role":` + quote(role) + `,"content":[{"type":"input_text","text":"Before."},`
			suffix := `,` + valid + `,` + url + `,` + file + `,` + withoutComma + `,{"type":"input_text","text":"After."}]}`
			payload := `{"type":"openaiResponsesHistory","provider":"openai","items":[` + top + `,` + prefix + invalid + `,` + percent + suffix + `]}`
			f.add(`{"type":"message","message":{"role":` + quote(role) + `,"content":"Fallback text.","providerPayload":` + payload + `}}`)
			f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-test", "stop", `[{"type":"text","text":"Seen."}]`))
			native := read(t, f.bytes()).Messages[0].Native
			if native == nil || string(native.Payload) != payload {
				t.Fatalf("native = %+v, want the recorded payload preserved beside the replay items", native)
			}
			wantJSON := `[` + invalidBase64Note + `,` + prefix + malformedNote + `,` + percentNote + suffix + `]`
			gotJSON, err := json.Marshal(native.Items)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if json.Unmarshal(gotJSON, &got) != nil || json.Unmarshal([]byte(wantJSON), &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("native replay = %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestReadDropsUnreadableComputerScreenshotMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, kind, uri string
		drop            bool
	}{
		{"invalid image", "computer", "data:image/png;base64,cGx1bQ==", true},
		{"invalid escape", "computer", "data:image/png,%GG", true},
		{"valid image", "computer", "data:image/png;base64," + attachmentFixturePNG, false},
		{"external URL", "computer", "https://example.invalid/image.png", false},
		{"other metadata", "custom", "data:image/png;base64,@@", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-test", "toolUse", `[{"type":"toolCall","id":"call_screen|cu_screen","name":"computer","arguments":{}}]`))
			metadata := fmt.Sprintf(`{"type":%q,"screenshot":{"type":"computer_screenshot","image_url":%q},"acknowledgedSafetyChecks":[{"id":"check_1"}]}`, tc.kind, tc.uri)
			f.add(withMessageFields(toolResultEntry("call_screen|cu_screen", "computer", "Screenshot captured.", false), `,"providerMetadata":`+metadata))
			got := read(t, f.bytes()).Messages[1].Content[0]
			if tc.drop {
				metadata = ""
			}
			if string(got.ProviderMetadata) != metadata || !reflect.DeepEqual(got.Content, []conversation.Block{text("Screenshot captured.")}) {
				t.Fatalf("result = %+v with metadata %s, want recorded text and metadata %q", got, got.ProviderMetadata, metadata)
			}
		})
	}
}

func jpegWithThumbnail(t *testing.T, truncated bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	photo := buf.Bytes()
	end := len(photo)
	if truncated {
		end -= 2 // Remove the outer EOI, not the thumbnail's EOI.
	}
	payload := append([]byte("Exif\x00\x00"), photo...)
	app1 := binary.BigEndian.AppendUint16([]byte{0xff, 0xd8, 0xff, 0xe1}, uint16(2+len(payload)))
	app1 = append(app1, payload...)
	return base64.StdEncoding.EncodeToString(append(app1, photo[2:end]...))
}

func TestReadOmitsTruncatedJPEGWithEmbeddedEOI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		truncated bool
	}{{"complete", false}, {"truncated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			data := jpegWithThumbnail(t, tc.truncated)
			f := newFixture()
			f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Inspect."},{"type":"image","mimeType":"image/jpeg","data":` + quote(data) + `}]}}`)
			f.add(claudeEntry("stop", `[{"type":"text","text":"Seen."}]`))
			want := conversation.Block{Type: conversation.Image, MediaType: "image/jpeg", ImageData: data}
			if tc.truncated {
				want = text("[image omitted: undecodable image/jpeg data (truncated JPEG data: no EOI marker)]")
			}
			assertMessages(t, read(t, f.bytes()).Messages[:1], []conversation.Message{{Role: conversation.User, Content: []conversation.Block{text("Inspect."), want}}})
		})
	}
}

func TestJPEGCompletenessWalksStuffedAndRestartMarkers(t *testing.T) {
	// The synthetic scan skips FF00 and a restart marker, then ends on EOI.
	const jpeg = "\xff\xd8\xff\xda\x00\x02" + "\x01\xff\x00\xd9\xff\xff\xd0\x42" + "\xff\xd9" + "metadata"
	if !jpegStreamIsComplete([]byte(jpeg)) {
		t.Fatal("complete scan with stuffed bytes, restart marker and trailing metadata was rejected")
	}
}

func largeFixturePNG(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 8192, 4097))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestReadKeepsDecodableImageAboveFormerPixelLimit(t *testing.T) {
	data := largeFixturePNG(t)
	f := newFixture()
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Inspect."},{"type":"image","mimeType":"image/png","data":` + quote(data) + `}]}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"Seen."}]`))
	got := read(t, f.bytes()).Messages[0].Content
	if len(got) != 2 || got[1].Type != conversation.Image || got[1].MediaType != "image/png" || got[1].ImageData != data {
		t.Fatal("reader did not retain decodable PNG above 32 megapixels")
	}
}
