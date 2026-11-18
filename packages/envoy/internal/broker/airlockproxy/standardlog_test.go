// packages/envoy/internal/broker/airlockproxy/standardlog_test.go
// The writer the log package writes through once SetDefaultLog has run (standardLog), and the
// placeholder mask's reading of a value a handler writes in a form fmt does not.
package airlockproxy

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"

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

// TestTheMaskReadsEachFormAHandlerWrites: the JSON handler writes a value of kind any as its JSON
// and fmt does not. It follows a pointer fmt prints as an address, it reads a field a String
// method hides, and it writes an error that marshals itself as that JSON; a []byte it writes in
// base64, which the text handler writes as text. A placeholder in any of those forms is masked in
// the form the line is written in.
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
