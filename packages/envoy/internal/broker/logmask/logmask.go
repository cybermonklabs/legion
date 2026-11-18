// Package logmask is what keeps a placeholder, and the bytes a peer sent, off the broker's log.
//
// Much of what a line names is a command's own text - the method, the path, the host and port it
// asked for, a reason or an error that quotes them, a refusal's correlation id from
// X-Correlation-Id, the name in a request's path, the host of a CONNECT whose authority is its
// Host header - and a command can write its own placeholder into any of it. A cut to 256 bytes
// keeps a line small and lowercasing a host keeps it one spelling; neither keeps a placeholder
// out. So the broker writes every line through Writer, under its JSON handler (cmd/broker's
// setUpLogging, logging.NewCloudHandler), which writes each placeholder in the line as Mark.
//
// The mask reads the line the handler wrote, not the values it was given: slog's JSON handler
// writes each record as one JSON object in one Write, under its own lock. The mask decodes each
// string of that object - each key, a group's name among them, and each string value, which is
// how the handler writes a message, an error, a value's MarshalJSON, MarshalText or fmt form, and
// the text slog writes for a value whose method failed - and writes again, with the placeholder
// as Mark, only a string whose decoded text holds one, leaving every other byte of the line as
// the handler wrote it. So it reads a value once, as written, after any ReplaceAttr; it calls no
// method of a value, and cannot panic for one; a placeholder hidden in a JSON escape is masked; a
// caller's text changes no token but its own, and the line stays one JSON object. What it cannot
// see is a placeholder the handler writes in another spelling: a []byte, which the JSON handler
// writes in base64 at any depth, a placeholder a value writes percent-escaped or split, and the
// 32 characters after the prefix written without it. The broker logs no such value.
//
// A real value the swap put in is no placeholder, and the mask cannot see it. What keeps it off
// a line is that no line reads a request's headers or body, and that each run of bytes a library
// quotes from a peer, which can hold it, is written as PeerBytesShown (HidePeerBytes).
package logmask

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// Mark is what a line writes in place of a placeholder.
const Mark = "[placeholder]"

// NotJSON is the line the mask writes in place of a line that is no JSON object: one it cannot
// read is one it cannot mask, so none of it is written.
const NotJSON = `{"severity":"ERROR","message":"broker: a log line that was no JSON object was not written"}`

// placeholder is a placeholder written whole, in any case: the prefix every placeholder the broker
// mints starts with (requests.PlaceholderPrefix) and the run of letters and digits after it.
var placeholder = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(requests.PlaceholderPrefix) + `[0-9a-z]*`)

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

// maskLines is p with each of its lines masked (maskLine), and true where that changed nothing. A
// Write that changed ends in a newline, so a line written as NotJSON is a line of its own.
func maskLines(p []byte) ([]byte, bool) {
	if len(p) == 0 {
		return p, true
	}
	var out []byte
	same := true
	for i, line := range bytes.Split(bytes.TrimSuffix(p, []byte("\n")), []byte("\n")) {
		masked, kept := maskLine(line)
		same = same && kept
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, masked...)
	}
	if same {
		return p, true
	}
	return append(out, '\n'), false
}

// maskLine is line, one JSON object, with each string whose decoded text holds a placeholder
// written again with Mark in its place and every other byte as it came, and true where no string
// did; a line that is no JSON object is NotJSON.
func maskLine(line []byte) ([]byte, bool) {
	if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(line) {
		return []byte(NotJSON), false
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
// holds as Mark, HTML left unescaped as slog's handler leaves it, and false for a string that
// holds none. A string with no escape is its own text.
func maskString(token []byte) ([]byte, bool) {
	text := string(token[1 : len(token)-1])
	if bytes.IndexByte(token, '\\') >= 0 {
		if err := json.Unmarshal(token, &text); err != nil {
			return nil, false
		}
	}
	if !placeholder.MatchString(text) {
		return nil, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(placeholder.ReplaceAllString(text, Mark)); err != nil {
		return []byte(`"` + Mark + `"`), true
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), true
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
// first line and the rest, its stack, "" for a write of one line. net/http's servers and httputil
// write through the proxy's ErrorLog (airlockproxy's slogWriter), and the log package through
// StandardLog; both read a write here.
func LibraryLines(p []byte) (first, stack string) {
	first, stack, _ = strings.Cut(HidePeerBytes(strings.TrimSuffix(string(p), "\n")), "\n")
	return first, stack
}

// StandardLog is the log package's output once the broker has set up its logging: one line of
// logger at INFO for each write, as slog.SetDefault's own writer makes one. Every write is a
// library's text (LibraryLines). The line's message is the first line cut to answer.FieldBytes
// (answer.Clip), since a library's first line can repeat what a client or a service sent, and its
// stack is every line after it, whole: a recovered panic's stack, which a server with no
// ErrorLog of its own writes here and which the cut would take the frames off.
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
