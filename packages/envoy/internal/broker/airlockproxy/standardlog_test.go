// packages/envoy/internal/broker/airlockproxy/standardlog_test.go
// The writer the log package writes through once SetDefaultLog has run (standardLog), and the
// placeholder mask's reading of a value a handler writes in a form fmt does not.
package airlockproxy

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/logtest"
)

// jsonLines is each line of a JSON handler's output, decoded.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(out), "\n") {
		if text == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("a line the handler wrote is no JSON object: %q: %v", text, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// setDefaultLogFor is SetDefaultLog into a JSON handler of the row's own, with the process's
// default logger and the log package's output and flags put back when the row ends
// (logtest.Swap).
func setDefaultLogFor(t *testing.T) *logtest.Buffer {
	t.Helper()
	out := &logtest.Buffer{}
	logtest.Swap(t, slog.NewJSONHandler(out, nil))
	SetDefaultLog(slog.NewJSONHandler(out, nil))
	return out
}

// TestTheStandardLoggerCutsSplitsAndMasksALibrarysLine: a line a library writes with the log
// package is one INFO line of the default logger, with no date or time of the log package's own
// before it. Its first line is the message, cut to answer.FieldBytes, and every line after it is
// the stack, whole. A placeholder in either is masked, and each run of bytes the library quotes
// from a peer is written as peerBytesShown, whatever it held, a real value among it.
func TestTheStandardLoggerCutsSplitsAndMasksALibrarysLine(t *testing.T) {
	out := setDefaultLogFor(t)
	first := workerPlaceholder + " " + strings.Repeat("x", 300)
	log.Print(first + "\nsecond line " + workerPlaceholder + "\nthird line")
	log.Printf("Unsolicited response received on idle HTTP channel starting with %q; err=%v", "echo Bearer "+workerValue, nil)
	slog.Info("a line of the default logger", "planted", workerPlaceholder)

	lines := jsonLines(t, out.String())
	if len(lines) != 3 {
		t.Fatalf("the handler wrote %d lines; want three: %q", len(lines), out.String())
	}
	if written := out.String(); strings.Contains(strings.ToLower(written), strings.ToLower(workerPlaceholder)) || strings.Contains(written, workerValue) {
		t.Fatalf("a line holds the placeholder or the real value: %q", written)
	}
	cut := placeholderMark + answer.Clip(first)[len(workerPlaceholder):]
	if got := lines[0]["msg"]; got != cut {
		t.Errorf("the library line's message = %q; want its first line cut to %d bytes and masked, %q, with no time before it", got, answer.FieldBytes, cut)
	}
	if got := lines[0]["stack"]; got != "second line "+placeholderMark+"\nthird line" {
		t.Errorf("the library line's stack = %q; want every line after the first, whole and masked", got)
	}
	if lines[0]["level"] != "INFO" {
		t.Errorf("the library line's level = %v; want INFO", lines[0]["level"])
	}
	want := "Unsolicited response received on idle HTTP channel starting with " + peerBytesShown + "; err=<nil>"
	if got := lines[1]["msg"]; got != want {
		t.Errorf("the quoting library line's message = %q; want %q", got, want)
	}
	if lines[2]["msg"] != "a line of the default logger" || lines[2]["planted"] != placeholderMark {
		t.Errorf("the default logger's line = %v; want its own message and the attribute masked", lines[2])
	}
}

type pointerField struct{ P *string }

type stringerHides struct{ F string }

func (stringerHides) String() string { return "nothing to see" }

type errorThatMarshals struct{ held string }

func (errorThatMarshals) Error() string                  { return "an error" }
func (e errorThatMarshals) MarshalJSON() ([]byte, error) { return json.Marshal(e.held) }

// stringShows holds the placeholder in a field the JSON leaves out, and shows it in its String:
// only a later form than the JSON, the one the text handler writes, holds it.
type stringShows struct{ held string }

func (s stringShows) String() string { return s.held }

// TestTheMaskReadsEachFormAHandlerWrites: the JSON handler writes a value of kind any as its JSON
// and fmt does not. It follows a pointer fmt prints as an address, it reads a field a String
// method hides, and it writes an error that marshals itself as that JSON; a []byte it writes in
// base64, which the text handler writes as text; and a String method can show what the JSON
// leaves out. A placeholder in any of those forms is masked in the form the line is written in.
func TestTheMaskReadsEachFormAHandlerWrites(t *testing.T) {
	held := workerPlaceholder
	for _, c := range []struct {
		name  string
		value any
	}{
		{"a struct of a string", struct{ F string }{held}},
		{"a map of a string", map[string]string{"k": held}},
		{"a struct of a pointer to a string", pointerField{&held}},
		{"a map of pointers to strings", map[string]*string{"k": &held}},
		{"a struct whose String hides its field", stringerHides{held}},
		{"a struct whose String shows what its JSON leaves out", stringShows{held}},
		{"an error that marshals itself", errorThatMarshals{held}},
		{"a []byte", []byte(held)},
		{"a json.RawMessage", json.RawMessage(`"` + held + `"`)},
		{"a slice of strings", []string{held}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, handler := range []struct {
				name string
				new  func(*bytes.Buffer) slog.Handler
			}{
				{"JSON", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
				{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
			} {
				var out bytes.Buffer
				slog.New(maskedHandler{handler.new(&out)}).Info("a line", "value", c.value)
				written := out.String()
				if strings.Contains(strings.ToLower(written), strings.ToLower(held)) {
					t.Errorf("the %s handler wrote the placeholder: %q", handler.name, written)
				}
				if !strings.Contains(written, placeholderMark) {
					t.Errorf("the %s handler wrote no mark where the placeholder was: %q", handler.name, written)
				}
			}
		})
	}
}

// marshalsThroughNil is a value whose MarshalJSON dereferences a nil pointer, which encoding/json
// panics with again; its String shows the placeholder, so the mask has a form to write.
type marshalsThroughNil struct{ missing *string }

func (m marshalsThroughNil) MarshalJSON() ([]byte, error) { return json.Marshal(*m.missing) }
func (marshalsThroughNil) String() string                 { return workerPlaceholder }

// marshalsThroughNilQuietly is the same with no String: no form of it holds a placeholder.
type marshalsThroughNilQuietly struct{ missing *string }

func (m marshalsThroughNilQuietly) MarshalJSON() ([]byte, error) { return json.Marshal(*m.missing) }

// errorThroughNil is an error whose Error dereferences its receiver.
type errorThroughNil struct{ text string }

func (e *errorThroughNil) Error() string { return e.text }

// nilShows has a String that reads nothing of its receiver, so a nil *nilShows shows the
// placeholder as any other does.
type nilShows struct{}

func (*nilShows) String() string { return workerPlaceholder }

// TestTheMaskWritesWhatSlogWritesForAValueThatPanics: the mask computes a value's forms by
// calling its methods, and a method of a nil receiver, or one that panics of its own, must not
// crash the log call: with SetDefaultLog that call is the whole process's. A nil pointer, a nil
// interface and a typed nil error are written as slog itself writes them, and a value one of
// whose forms panics is written as slog writes it, or, where another form holds the placeholder,
// as that form, masked. Each row is logged through the mask over the JSON and the text handler
// and through each handler alone, and the two lines must be the same, the mask's masked.
func TestTheMaskWritesWhatSlogWritesForAValueThatPanics(t *testing.T) {
	var nilError error = (*errorThroughNil)(nil)
	for _, c := range []struct {
		name   string
		value  any
		masked bool
	}{
		{"a nil *netip.Addr", (*netip.Addr)(nil), false},
		{"a nil *time.Time", (*time.Time)(nil), false},
		{"a nil interface", nil, false},
		{"a typed nil error", nilError, false},
		{"a value whose MarshalJSON dereferences nil", marshalsThroughNilQuietly{}, false},
		{"a value whose MarshalJSON dereferences nil and whose String holds the placeholder", marshalsThroughNil{}, true},
		// A nil receiver whose String does not dereference it shows text all the same, which the
		// text handler writes: a nil value is read as any other, not given a form of its own.
		{"a nil pointer whose String holds the placeholder", (*nilShows)(nil), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, handler := range []struct {
				name string
				new  func(*bytes.Buffer) slog.Handler
			}{
				{"JSON", func(b *bytes.Buffer) slog.Handler {
					return slog.NewJSONHandler(b, &slog.HandlerOptions{ReplaceAttr: dropTime})
				}},
				{"text", func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{ReplaceAttr: dropTime})
				}},
			} {
				var plain, masked bytes.Buffer
				slog.New(handler.new(&plain)).Info("a line", "value", c.value)
				func() {
					defer func() {
						if panicked := recover(); panicked != nil {
							t.Fatalf("the mask over the %s handler panicked: %v", handler.name, panicked)
						}
					}()
					slog.New(maskedHandler{handler.new(&masked)}).Info("a line", "value", c.value)
				}()
				switch {
				case strings.Contains(strings.ToLower(masked.String()), strings.ToLower(workerPlaceholder)):
					t.Errorf("the mask over the %s handler wrote the placeholder: %q", handler.name, masked.String())
				case c.masked && !strings.Contains(masked.String(), placeholderMark):
					t.Errorf("the mask over the %s handler wrote no mark where the placeholder was: %q", handler.name, masked.String())
				case !c.masked && masked.String() != plain.String():
					t.Errorf("the mask over the %s handler wrote %q; want what the handler alone writes, %q", handler.name, masked.String(), plain.String())
				}
			}
		})
	}
}

// dropTime leaves the time out of a line, so two lines written a moment apart compare equal.
func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
