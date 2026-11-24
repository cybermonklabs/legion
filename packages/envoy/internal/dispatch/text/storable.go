package text

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Storable reports whether value is text PostgreSQL can store: UTF-8 holding no U+0000. Its text
// type refuses a byte sequence that is not UTF-8, and its text and jsonb both refuse U+0000, so a
// statement given anything else fails.
func Storable(value string) bool {
	return strings.IndexByte(value, 0) < 0 && utf8.ValidString(value)
}

// StorableReplacement is value with each byte PostgreSQL cannot store replaced by U+FFFD: invalid
// UTF-8 is made valid first, then each U+0000 becomes U+FFFD. A recorded error can come from an
// input parser, so it passes through this before it reaches a text or jsonb column.
func StorableReplacement(value string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "\uFFFD")
}

// StorableBytes is Storable for text held as bytes, such as an uploaded file.
func StorableBytes(value []byte) bool {
	return bytes.IndexByte(value, 0) < 0 && utf8.Valid(value)
}

// nulEscape is the only way JSON carries U+0000: a decoder refuses the raw control character.
var nulEscape = []byte(`\u0000`)

// NulEscaped is a name a caller wrote, a JSON member's, a query parameter's or a multipart field's,
// as a refusal names it: each U+0000 in it written as the six characters \u0000, so no refusal
// sends the character back.
func NulEscaped(name string) string {
	return strings.ReplaceAll(name, "\x00", `\u0000`)
}

// NulInJSON is the first string of the JSON value data opens with - a member name or a value, in
// the order data writes them - that holds U+0000, and its place: the path the caller wrote it at
// under prefix, such as title, options[1].label or ops[0].attributes.title, each U+0000 of a
// member's name written as the six characters \u0000 so the place carries none, or prefix itself
// for the value data is, "body" when prefix is empty. found is false when no string holds one. It
// answers the string, so each caller says where in it the U+0000 stands in its own count, and
// never sends the string back. Data without the escape is passed over unread. Every string of the
// value is read, each copy of a repeated member among them, because a decoder keeps strings of
// more copies than the last: it merges a repeated object into its first copy, so a member the
// later copy leaves out stays as the first copy set it, and a null leaves a string as it was. A
// number is read as its text, so a number no decoder parses refuses nothing here. The read stops
// where data stops being JSON, which the caller's own decoder refuses.
func NulInJSON(prefix string, data []byte) (place, value string, found bool) {
	if !bytes.Contains(data, nulEscape) {
		return "", "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var open []jsonFrame
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", "", false
		}
		if token == json.Delim('}') || token == json.Delim(']') {
			open = open[:len(open)-1]
			if len(open) == 0 {
				return "", "", false
			}
			continue
		}
		at := prefix
		if len(open) > 0 {
			frame := &open[len(open)-1]
			switch frame.next {
			case nextElement:
				at = fmt.Sprintf("%s[%d]", frame.path, frame.elements)
				frame.elements++
			case nextName:
				// The token is a member's name; its value is the next one.
				name, _ := token.(string)
				frame.member, frame.next = NulEscaped(name), nextValue
				if frame.path != "" {
					frame.member = frame.path + "." + frame.member
				}
				if strings.IndexByte(name, 0) >= 0 {
					return frame.member, name, true
				}
				continue
			case nextValue:
				at, frame.next = frame.member, nextName
			}
		}
		switch token := token.(type) {
		case json.Delim:
			frame := jsonFrame{path: at, next: nextElement}
			if token == '{' {
				frame.next = nextName
			}
			open = append(open, frame)
		case string:
			if strings.IndexByte(token, 0) >= 0 {
				if at == "" {
					at = "body"
				}
				return at, token, true
			}
		}
		if len(open) == 0 {
			return "", "", false
		}
	}
}

// jsonFrame is an array or an object NulInJSON reads inside: where it stands, what the token read
// next stands for in it, its count of elements read for an array, and, for an object, the path of
// the member whose name was read last.
type jsonFrame struct {
	path     string
	next     jsonNext
	elements int
	member   string
}

// jsonNext is what the token read next inside a jsonFrame stands for: an array's next element,
// an object's next member name, or the value of the member just named.
type jsonNext int

const (
	nextElement jsonNext = iota
	nextName
	nextValue
)
