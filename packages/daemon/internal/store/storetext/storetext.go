// Package storetext is the rule every string the Legion daemon stores passes on its way to
// PostgreSQL, and the bound a far side's error text takes before it is stored.
//
// The store takes only valid UTF-8. On the UTF8 server encoding the daemon runs, PostgreSQL
// refuses a NUL (U+0000) and any byte that encoding cannot read, such as a lone 0xff, in a text
// value (SQLSTATE 22021), and a jsonb value refuses a \u0000 escape (22P05) and a surrogate escape
// that is not half of a pair (22P02) as well. Much of what the daemon stores comes from outside
// it — a GitHub event, a Dispatch event, a worker's tool output, a far side's error — so it cannot be
// refused at its source, and a write that fails on it fails the whole fact. A NUL comes even from
// a far side that answers in JSON: the `error` Oh My Pi gives a refused prompt is decoded, and a
// \u0000 escape in it becomes a NUL byte in the daemon's string. The rule makes such text storable
// instead (Text, TextBytes, JSON): each run of bytes that reads as no character becomes U+FFFD
// rather than going, so the stored text still shows where the words were unreadable;
// each NUL becomes U+FFFD as well; and in a JSON value, each \u0000 escape and each lone surrogate
// escape does. Every other character stays as written, and a text that is already storable is
// returned as it is, with no allocation, so a caller can give every string it stores through the
// rule. The rule leaves URLs alone: the store holds text that people and agents wrote, whose links
// keep their queries. Only a far side's error text loses its URL credentials (FarSideText, which
// FarSideError and LogWhole apply).
//
// The rule is a package of its own, under internal/store, because internal/store imports
// internal/supervise, and internal/supervise stores a far side's error text by this rule too (a
// delivery's send error): a function of package store would be an import cycle. The package
// imports nothing of the daemon but internal/redact, so internal/store, internal/supervise,
// internal/record and internal/leakguard can all call it.
//
// A far side's error text is bounded as well, keeping its start (FarSideError). What a reader of
// the record needs is why the far side failed: the first line of its answer and enough of what
// follows to name the failure — the shim's words name a stale lock's path and its age within the
// first two hundred characters — so FarSideErrorRunes is more room than the diagnosis asks for and
// far less than the text can be. What the bound cuts is the rest of a relayed command's output: a
// refused adoption carries the wedged command's whole stderr, measured at about a megabyte
// (INFRA-1480), which would otherwise be stored on every failed attempt and served again in every
// listing that reads the record. The start is what is kept because that is where the far side
// names the failure. The cut counts runes rather than bytes, so it can never make the text invalid
// again by ending inside a character. Nothing of the far side's answer is lost this way, only
// moved: LogWhole writes the whole text to the log, which has no store to refuse it, so an
// operator who needs the megabyte reads it there.
package storetext

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// replacement is what the rule writes for each NUL, each run of invalid bytes and each refused
// JSON escape: U+FFFD, the replacement character.
const replacement = "\uFFFD"

// Text is s as the store takes it: each run of bytes that is not UTF-8 becomes U+FFFD, as
// strings.ToValidUTF8(s, "\uFFFD") makes it, and then each NUL becomes U+FFFD. Every other
// character stays, the six characters `\u0000` among them, which a text column takes as written
// (JSON applies the rule to such an escape in a JSON value). Text is idempotent, and a text it
// does not change is returned as it is, with no allocation.
func Text(s string) string {
	if storable(s) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, replacement), "\x00", replacement)
}

// storable reports whether Text keeps s as it is: s is valid UTF-8 and holds no NUL.
func storable(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

// TextBytes is the rule of Text on bytes. It returns b itself when the rule changes nothing, and a
// new slice otherwise: it never writes into b.
func TextBytes(b []byte) []byte {
	if utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 {
		return b
	}
	return bytes.ReplaceAll(bytes.ToValidUTF8(b, []byte(replacement)), []byte{0}, []byte(replacement))
}

// JSON is the rule of Text on each string of the JSON text b, keys included, as a jsonb column
// takes it.
//
// First the byte rule of TextBytes applies to the raw text: neither an invalid byte nor a raw NUL
// can be JSON structure, so this changes strings only. Then, only when the text holds a \u0000
// escape or a surrogate escape that is not half of a pair (a high one, \ud800 to \udbff, followed
// at once by a low one, \udc00 to \udfff), the value is decoded (json.Decoder with UseNumber),
// Text applies to every string and key at any depth (storableValue), where Go's decoder made the
// escape a NUL or a lone surrogate U+FFFD, and it is encoded again (SetEscapeHTML(false), no
// trailing newline). For jsonb the result is the same value: jsonb keeps no white space and no key
// order, a duplicate key keeps its last value in both Go and jsonb, and json.Number keeps each
// number's text (1e400, 0.10). Two keys that the rule makes one (a\u0000 and a\ufffd) keep the
// value of the greater original key, so the result does not depend on a map's order.
//
// A text that does not decode, or holds more than one value, is returned after the byte rule, and
// PostgreSQL refuses it as before: invalid JSON is a defect of the daemon, not outside text. JSON
// returns b itself when nothing changes, and never writes into b.
func JSON(b []byte) []byte {
	b = TextBytes(b)
	if !refusedEscape(b) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return b
	}
	if _, err := dec.Token(); err != io.EOF {
		return b
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(storableValue(v)) != nil {
		return b
	}
	return out.Bytes()[:out.Len()-1] // Encode ends the value with a newline
}

// storableValue is v, a value json.Decoder decoded with UseNumber, with Text applied to every
// string and key; of two keys the rule makes one, the entry of the greater original key is kept
// (originalKeys). It is a walk of its own rather than Strings because Strings stops at maxDepth,
// and a decoded value costs it two levels for each array or object (the container and the
// interface that holds it): a string nested deeper would be left as Go decoded it, its NUL encoded
// as \u0000 again, and the server would refuse the whole value. A decoded value holds no pointer,
// so no cycle, and Go's decoder bounds its nesting itself, so this walk needs no bound.
func storableValue(v any) any {
	switch v := v.(type) {
	case string:
		return Text(v)
	case []any:
		for i, e := range v {
			v[i] = storableValue(e)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		originals := make(originalKeys, len(v))
		for k, e := range v {
			if key := Text(k); originals.keeps(key, k) {
				out[key] = storableValue(e)
			}
		}
		return out
	}
	return v
}

// originalKeys is the one rule that decides a key collision, for both walks that apply Text to a
// map's keys (storableValue and Strings): of two or more keys that the rule makes one, the entry
// of the greatest original key is kept, so the result does not depend on a map's order. It maps
// each key as the rule makes it to the original key of the entry kept under it so far.
type originalKeys map[string]string

// keeps reports whether the entry of original key original, whose key the rule makes key, is kept
// over every entry put under key so far, and records it as the one kept when it is.
func (o originalKeys) keeps(key, original string) bool {
	if kept, found := o[key]; found && kept > original {
		return false
	}
	o[key] = original
	return true
}

// Changes reports whether the rule changes s where it is stored, in a text or a jsonb column:
// Text changes s, or s holds a \u0000 escape or a surrogate escape that is not half of a pair,
// which JSON replaces when s is a JSON text. It reads s with the same scan as Text and JSON and
// allocates nothing, so a caller can ask it of every string it is about to store and pass on
// unchanged the strings it answers false for. A text that holds such an escape and is no JSON
// reads true, although neither rule changes it: true says that the rule of the column's type
// decides.
func Changes(s string) bool {
	return !storable(s) || refusedEscape(s)
}

// refusedEscape reports whether s holds a JSON escape that jsonb refuses: \u0000, a low surrogate
// escape (\udc00 to \udfff) that no high one comes just before, or a high surrogate escape (\ud800
// to \udbff) that no low one comes just after. An escape is read whole, so an escaped backslash
// is one escape: `\\u0000` is a backslash, then the text u0000, and no NUL.
func refusedEscape[T string | []byte](s T) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		i++ // the escaped character, a backslash among them
		if i == len(s) || s[i] != 'u' {
			continue
		}
		r, ok := hex4(s, i+1)
		if !ok {
			continue
		}
		i += 4 // the last hex digit
		switch {
		case r == 0, 0xDC00 <= r && r <= 0xDFFF:
			return true
		case 0xD800 <= r && r <= 0xDBFF:
			if i+2 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
				if low, ok := hex4(s, i+3); ok && 0xDC00 <= low && low <= 0xDFFF {
					i += 6
					continue
				}
			}
			return true
		}
	}
	return false
}

// hex4 is the value of the four hex digits of s at i, and false when s holds no four there.
func hex4[T string | []byte](s T, i int) (rune, bool) {
	if i+4 > len(s) {
		return 0, false
	}
	var r rune
	for j := i; j < i+4; j++ {
		c := s[j]
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		case 'A' <= c && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, false
		}
		r = r<<4 | rune(c)
	}
	return r, true
}

// FarSideErrorRunes bounds a far side's error text as the daemon stores it (FarSideError): enough
// for the far side to name its failure, far less than a relayed command's whole stderr (the
// package's doc gives the measure).
const FarSideErrorRunes = 2048

// FarSideText is a far side's error text, whole, as the daemon stores and logs it: Text, then
// redact.URLCredentials. The order is the point: the text is made valid before the URL rule reads
// it.
func FarSideText(s string) string {
	return redact.URLCredentials(Text(s))
}

// FarSideError is a far side's error text as the daemon stores it: FarSideText, then, when that is
// more than FarSideErrorRunes runes, its first FarSideErrorRunes-1 runes and "…". The result is at
// most FarSideErrorRunes runes, and FarSideError(FarSideError(s)) == FarSideError(s). Text makes
// the text valid first, and redact.Runes then removes the credentials before it cuts, so a cut can
// never leave a part of a userinfo or a query behind.
func FarSideError(s string) string {
	return redact.Runes(Text(s), FarSideErrorRunes)
}

// logPartBytes bounds one part of the text LogWhole writes. A JSON handler writes a character of a
// cleaned text in at most six bytes (a control character's \u00XX escape), so a part's line stays
// under 192 KiB, below the 256 KiB Cloud Logging takes in one entry.
const logPartBytes = 32 << 10

// LogWhole writes to log the whole of text, a far side's error text, when FarSideError would cut
// it, so that what the store does not keep is in the log: FarSideText(text), the text FarSideError
// cuts. It is written at Info in parts of at most 32 KiB, cut at rune boundaries, one record of msg
// for each part, with args, then "part" (from 1) and "parts" (their number), then the part itself
// as "error". The parts in the order of "part" join to the whole text. A text FarSideError keeps
// whole is not logged, because the store holds it whole.
func LogWhole(log *slog.Logger, msg string, text string, args ...any) {
	if !log.Enabled(context.Background(), slog.LevelInfo) {
		return
	}
	text = FarSideText(text)
	if utf8.RuneCountInString(text) <= FarSideErrorRunes {
		return
	}
	var parts []string
	for rest := text; rest != ""; {
		n := min(len(rest), logPartBytes)
		for n < len(rest) && !utf8.RuneStart(rest[n]) {
			n--
		}
		parts = append(parts, rest[:n])
		rest = rest[n:]
	}
	args = slices.Clip(args)
	for i, part := range parts {
		log.Info(msg, append(args, "part", i+1, "parts", len(parts), "error", part)...)
	}
}
