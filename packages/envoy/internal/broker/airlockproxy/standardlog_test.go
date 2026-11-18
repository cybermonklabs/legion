// packages/envoy/internal/broker/airlockproxy/standardlog_test.go
// The writer the log package writes through once SetDefaultLog has run (standardLog), and the
// placeholder mask's reading of a value a handler writes in a form fmt does not.
package airlockproxy

import (
	"bytes"
	"encoding/json"
	"errors"
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

// marshalErrorQuotes answers MarshalJSON with an error that quotes the placeholder, which slog's
// JSON handler writes after "!ERROR:".
type marshalErrorQuotes struct{}

func (marshalErrorQuotes) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot marshal " + workerPlaceholder)
}

// marshalPanicsWith panics in MarshalJSON with the placeholder, which slog writes after "!PANIC: ".
type marshalPanicsWith struct{}

func (marshalPanicsWith) MarshalJSON() ([]byte, error) { panic(workerPlaceholder) }

// errorPanicsWith is an error whose Error panics with the placeholder.
type errorPanicsWith struct{}

func (errorPanicsWith) Error() string { panic(workerPlaceholder) }

// panicsTwice has a String that panics with a value whose own Error panics: fmt cannot print that
// value and panics again, out of fmt.Sprint, where slog's own recover writes "!PANIC: …".
type panicsTwice struct{}

type errorThatPanics struct{}

func (errorThatPanics) Error() string { panic("an Error that panics") }
func (panicsTwice) String() string    { panic(errorThatPanics{}) }

// errorPanicsTwice is an error whose Error panics with a value whose own Error panics: fmt
// cannot print that value either, and panics again.
type errorPanicsTwice struct{}

func (errorPanicsTwice) Error() string { panic(errorThatPanics{}) }

// marshalErrorPanics answers MarshalJSON with an error whose Error panics twice over: the text
// slog writes for the failure, "!ERROR:" and that error's text, cannot be computed even by fmt,
// which panics again, and the mask must not panic for it (guarded): it writes what slog's own
// recover writes, "!PANIC: " and the second panic's value.
type marshalErrorPanics struct{}

func (marshalErrorPanics) MarshalJSON() ([]byte, error) { return nil, errorPanicsTwice{} }

// marshalErrorPanicsWith answers MarshalJSON with an error whose Error panics with the
// placeholder: slog writes "!ERROR:" and fmt's report of that panic, which quotes it.
type marshalErrorPanicsWith struct{}

func (marshalErrorPanicsWith) MarshalJSON() ([]byte, error) { return nil, errorPanicsWith{} }

// errorPanicsWithAPanickingValue is an error whose Error panics with errorPanicsWith{}, whose own
// Error panics with the placeholder: fmt cannot print the first panic's value and panics again,
// with the placeholder, and slog's own recover writes "!PANIC: " and the placeholder.
type errorPanicsWithAPanickingValue struct{}

func (errorPanicsWithAPanickingValue) Error() string { panic(errorPanicsWith{}) }

// marshalErrorPanicsNested answers MarshalJSON with that error.
type marshalErrorPanicsNested struct{}

func (marshalErrorPanicsNested) MarshalJSON() ([]byte, error) {
	return nil, errorPanicsWithAPanickingValue{}
}

// TestTheMaskWritesWhatSlogWritesForAValueThatPanics: the mask computes a value's forms by
// calling its methods, and a method of a nil receiver, or one that panics of its own, must not
// crash the log call: with SetDefaultLog that call is the whole process's. A nil pointer, a nil
// interface and a typed nil error are written as slog itself writes them, and a value one of
// whose forms panics is written as slog writes it, or, where another form holds the placeholder,
// as that form, masked. Each row is logged through the mask over the JSON and the text handler
// and through each handler alone, and the two lines must be the same, the mask's masked. Where
// the JSON handler alone writes the placeholder - in the text slog writes for a method that
// failed, "!ERROR:" and the error or "!PANIC: " and the panic's value - the mask's line must be
// that line with the placeholder written as placeholderMark, so the mask spells the failure as
// slog does. A row whose placeholder only a later form holds, a String the JSON leaves out, is
// written as that form by the mask, which no JSON line of slog's holds, so it is held to the
// mark alone.
func TestTheMaskWritesWhatSlogWritesForAValueThatPanics(t *testing.T) {
	var nilError error = (*errorThroughNil)(nil)
	spelled := 0
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
		// A method that fails is written as slog writes its failure, "!ERROR:" and the error or
		// "!PANIC: " and the panic's value, which holds the placeholder here: the mask reads that
		// text as a form too.
		{"a MarshalJSON whose error quotes the placeholder", marshalErrorQuotes{}, true},
		{"a MarshalJSON that panics with the placeholder", marshalPanicsWith{}, true},
		{"an Error that panics with the placeholder", errorPanicsWith{}, true},
		// fmt.Sprint itself panics for this value; the mask must read that form under its
		// recover too, and write what slog alone writes.
		{"a String that panics with a value whose Error panics", panicsTwice{}, false},
		// The text of the failure itself panics, and fmt reports the panic in its place, quoting
		// the placeholder: the mask spells that failure as slog does, masked.
		{"a MarshalJSON whose error's Error panics with the placeholder", marshalErrorPanicsWith{}, true},
		// The text of the failure panics so that fmt cannot report it either, and panics again:
		// slog's own recover writes "!PANIC: " and the second panic's value, and so does the mask,
		// masked. With that value the placeholder, the line holds it; with any other, the mask
		// has no form to mask and writes what slog alone writes.
		{"a MarshalJSON whose error's Error panics with a value whose Error panics with the placeholder", marshalErrorPanicsNested{}, true},
		{"a MarshalJSON whose error's Error panics twice over", marshalErrorPanics{}, false},
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
				case c.masked && handler.name == "JSON" && strings.Contains(plain.String(), workerPlaceholder):
					spelled++
					if want := strings.ReplaceAll(plain.String(), workerPlaceholder, placeholderMark); masked.String() != want {
						t.Errorf("the mask over the JSON handler wrote %q; want what the handler alone writes with the placeholder masked, %q", masked.String(), want)
					}
				case !c.masked && masked.String() != plain.String():
					t.Errorf("the mask over the %s handler wrote %q; want what the handler alone writes, %q", handler.name, masked.String(), plain.String())
				}
			}
		})
	}
	// The control: the exact comparison ran for the five rows whose failure text the JSON handler
	// writes with the placeholder in it, so it can fail.
	if spelled != 5 {
		t.Fatalf("the JSON handler alone wrote the placeholder for %d rows; want 5, the rows whose failure text holds it", spelled)
	}
}

// dropTime leaves the time out of a line, so two lines written a moment apart compare equal.
func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
