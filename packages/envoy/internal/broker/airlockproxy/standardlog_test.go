// packages/envoy/internal/broker/airlockproxy/standardlog_test.go
// The writer the log package writes through once SetDefaultLog has run (standardLog), and the
// placeholder mask's reading of a value a handler writes in a form fmt does not.
package airlockproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// errorMarshalsText is an error whose MarshalText holds what its Error does not: the JSON handler
// writes its Error, the text handler its MarshalText.
type errorMarshalsText struct{ held string }

func (errorMarshalsText) Error() string                  { return "an error" }
func (e errorMarshalsText) MarshalText() ([]byte, error) { return []byte(e.held), nil }

// errorFormats is an error whose Format shows, for "%+v", what its Error does not: the text
// handler writes it with "%+v".
type errorFormats struct{ held string }

func (errorFormats) Error() string { return "an error" }
func (e errorFormats) Format(f fmt.State, verb rune) {
	if f.Flag('+') {
		fmt.Fprint(f, e.held)
		return
	}
	fmt.Fprint(f, "an error")
}

// formatsPlus is a value whose Format shows, for "%+v" alone, a field its JSON leaves out.
type formatsPlus struct{ held string }

func (p formatsPlus) Format(f fmt.State, verb rune) {
	if f.Flag('+') {
		fmt.Fprint(f, p.held)
		return
	}
	fmt.Fprint(f, "nothing")
}

// namedBytes is a byte slice of a named type: the text handler writes its bytes as text, the JSON
// handler in base64, and fmt as numbers.
type namedBytes []byte

// appendsText is a value whose AppendText, which the text handler writes before any other form,
// holds what its JSON does not.
type appendsText struct{}

func (appendsText) MarshalJSON() ([]byte, error)        { return []byte(`"nothing"`), nil }
func (appendsText) AppendText(b []byte) ([]byte, error) { return append(b, workerPlaceholder...), nil }

// The four values a review measured: each holds the placeholder and a real value in a form the
// text handler writes and the JSON handler does not. A mask that wrote the first form of a value
// holding a placeholder, whatever the handler behind it, wrote the real value into a JSON line.
type errorQuotesInText struct{ held, real string }

func (errorQuotesInText) Error() string { return "request failed" }
func (e errorQuotesInText) MarshalText() ([]byte, error) {
	return []byte("Bearer " + e.held + " swapped for " + e.real), nil
}

type errorFormatsBoth struct{ held, real string }

func (errorFormatsBoth) Error() string { return "request failed" }
func (e errorFormatsBoth) Format(f fmt.State, verb rune) {
	if f.Flag('+') {
		fmt.Fprintf(f, "request failed: [Bearer %s] upstream [%s]", e.held, e.real)
		return
	}
	fmt.Fprint(f, "request failed")
}

type appendsBoth struct{ held, real string }

func (appendsBoth) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }
func (a appendsBoth) AppendText(b []byte) ([]byte, error) {
	return append(b, a.held+" "+a.real...), nil
}

type hostShowsBoth struct {
	Host       string
	held, real string
}

func (h hostShowsBoth) String() string { return h.Host + " " + h.held + " -> " + h.real }

// realValue is a value a swap puts in, built from parts so that no scanner reads it as a key.
var realValue = strings.Join([]string{"sk", "live", "REAL", "VALUE", "7f3c9a1e5b2d"}, "-")

// TestTheMaskReadsEachFormAHandlerWrites: slog's JSON handler writes a value of kind any as its
// JSON, which follows a pointer, reads a field a String method hides and calls a MarshalJSON,
// and an error that does not marshal itself as its Error; a []byte it writes in base64. Its text
// handler writes the value's AppendText, else its MarshalText, else a byte slice's bytes, its type
// named or not, else fmt's "%+v", which calls a Format, an Error or a String. So one handler's
// form can show what the other's leaves out. The mask writes what the handler behind it writes,
// masked, and nothing else (checkMaskedLine): a placeholder in that handler's form is masked, and
// a value whose placeholder only the other handler's form holds is written as the handler alone
// writes it. The four rows with a real value beside the placeholder hold it where only the text
// handler writes it, so no JSON line may hold it.
func TestTheMaskReadsEachFormAHandlerWrites(t *testing.T) {
	held := workerPlaceholder
	for _, c := range []struct {
		name       string
		value      any
		json, text bool // the JSON or the text handler alone writes the placeholder
	}{
		{"a struct of a string", struct{ F string }{held}, true, true},
		{"a map of a string", map[string]string{"k": held}, true, true},
		// The JSON handler leaves HTML unescaped, and so must the form the mask writes in its place.
		{"a map of a string with HTML in it", map[string]string{"k": "<a> & " + held}, true, true},
		{"a struct of a pointer to a string", pointerField{&held}, true, false},
		{"a map of pointers to strings", map[string]*string{"k": &held}, true, false},
		{"a struct whose String hides its field", stringerHides{held}, true, false},
		{"a struct whose String shows what its JSON leaves out", stringShows{held}, false, true},
		{"an error that marshals itself", errorThatMarshals{held}, true, false},
		{"a []byte", []byte(held), false, true},
		{"a json.RawMessage", json.RawMessage(`"` + held + `"`), true, true},
		{"a slice of strings", []string{held}, true, true},
		{"an error whose MarshalText shows what its Error does not", errorMarshalsText{held}, false, true},
		{"an error whose Format shows what its Error does not", errorFormats{held}, false, true},
		{"a value whose Format shows for %+v what its JSON leaves out", formatsPlus{held}, false, true},
		{"a byte slice of a named type", namedBytes(held), false, true},
		{"a value whose AppendText shows what its JSON does not", appendsText{}, false, true},
		{"an error whose MarshalText holds the placeholder and a real value", errorQuotesInText{held, realValue}, false, true},
		{"an error whose Format holds the placeholder and a real value", errorFormatsBoth{held, realValue}, false, true},
		{"a value whose JSON is fixed and whose AppendText holds the placeholder and a real value", appendsBoth{held, realValue}, false, true},
		{"a struct whose String holds the placeholder and a real value", hostShowsBoth{"api.example.com", held, realValue}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, handler := range maskCases() {
				for _, viaWith := range []bool{false, true} {
					plain, plainPanicked := writeLine(handler.plain, c.value, viaWith)
					masked, maskPanicked := writeLine(handler.masked, c.value, viaWith)
					where := handler.where(viaWith)
					checkPremise(t, where, c.json, c.text, handler.json, plain, plainPanicked)
					checkMaskedLine(t, where, plain, plainPanicked, masked, maskPanicked)
					if strings.Contains(masked, realValue) && !strings.Contains(plain, realValue) {
						t.Errorf("%s: the mask wrote the real value, which the handler alone does not: %q", where, masked)
					}
				}
			}
		})
	}
}

// otherHandler is a handler the mask does not know: it writes through slog's JSON handler, but
// is not one, so the mask cannot know which form of a value it writes. Its WithAttrs and
// WithGroup keep it what it is.
type otherHandler struct{ slog.Handler }

func (h otherHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return otherHandler{h.Handler.WithAttrs(attrs)}
}
func (h otherHandler) WithGroup(name string) slog.Handler {
	return otherHandler{h.Handler.WithGroup(name)}
}

// fmtShows shows the placeholder in fmt's "%v" alone, the text slog's Value.String answers.
type fmtShows struct{ held string }

func (s fmtShows) Format(f fmt.State, verb rune) {
	if f.Flag('+') {
		fmt.Fprint(f, "nothing")
		return
	}
	fmt.Fprint(f, s.held)
}

// TestTheMaskWritesNoFormOverAHandlerItDoesNotKnow: over a handler that is neither slog's JSON
// handler nor its text handler, a value of kind any some form of which - its JSON or an error's
// text, the text handler's form, or fmt's "%v" - holds a placeholder is written as valueHeld, and
// none of its forms is written; the message and a string are masked as over any handler, and a
// value no form of which holds a placeholder is handed on as it is.
func TestTheMaskWritesNoFormOverAHandlerItDoesNotKnow(t *testing.T) {
	held := workerPlaceholder
	var out bytes.Buffer
	logger := slog.New(maskedHandler{otherHandler{slog.NewJSONHandler(&out, &slog.HandlerOptions{ReplaceAttr: dropTime})}})
	logger.With("with", hostShowsBoth{"api.example.com", held, realValue}).Info("a line "+held,
		"string", "a "+held,
		"error", errorQuotesInText{held, realValue},
		"json", struct{ F string }{held},
		"fmt", fmtShows{held},
		"none", hostShowsBoth{Host: "api.example.com"})
	want := `{"level":"INFO","msg":"a line [placeholder]","with":"` + valueHeld + `","string":"a [placeholder]","error":"` + valueHeld +
		`","json":"` + valueHeld + `","fmt":"` + valueHeld + `","none":{"Host":"api.example.com"}}` + "\n"
	if out.String() != want {
		t.Fatalf("the mask over a handler it does not know wrote %q; want %q", out.String(), want)
	}
}

// marshalsThroughNil is a value whose MarshalJSON dereferences a nil pointer, which encoding/json
// panics with again; its String shows the placeholder, which the text handler writes.
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

// nilPanicsWith is an error whose Error, on a nil receiver, panics with the placeholder.
type nilPanicsWith struct{}

func (*nilPanicsWith) Error() string { panic(workerPlaceholder) }

// errorPanicsThreeDeep is an error whose Error panics with errorPanicsWithAPanickingValue{}: its
// placeholder is three panics deep. The JSON handler alone calls Error, and its recover panics
// printing that value, so the log call panics; the text handler alone reaches Error through fmt,
// which reports one panic more, and writes "!PANIC: %!v(PANIC=Error method: " and the
// placeholder.
type errorPanicsThreeDeep struct{}

func (errorPanicsThreeDeep) Error() string { panic(errorPanicsWithAPanickingValue{}) }

// marshalPanicsNested's MarshalJSON panics with errorPanicsWithAPanickingValue{}: the JSON
// handler alone panics, as for errorPanicsThreeDeep, and the text handler alone writes "{}".
type marshalPanicsNested struct{}

func (marshalPanicsNested) MarshalJSON() ([]byte, error) { panic(errorPanicsWithAPanickingValue{}) }

// TestTheMaskWritesWhatSlogWritesForAValueThatPanics: the mask computes a value's form by calling
// its methods, and a method of a nil receiver, or one that panics of its own, must not crash the
// log call: with SetDefaultLog that call is the whole process's. The mask writes what the
// handler behind it writes, masked (checkMaskedLine): a nil pointer, a nil interface and a typed
// nil error as slog itself writes them, and a value whose form fails as slog writes that failure,
// "!ERROR:" and the error or "!PANIC: " and the panic's value, with the placeholder in it masked.
// A handler alone may panic, for a value whose failure it cannot print: the mask then writes the
// line, masked. Each row says which handler alone writes the placeholder or panics, and the row
// fails when that premise does not hold, so neither half of checkMaskedLine goes unexercised.
func TestTheMaskWritesWhatSlogWritesForAValueThatPanics(t *testing.T) {
	var nilError error = (*errorThroughNil)(nil)
	for _, c := range []struct {
		name       string
		value      any
		json, text bool // the JSON or the text handler alone writes the placeholder, or panics
	}{
		{"a nil *netip.Addr", (*netip.Addr)(nil), false, false},
		{"a nil *time.Time", (*time.Time)(nil), false, false},
		{"a nil interface", nil, false, false},
		{"a typed nil error", nilError, false, false},
		// slog's recover writes a nil pointer whose method panicked as "<nil>", whatever the panic
		// held, and fmt does the same: the mask writes no "!PANIC: " for it either.
		{"a nil pointer whose Error panics with the placeholder", (*nilPanicsWith)(nil), false, false},
		{"a value whose MarshalJSON dereferences nil", marshalsThroughNilQuietly{}, false, false},
		// The JSON handler writes the panic of the MarshalJSON; the text handler, the String.
		{"a value whose MarshalJSON dereferences nil and whose String holds the placeholder", marshalsThroughNil{}, false, true},
		// A nil receiver whose String does not dereference it shows text all the same, which the
		// text handler writes: a nil value is read as any other, not given a form of its own.
		{"a nil pointer whose String holds the placeholder", (*nilShows)(nil), false, true},
		// A method that fails is written as slog writes its failure, "!ERROR:" and the error or
		// "!PANIC: " and the panic's value, which holds the placeholder here: the mask reads that
		// text as the form. The text handler writes these values with fmt, as "{}".
		{"a MarshalJSON whose error quotes the placeholder", marshalErrorQuotes{}, true, false},
		{"a MarshalJSON that panics with the placeholder", marshalPanicsWith{}, true, false},
		// fmt reports the panic of an Error it calls in its place, quoting the placeholder.
		{"an Error that panics with the placeholder", errorPanicsWith{}, true, true},
		// fmt.Sprint itself panics for this value; the mask must read that form under its
		// recover too, and write what slog alone writes.
		{"a String that panics with a value whose Error panics", panicsTwice{}, false, false},
		// The text of the failure itself panics, and fmt reports the panic in its place, quoting
		// the placeholder: the mask spells that failure as slog does, masked.
		{"a MarshalJSON whose error's Error panics with the placeholder", marshalErrorPanicsWith{}, true, false},
		// The text of the failure panics so that fmt cannot report it either, and panics again:
		// slog's own recover writes "!PANIC: " and the second panic's value, and so does the mask,
		// masked. With that value the placeholder, the line holds it; with any other, the mask
		// has no form to mask and writes what slog alone writes.
		{"a MarshalJSON whose error's Error panics with a value whose Error panics with the placeholder", marshalErrorPanicsNested{}, true, false},
		{"a MarshalJSON whose error's Error panics twice over", marshalErrorPanics{}, false, false},
		// The placeholder three panics deep: the JSON handler alone panics, and the text handler
		// alone writes it inside fmt's report.
		{"an Error that panics three deep with the placeholder at the bottom", errorPanicsThreeDeep{}, true, true},
		// A MarshalJSON's panic whose value fmt cannot print, panicking again with the
		// placeholder: the JSON handler alone panics, and the mask reads "!PANIC: " and that
		// second panic's value and writes it masked. The text handler writes "{}".
		{"a MarshalJSON that panics with a value whose printing panics with the placeholder", marshalPanicsNested{}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, handler := range maskCases() {
				for _, viaWith := range []bool{false, true} {
					plain, plainPanicked := writeLine(handler.plain, c.value, viaWith)
					masked, maskPanicked := writeLine(handler.masked, c.value, viaWith)
					where := handler.where(viaWith)
					checkPremise(t, where, c.json, c.text, handler.json, plain, plainPanicked)
					checkMaskedLine(t, where, plain, plainPanicked, masked, maskPanicked)
				}
			}
		})
	}
}

// maskCase is a handler the mask tests write through: plain is the handler alone, and masked the
// same handler behind the mask, or behind two, as the airlock's lines pass a mask of their own
// and the default logger's (New, SetDefaultLog).
type maskCase struct {
	name          string
	plain, masked func(*bytes.Buffer) slog.Handler
	json          bool // the handler alone is slog's JSON handler, and not its text handler
}

func maskCases() []maskCase {
	jsonHandler := func(b *bytes.Buffer) slog.Handler {
		return slog.NewJSONHandler(b, &slog.HandlerOptions{ReplaceAttr: dropTime})
	}
	textHandler := func(b *bytes.Buffer) slog.Handler {
		return slog.NewTextHandler(b, &slog.HandlerOptions{ReplaceAttr: dropTime})
	}
	return []maskCase{
		{"JSON", jsonHandler, func(b *bytes.Buffer) slog.Handler { return maskedHandler{jsonHandler(b)} }, true},
		{"text", textHandler, func(b *bytes.Buffer) slog.Handler { return maskedHandler{textHandler(b)} }, false},
		{"JSON behind two masks", jsonHandler, func(b *bytes.Buffer) slog.Handler { return maskedHandler{maskedHandler{jsonHandler(b)}} }, true},
	}
}

// where names a case's handler and how the value reached it, for a failure's text.
func (c maskCase) where(viaWith bool) string {
	if viaWith {
		return "over the " + c.name + " handler, as an attribute of a logger made With it"
	}
	return "over the " + c.name + " handler"
}

// writeLine is the line one call logs through the handler handler makes, on a buffer of its own:
// value as an attribute of the call, or with viaWith of a logger made With it; and the value the
// call panicked with, if it did.
func writeLine(handler func(*bytes.Buffer) slog.Handler, value any, viaWith bool) (string, any) {
	var out bytes.Buffer
	logger := slog.New(handler(&out))
	panicked := func() (panicked any) {
		defer func() { panicked = recover() }()
		if viaWith {
			logger.With("value", value).Info("a line")
		} else {
			logger.Info("a line", "value", value)
		}
		return nil
	}()
	return out.String(), panicked
}

// checkPremise fails t when a row's premise is false: that the JSON handler alone (jsonWrites), or
// the text handler alone (textWrites), writes the placeholder for the row's value or panics.
func checkPremise(t *testing.T, where string, jsonWrites, textWrites, overJSON bool, plain string, plainPanicked any) {
	t.Helper()
	want := textWrites
	if overJSON {
		want = jsonWrites
	}
	if writes := strings.Contains(plain, workerPlaceholder) || plainPanicked != nil; writes != want {
		t.Errorf("%s: the handler alone writes the placeholder or panics = %v; the row says %v (it wrote %q, panicked %v)", where, writes, want, plain, plainPanicked)
	}
}

// checkMaskedLine is the mask's one claim, checked for one line: the mask writes what the handler
// behind it writes alone, with each placeholder written as placeholderMark, and nothing else - no
// form of a value that handler does not write. Where the handler alone panics, for a value whose
// failure it cannot print, the mask writes a line, masked, and does not panic.
func checkMaskedLine(t *testing.T, where, plain string, plainPanicked any, masked string, maskPanicked any) {
	t.Helper()
	switch {
	case maskPanicked != nil:
		t.Errorf("%s: the mask panicked: %v", where, maskPanicked)
	case strings.Contains(strings.ToLower(masked), strings.ToLower(workerPlaceholder)):
		t.Errorf("%s: the mask wrote the placeholder: %q", where, masked)
	case plainPanicked != nil:
		if !strings.Contains(masked, placeholderMark) {
			t.Errorf("%s: the handler alone panicked, and the mask wrote no mark: %q", where, masked)
		}
	default:
		if want := strings.ReplaceAll(plain, workerPlaceholder, placeholderMark); masked != want {
			t.Errorf("%s: the mask wrote %q; want what the handler alone writes, with the placeholder masked, %q", where, masked, want)
		}
	}
}

// dropTime leaves the time out of a line, so two lines written a moment apart compare equal.
func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
