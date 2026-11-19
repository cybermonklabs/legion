// Package logmask is what keeps a placeholder, and the bytes a peer sent, off the broker's log.
//
// Much of what a line names is a command's own text - the method, the path, the host and port it
// asked for, a reason or an error that quotes them, a refusal's correlation id from
// X-Correlation-Id, the name in a request's path - and a command can write its own placeholder
// into any of it. So the broker writes every line through Writer, under its JSON handler, which
// writes each placeholder in the line as Mark, and points slog and the log package at that handler
// (SetDefault).
//
// The mask reads the line the handler wrote, not the values it was given: slog's JSON handler
// writes each record as one JSON object in one Write, under its own lock. The mask decodes each
// string of that object - each key, a group's name among them, and each string value, which is
// how the handler writes a message, an error, a value's MarshalJSON or MarshalText, and the text
// slog writes for a value whose method failed - and writes again, with the placeholder as Mark,
// only a string whose decoded text holds one, leaving every other byte of the line as the handler
// wrote it. So it reads a value once, as written, after any ReplaceAttr; it calls no method of a
// value, and cannot panic for one; a placeholder hidden in a JSON escape is masked; a caller's
// text changes no token but its own, and the line stays one JSON object. What it cannot see is a
// placeholder the handler writes in another spelling: a []byte, which the JSON handler writes in
// base64 at any depth, a placeholder a value writes percent-escaped or split, and the 32
// characters after the prefix written without it. The broker logs no such value.
//
// A real value the swap put in is no placeholder, and the mask cannot see it. What keeps it off
// a line is that no line reads a request's headers or body, and that each run of bytes a library
// quotes from a peer, which can hold it, is written as PeerBytesShown (HidePeerBytes).
package logmask

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// Mark is what a line writes in place of a placeholder.
const Mark = "[placeholder]"

// markToken is Mark as one JSON string.
const markToken = `"` + Mark + `"`

// NotJSON is the line the mask writes in place of a line that is no JSON object: one it cannot
// read is one it cannot mask, so none of it is written.
const NotJSON = `{"severity":"ERROR","message":"broker: a log line that was no JSON object was not written"}`

// prefix is the prefix every placeholder the broker mints starts with (requests.PlaceholderPrefix),
// in any case: Go's case folding, which also reads ſ as s.
var prefix = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(requests.PlaceholderPrefix))

// Writer is w behind the placeholder mask, for the lines slog's JSON handler writes: each line of
// each Write is read as one JSON object, each of its strings holding a placeholder is written
// again with it as Mark, and a line that is no JSON object is written as NotJSON. Each Write is
// read on its own, so a record must arrive in one Write, as slog's handler writes it
// (TestSlogWritesEachRecordInOneWrite); one split over two would be two lines that are no JSON
// object.
func Writer(w io.Writer) io.Writer { return maskWriter{w} }

type maskWriter struct{ w io.Writer }

func (m maskWriter) Write(p []byte) (int, error) {
	masked, same := maskLines(p)
	if same {
		return m.w.Write(p)
	}
	if _, err := m.w.Write(masked); err != nil {
		return 0, err
	}
	return len(p), nil
}

// maskLines is p with each of its lines masked (maskLine), and true where that changed nothing:
// then it is p itself, and no line of it was copied. A Write that changed ends in a newline, so a
// line written as NotJSON is a line of its own.
func maskLines(p []byte) ([]byte, bool) {
	if len(p) == 0 {
		return p, true
	}
	var out []byte
	start, rest := 0, bytes.TrimSuffix(p, []byte("\n"))
	for {
		line, after, more := bytes.Cut(rest, []byte("\n"))
		masked, kept := maskLine(line)
		switch {
		case !kept && out == nil:
			out = append(append(make([]byte, 0, len(p)+len(NotJSON)), p[:start]...), masked...)
		case out != nil:
			out = append(out, masked...)
		}
		if !more {
			break
		}
		if out != nil {
			out = append(out, '\n')
		}
		start, rest = start+len(line)+1, after
	}
	if out == nil {
		return p, true
	}
	return append(out, '\n'), false
}

// maskLine is line, one JSON object, with each string whose decoded text holds a placeholder
// written again with Mark in its place and every other byte as it came, and true where no string
// did; a line that is no JSON object is NotJSON. A line that mayHoldPlaceholder clears is read no
// further.
func maskLine(line []byte) ([]byte, bool) {
	if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(line) {
		return []byte(NotJSON), false
	}
	if !mayHoldPlaceholder(line) {
		return line, true
	}
	var out []byte
	kept := 0
	for i := 0; i < len(line); {
		if line[i] != '"' {
			i++
			continue
		}
		end := stringEnd(line, i)
		if masked, ok := maskString(line[i:end]); ok {
			out = append(append(out, line[kept:i]...), masked...)
			kept = end
		}
		i = end
	}
	if out == nil {
		return line, true
	}
	return append(out, line[kept:]...), false
}

// mayHoldPlaceholder is false only for a line no string of which can hold a placeholder once
// decoded: one of ASCII alone, with no escape, that holds the prefix in no ASCII case. A line
// with an escape or a byte past ASCII is read string by string, so an escape is decoded and a
// letter Go's case folding reads as one of the prefix's, such as ſ, is read as that letter.
func mayHoldPlaceholder(line []byte) bool {
	opening := requests.PlaceholderPrefix
	for i, b := range line {
		switch {
		case b == '\\' || b >= utf8.RuneSelf:
			return true
		case b|0x20 == opening[0] && len(line)-i >= len(opening) && opensWithASCIIFold(line[i:], opening):
			return true
		}
	}
	return false
}

// opensWithASCIIFold is whether text opens with lower, written in lower case, in any ASCII case.
func opensWithASCIIFold(text []byte, lower string) bool {
	for j := range len(lower) {
		c := text[j]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[j] {
			return false
		}
	}
	return true
}

// stringEnd is the index just past the JSON string that opens at line[start], in a line that is
// valid JSON: past the first quotation mark after it that no backslash escapes.
func stringEnd(line []byte, start int) int {
	for i := start + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(line)
}

// maskString is token, one JSON string, written again with each placeholder its decoded text
// holds as Mark (maskText), HTML left unescaped as slog's handler leaves it, and false for a
// string that holds none. A string with no escape is its own text, read where it lies. The mask
// fails closed: a string it cannot decode, or cannot write again, is written as Mark, whole, since
// the mask cannot say what it holds. Writer reads only a line json.Valid passes, whose every string
// decodes, and a Go string always encodes, so neither happens from Writer.
func maskString(token []byte) ([]byte, bool) {
	text := token[1 : len(token)-1]
	if bytes.IndexByte(text, '\\') >= 0 {
		var decoded string
		if err := json.Unmarshal(token, &decoded); err != nil {
			return []byte(markToken), true
		}
		text = []byte(decoded)
	}
	masked, ok := maskText(text)
	if !ok {
		return nil, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(string(masked)); err != nil {
		return []byte(markToken), true
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), true
}

// maskText is text with each placeholder in it written as Mark, and false where it holds none. A
// placeholder is the prefix, in any case, and the letters and digits after it, at most as many as
// the broker mints (requests.PlaceholderLength), up to where the next prefix starts: so of two
// placeholders written together, or of the prefix written before a placeholder, each is a
// placeholder of its own, and none takes the opening of the next one. A letter is read in any
// case, as Go's case folding reads one, so ſ and the Kelvin sign are letters here too.
func maskText(text []byte) ([]byte, bool) {
	openings := prefix.FindAllIndex(text, -1)
	if openings == nil {
		return nil, false
	}
	var out []byte
	kept := 0
	for i, opening := range openings {
		limit := len(text)
		if i+1 < len(openings) {
			limit = openings[i+1][0]
		}
		end := opening[1]
		for range requests.PlaceholderLength {
			r, size := utf8.DecodeRune(text[end:limit])
			if size == 0 || !minted(r) {
				break
			}
			end += size
		}
		out = append(append(out, text[kept:opening[0]]...), Mark...)
		kept = end
	}
	return append(out, text[kept:]...), true
}

// minted is whether r is a letter or digit of a placeholder, in any case as Go's case folding
// reads one: an ASCII letter or digit, ſ (s) or the Kelvin sign (k).
func minted(r rune) bool {
	return '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || r == 'ſ' || r == '\u212a'
}

// SetDefault makes handler the process's default logger and points the log package at it
// (StandardLog), so every line written through slog or the log package is handler's: the
// broker's own logging (cmd/broker's setUpLogging) calls it, and so does a test that routes lines
// as the broker does. slog.SetDefault points the log package at a writer of its own for every
// handler but slog's own, which writes each record through the log package, whose output holds
// the log package's lock while it writes: a log package pointed at that handler would wait on its
// own lock, and every log call with it. So where slog.SetDefault leaves the log package's output
// as it was, as it does for that handler and for any copy of it (WithAttrs, WithGroup), SetDefault
// leaves it too.
func SetDefault(handler slog.Handler) {
	logger := slog.New(handler)
	output := log.Writer()
	slog.SetDefault(logger)
	if log.Writer() == output {
		return
	}
	log.SetFlags(0)
	log.SetOutput(StandardLog(logger))
}

// PeerBytesShown is what a line writes in place of the bytes a library quoted from a peer.
const PeerBytesShown = `"[service bytes not shown]"`

// HidePeerBytes is text with each line's text from its first quotation mark to its end written as
// PeerBytesShown. net/http, net/textproto and httputil quote with %q every run of bytes they repeat
// from a peer - an answer's bytes on an idle connection, a header or trailer line they could not
// read, a status line, an ALPN list - and a bound service is sent the real value the swap put in,
// so it can send it back; the mask cannot see a real value. A reading that paired quotation marks
// would be misled by a stray one before a run, which would close where the run opens and leave the
// run itself in clear, so the whole rest of the line goes: a line keeps only its own words before
// the first quotation mark, which say what kind of thing went wrong.
func HidePeerBytes(text string) string {
	if !strings.Contains(text, `"`) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if quote := strings.IndexByte(line, '"'); quote >= 0 {
			lines[i] = line[:quote] + PeerBytesShown
		}
	}
	return strings.Join(lines, "\n")
}

// LibraryLines is a write a library made to a log of its own, as one line takes it: its peer
// bytes hidden (HidePeerBytes), its final newline dropped, and cut at its first newline into its
// first line and the rest, its stack, "" for a write of one line.
func LibraryLines(p []byte) (first, stack string) {
	first, stack, _ = strings.Cut(HidePeerBytes(strings.TrimSuffix(string(p), "\n")), "\n")
	return first, stack
}

// StandardLog is the log package's output once SetDefault has run: one line of logger at INFO
// for each write, as slog.SetDefault's own writer makes one. Every write is a library's text
// (LibraryLines). The line's message is the first line cut to answer.FieldBytes (answer.Clip),
// since a library's first line can repeat what a client or a service sent, and its stack is every
// line after it, whole: a recovered panic's stack, which a server with no ErrorLog of its own
// writes here and which the cut would take the frames off.
func StandardLog(logger *slog.Logger) io.Writer { return standardLog{logger} }

type standardLog struct{ log *slog.Logger }

func (w standardLog) Write(p []byte) (int, error) {
	first, stack := LibraryLines(p)
	if stack == "" {
		w.log.Info(answer.Clip(first))
	} else {
		w.log.Info(answer.Clip(first), "stack", stack)
	}
	return len(p), nil
}
