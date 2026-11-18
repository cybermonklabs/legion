package logmask

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logging"
)

// held is a placeholder as the broker mints one, and realValue a value a swap puts in, built from
// parts so that no scanner reads it as a key.
var (
	held      = requests.PlaceholderPrefix + "AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPp"
	realValue = strings.Join([]string{"sk", "live", "REAL", "VALUE", "7f3c9a1e5b2d"}, "-")
	at        = time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)
	// heldAnywhere is the test's own reading of a placeholder, in any case, so a check of the
	// mask does not lean on the mask's own pattern.
	heldAnywhere = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(requests.PlaceholderPrefix) + `[0-9a-z]*`)
)

// lineOf is the line handler writes, on a buffer of its own, for one record with message and
// attrs, written through with: the handler a row gets from WithAttrs or WithGroup, or the
// handler itself. A record is made at one time, so two lines of it compare byte for byte.
func lineOf(t *testing.T, handler slog.Handler, with func(slog.Handler) slog.Handler, message string, attrs ...any) string {
	t.Helper()
	if with != nil {
		handler = with(handler)
	}
	record := slog.NewRecord(at, slog.LevelWarn, message, 0)
	record.Add(attrs...)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("the handler refused the record: %v", err)
	}
	return ""
}

// brokerLines is the line the broker's own handler (logging.NewCloudHandler) writes for one
// record, alone and through the mask, each on a buffer of its own.
func brokerLines(t *testing.T, with func(slog.Handler) slog.Handler, message string, attrs ...any) (plain, masked string) {
	t.Helper()
	var alone, through bytes.Buffer
	lineOf(t, logging.NewCloudHandler(&alone), with, message, attrs...)
	lineOf(t, logging.NewCloudHandler(Writer(&through)), with, message, attrs...)
	return alone.String(), through.String()
}

// span is one token of a JSON line as it was written: the bytes from the end of the token before
// it to its own end, separators included, and for a string its decoded text.
type span struct {
	raw      string
	text     string
	isString bool
}

// spans is line, one JSON object, as its tokens, failing t when it is not one.
func spans(t *testing.T, line string) []span {
	t.Helper()
	line = strings.TrimSuffix(line, "\n")
	var object map[string]any
	if err := json.Unmarshal([]byte(line), &object); err != nil || object == nil {
		t.Fatalf("the line is no JSON object: %q: %v", line, err)
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	var out []span
	previous := int64(0)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the line %q does not read as JSON tokens: %v", line, err)
		}
		offset := decoder.InputOffset()
		s := span{raw: line[previous:offset]}
		s.text, s.isString = token.(string)
		out = append(out, s)
		previous = offset
	}
	return out
}

// checkMasked is the mask's claim for one line, against the line the handler wrote alone: the
// masked line is one JSON object; it holds no placeholder, decoded, in any case; and it is the
// handler's line byte for byte but for each string whose decoded text holds a placeholder, which
// it writes with logmask.Mark in the placeholder's place. So a caller's text changes no token but
// its own. It answers how many strings the mask wrote again.
func checkMasked(t *testing.T, plain, masked string) int {
	t.Helper()
	before, after := spans(t, plain), spans(t, masked)
	if len(before) != len(after) {
		t.Fatalf("the masked line has %d tokens and the handler's %d: %q, %q", len(after), len(before), masked, plain)
	}
	rewritten := 0
	for i := range before {
		switch {
		case after[i].isString && heldAnywhere.MatchString(after[i].text):
			t.Errorf("the masked line holds a placeholder in %q: %q", after[i].raw, masked)
		case before[i].isString && heldAnywhere.MatchString(before[i].text):
			rewritten++
			want := heldAnywhere.ReplaceAllString(before[i].text, Mark)
			if !after[i].isString || after[i].text != want {
				t.Errorf("the masked line writes %q where the handler wrote %q; want the string %q", after[i].raw, before[i].raw, want)
			}
			// What comes before a string's opening quotation mark - a separator, a brace - is
			// the handler's own, and the string is JSON with HTML left unescaped, as slog's
			// handler writes a string.
			sep := before[i].raw[:strings.IndexByte(before[i].raw, '"')]
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(false)
			encoder.Encode(want)
			if exact := sep + strings.TrimSuffix(encoded.String(), "\n"); after[i].raw != exact {
				t.Errorf("the masked line writes %q where the handler wrote %q; want %q", after[i].raw, before[i].raw, exact)
			}
		case before[i].raw != after[i].raw:
			t.Errorf("the masked line writes %q where the handler wrote %q, which holds no placeholder: %q, %q", after[i].raw, before[i].raw, masked, plain)
		}
	}
	if rewritten == 0 && masked != plain {
		t.Errorf("the line holds no placeholder, and the mask changed it: %q; want %q", masked, plain)
	}
	return rewritten
}

// TestSlogWritesEachRecordInOneWrite: the mask reads each Write on its own (Writer), so a record
// must reach it in one Write. slog's JSON handler writes a record of many attributes, groups, a
// logger's own attributes and a value of 64 KiB in one Write each, one line ending in its one
// newline. The control: a writer that splits each Write in two fails the same check, and through
// the mask each half is no JSON object and is written as NotJSON, not as the half it was.
func TestSlogWritesEachRecordInOneWrite(t *testing.T) {
	big := strings.Repeat("x", 64<<10)
	write := func(w io.Writer) int {
		logger := slog.New(logging.NewCloudHandler(w)).With("with", held).WithGroup("group")
		logger.Info("one", "k", "v")
		logger.Warn("two", "a", 1, "b", slog.GroupValue(slog.String("c", held)), "big", big)
		logger.Error("three", "err", errors.New("an\nerror with a newline"))
		return 3
	}
	check := func(writes [][]byte, records int) error {
		if len(writes) != records {
			return fmt.Errorf("%d records came in %d writes", records, len(writes))
		}
		for _, w := range writes {
			if bytes.Count(w, []byte("\n")) != 1 || !bytes.HasSuffix(w, []byte("\n")) || !json.Valid(bytes.TrimSuffix(w, []byte("\n"))) {
				return fmt.Errorf("a write is not one JSON line: %.200q", w)
			}
		}
		return nil
	}
	var writes recordedWrites
	if err := check(writes.of(write), 3); err != nil {
		t.Fatalf("slog's JSON handler: %v", err)
	}
	t.Run("the control: a record in two writes", func(t *testing.T) {
		var halves recordedWrites
		split := splitWriter{&halves}
		records := write(split)
		if err := check(halves.writes, records); err == nil {
			t.Fatal("the check passed writes that each held half a record")
		}
		var out bytes.Buffer
		write(splitWriter{Writer(&out)})
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if line != NotJSON {
				t.Fatalf("the mask wrote half a record as %.200q; want %s", line, NotJSON)
			}
		}
	})
}

type recordedWrites struct{ writes [][]byte }

func (r *recordedWrites) Write(p []byte) (int, error) {
	r.writes = append(r.writes, bytes.Clone(p))
	return len(p), nil
}

func (r *recordedWrites) of(write func(io.Writer) int) [][]byte {
	write(r)
	return r.writes
}

// splitWriter writes each Write to w as two.
type splitWriter struct{ w io.Writer }

func (s splitWriter) Write(p []byte) (int, error) {
	half := len(p) / 2
	if _, err := s.w.Write(p[:half]); err != nil {
		return 0, err
	}
	if _, err := s.w.Write(p[half:]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// The kinds of value a placeholder can reach a line in.
type (
	stringsField struct{ Host, Path string }
	jsonHolds    struct{}
	escapedJSON  struct{}
	textHolds    struct{}
	appendsHeld  struct{}
	formatsHeld  struct{}
	stringerHeld struct{ Host string }
	valuerHeld   struct{}
	// valuerOnce answers clean on its first LogValue and the placeholder on every later one: a
	// mask that read a value and handed the handler the value again would see the clean form and
	// let the written one through.
	valuerOnce       struct{ calls *int }
	marshalErrHeld   struct{}
	marshalPanicHeld struct{}
	errorPanicHeld   struct{}
)

func (jsonHolds) MarshalJSON() ([]byte, error) { return json.Marshal("json " + held) }
func (escapedJSON) MarshalJSON() ([]byte, error) {
	return []byte(`"\u0061gent-secrets-placeholder-EscapedInJSON"`), nil
}
func (textHolds) MarshalText() ([]byte, error) { return []byte("text " + held), nil }
func (appendsHeld) AppendText(b []byte) ([]byte, error) {
	return append(b, "appended "+held...), nil
}
func (formatsHeld) Format(f fmt.State, verb rune) { fmt.Fprint(f, "formatted "+held) }
func (s stringerHeld) String() string             { return "stringer " + held }
func (valuerHeld) LogValue() slog.Value           { return slog.StringValue("valued " + held) }
func (v valuerOnce) LogValue() slog.Value {
	*v.calls++
	if *v.calls == 1 {
		return slog.StringValue("clean")
	}
	return slog.StringValue(held)
}
func (marshalErrHeld) MarshalJSON() ([]byte, error)   { return nil, errors.New("refused " + held) }
func (marshalPanicHeld) MarshalJSON() ([]byte, error) { panic(held) }
func (errorPanicHeld) Error() string                  { panic(held) }

// TestTheMaskMasksEveryPlaceAPlaceholderReachesALine is the control of the mask: the placeholder
// planted in each place it can reach a line of the broker's own handler - the message; a string
// value; a key; a group's name, from WithGroup and from slog.Group; a group's member, nested; a
// logger's own attribute, its key and its value; an error; a Stringer and a Format, as a caller
// formats them into a string, and a Stringer as the JSON handler writes it, its fields; a value's
// MarshalJSON, its MarshalJSON hiding the placeholder in a JSON escape, its MarshalText and its
// AppendText; a LogValuer; the text slog writes for a MarshalJSON that failed or panicked and for
// an Error that panicked; the placeholder in upper case; and a key that holds it after a control
// byte the handler escapes. Each row's premise is that the handler alone writes it (a row whose
// handler's line holds no placeholder would test nothing), and each masked line is the handler's
// own with that string, and that string alone, written again (checkMasked).
func TestTheMaskMasksEveryPlaceAPlaceholderReachesALine(t *testing.T) {
	withGroup := func(name string) func(slog.Handler) slog.Handler {
		return func(h slog.Handler) slog.Handler { return h.WithGroup(name) }
	}
	withAttrs := func(attrs ...slog.Attr) func(slog.Handler) slog.Handler {
		return func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) }
	}
	for _, c := range []struct {
		name    string
		with    func(slog.Handler) slog.Handler
		message string
		attrs   []any
	}{
		{"the message", nil, "a line naming " + held, nil},
		{"a string value", nil, "a line", []any{"host", held + ".example.net"}},
		{"a key", nil, "a line", []any{held, "v"}},
		{"a group's name, from WithGroup", withGroup("g " + held), "a line", []any{"k", "v"}},
		{"a group's name, from slog.Group", nil, "a line", []any{slog.Group(held, "k", "v")}},
		{"a group's member", nil, "a line", []any{slog.Group("g", "k", held)}},
		{"a nested group's member", nil, "a line", []any{slog.Group("g", slog.Group("h", "k", held))}},
		{"a logger's own attribute", withAttrs(slog.String("with", held)), "a line", []any{"k", "v"}},
		{"a logger's own attribute's key", withAttrs(slog.String(held, "v")), "a line", nil},
		{"an attribute under WithGroup", withGroup("g"), "a line", []any{"k", held}},
		{"an error", nil, "a line", []any{"error", errors.New("dial " + held + ": refused")}},
		{"a Stringer, as a caller formats it", nil, "a line", []any{"text", fmt.Sprint(stringerHeld{})}},
		{"a Stringer, as the JSON handler writes it", nil, "a line", []any{"value", stringerHeld{Host: held}}},
		{"a Format, as a caller formats it", nil, "a line", []any{"text", fmt.Sprintf("%+v", formatsHeld{})}},
		{"a struct of strings", nil, "a line", []any{"value", stringsField{Host: "api.example.com", Path: "/v1/" + held}}},
		{"a MarshalJSON", nil, "a line", []any{"value", jsonHolds{}}},
		{"a MarshalJSON hiding it in a JSON escape", nil, "a line", []any{"value", escapedJSON{}}},
		{"a json.RawMessage hiding it in a JSON escape", nil, "a line", []any{"value", json.RawMessage(`{"k":"\u0061gent-secrets-placeholder-Raw"}`)}},
		{"a MarshalText", nil, "a line", []any{"value", textHolds{}}},
		{"an AppendText", nil, "a line", []any{"value", appendsHeld{}}},
		{"a LogValuer", nil, "a line", []any{"value", valuerHeld{}}},
		{"a list of names", nil, "a line", []any{"secrets", []string{"DEEL_API_KEY", held}}},
		{"a MarshalJSON's error", nil, "a line", []any{"value", marshalErrHeld{}}},
		{"a MarshalJSON's panic", nil, "a line", []any{"value", marshalPanicHeld{}}},
		{"an Error's panic", nil, "a line", []any{"value", errorPanicHeld{}}},
		{"the placeholder in upper case", nil, "a line", []any{"host", strings.ToUpper(held)}},
		{"a key holding it after a control byte", nil, "a line", []any{"\x01" + held, "v"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain, masked := brokerLines(t, c.with, c.message, c.attrs...)
			if !heldAnywhere.MatchString(decodedText(t, plain)) {
				t.Fatalf("the premise: the handler's own line holds no placeholder, so the row tests nothing: %q", plain)
			}
			if rewritten := checkMasked(t, plain, masked); rewritten == 0 {
				t.Errorf("the mask wrote no string again for a line that holds the placeholder: %q", masked)
			}
		})
	}
	// A value read once: a LogValuer answers clean the first time and the placeholder after.
	// The handler reads it once and the mask reads the line the handler wrote, never the value,
	// so the line through the mask holds the clean answer, as the same value's line alone does;
	// a mask that read the value before handing it on would have read it once more itself.
	t.Run("a LogValuer whose second answer differs from its first", func(t *testing.T) {
		alone, through := 0, 0
		var plain, masked bytes.Buffer
		lineOf(t, logging.NewCloudHandler(&plain), nil, "a line", "value", valuerOnce{&alone})
		lineOf(t, logging.NewCloudHandler(Writer(&masked)), nil, "a line", "value", valuerOnce{&through})
		if alone != 1 || through != 1 {
			t.Errorf("the value was read %d times alone and %d times through the mask; want once each", alone, through)
		}
		if masked.String() != plain.String() || !strings.Contains(masked.String(), `"value":"clean"`) {
			t.Errorf("the line through the mask is %q; want the line alone, which holds the first answer: %q", masked.String(), plain.String())
		}
	})
}

// decodedText is every string of line, one JSON object, keys included, decoded and joined.
func decodedText(t *testing.T, line string) string {
	t.Helper()
	var texts []string
	for _, s := range spans(t, line) {
		if s.isString {
			texts = append(texts, s.text)
		}
	}
	return strings.Join(texts, "\n")
}

// TestTheMaskReadsTheLineAReplaceAttrWrote: a handler's ReplaceAttr can change a value after the
// handler was given it, here decoding a []byte the caller logged into the text it holds. The mask
// reads the line the handler wrote, so the value the ReplaceAttr made is masked.
func TestTheMaskReadsTheLineAReplaceAttrWrote(t *testing.T) {
	decode := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		if raw, ok := a.Value.Any().([]byte); ok {
			return slog.String(a.Key, string(raw))
		}
		return a
	}
	var plain, masked bytes.Buffer
	slog.New(slog.NewJSONHandler(&plain, &slog.HandlerOptions{ReplaceAttr: decode})).Info("a line", "value", []byte("decoded "+held))
	slog.New(slog.NewJSONHandler(Writer(&masked), &slog.HandlerOptions{ReplaceAttr: decode})).Info("a line", "value", []byte("decoded "+held))
	if !strings.Contains(plain.String(), held) {
		t.Fatalf("the premise: the ReplaceAttr wrote no placeholder: %q", plain.String())
	}
	checkMasked(t, plain.String(), masked.String())
}

// TestTheMaskWritesNoOtherFormOfAValue: the four values a review measured, each holding the
// placeholder and a real value in a form the JSON handler does not write - an error whose
// MarshalText or Format holds both, a value whose AppendText holds both, a struct whose String
// holds both. The mask writes the line the JSON handler wrote, which holds neither, byte for byte.
func TestTheMaskWritesNoOtherFormOfAValue(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
	}{
		{"an error whose MarshalText holds both", errorQuotesInText{}},
		{"an error whose Format holds both", errorFormatsBoth{}},
		{"a value whose AppendText holds both", appendsBoth{}},
		{"a struct whose String holds both", hostShowsBoth{Host: "api.example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain, masked := brokerLines(t, nil, "a line", "value", c.value)
			if masked != plain {
				t.Errorf("the mask wrote %q; want the JSON handler's own line %q", masked, plain)
			}
			if strings.Contains(masked, realValue) || heldAnywhere.MatchString(masked) {
				t.Errorf("the line holds the real value or the placeholder: %q", masked)
			}
		})
	}
}

type (
	errorQuotesInText struct{}
	errorFormatsBoth  struct{}
	appendsBoth       struct{}
	hostShowsBoth     struct{ Host string }
)

func (errorQuotesInText) Error() string { return "request failed" }
func (errorQuotesInText) MarshalText() ([]byte, error) {
	return []byte("Bearer " + held + " swapped for " + realValue), nil
}
func (errorFormatsBoth) Error() string { return "request failed" }
func (errorFormatsBoth) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, "request failed: [Bearer %s] upstream [%s]", held, realValue)
}
func (appendsBoth) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }
func (appendsBoth) AppendText(b []byte) ([]byte, error) {
	return append(b, held+" "+realValue...), nil
}
func (h hostShowsBoth) String() string { return h.Host + " " + held + " -> " + realValue }

// TestTheMaskKeepsARefusalsOtherNames: a name a caller chose can change only its own string. A
// refusal names the secrets its request named (answer.SecretNames), and a name holding a byte the
// JSON handler escapes before text that reads as a placeholder once escaped - a reread of
// "%1Agent-secrets-placeholder-", the byte 0x1A and "gent-secrets-placeholder-", which is no
// placeholder - is written as "\u001agent-secrets-placeholder-". A mask that read the escaped text
// as a placeholder from the escape's last digit would write "\u001[placeholder]", which is no
// JSON; the mask reads the decoded name, so that name is written as it came, a name that does hold
// a placeholder after such a byte is masked as decoded, the line stays one JSON object and
// DEEL_API_KEY stays as it was written.
func TestTheMaskKeepsARefusalsOtherNames(t *testing.T) {
	for _, name := range []string{"\x1agent-secrets-placeholder-", "\x1a" + requests.PlaceholderPrefix, "\x1a" + held, "\x00" + held, "a\\" + held, "a\"" + held} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			var names answer.SecretNames
			names.Note("DEEL_API_KEY", name)
			plain, masked := brokerLines(t, nil, answer.RefusalMessage, names.AppendFields(nil)...)
			checkMasked(t, plain, masked)
			var line struct{ Secrets []string }
			if err := json.Unmarshal([]byte(masked), &line); err != nil || len(line.Secrets) != 2 || line.Secrets[0] != "DEEL_API_KEY" {
				t.Fatalf("the refusal's secrets are %v (%v); want DEEL_API_KEY and the masked name: %q", line.Secrets, err, masked)
			}
		})
	}
}

// FuzzTheMaskKeepsALineOneJSONObject: for any name a caller chooses - control bytes, escapes,
// quotation marks, backslashes, a placeholder whole, in part or in any case, invalid UTF-8 - and
// written where a caller's text reaches a line, as a value, in a list beside another name, as a
// key and as a group's name, the masked line is one JSON object that changes only the strings
// that hold a placeholder, and with none in it is the handler's own line byte for byte
// (checkMasked). Its seeds run as rows of every go test.
func FuzzTheMaskKeepsALineOneJSONObject(f *testing.F) {
	for _, seed := range []string{
		"\x1agent-secrets-placeholder-", "\x1a" + requests.PlaceholderPrefix, "%1A" + requests.PlaceholderPrefix, "\x1a" + held, held, strings.ToUpper(held),
		`\u0061gent-secrets-placeholder-x`, "\"" + held + "\"", "\\" + held, requests.PlaceholderPrefix[:10], "agent-secrets-placehold",
		"\xff\xfe" + held, "\x00\x01\x1f\x7f", "\u2028\u2029", "<b>&amp;" + held, "", "a\nb" + held, strings.Repeat(held, 3),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		var names answer.SecretNames
		names.Note("DEEL_API_KEY", name)
		attrs := append(names.AppendFields(nil), "name", name, slog.String("k"+name, "v"))
		plain, masked := brokerLines(t, func(h slog.Handler) slog.Handler { return h.WithGroup("g" + name) }, "a line "+name, attrs...)
		checkMasked(t, plain, masked)
	})
}

// TestTheMaskWritesALineThatIsNoJSONObjectAsNotJSON: a line the mask cannot read as one JSON
// object is a line it cannot mask, so it writes NotJSON in its place - a text, a JSON value that
// is no object, an object cut short - and each line of a Write is read on its own.
func TestTheMaskWritesALineThatIsNoJSONObjectAsNotJSON(t *testing.T) {
	for _, c := range []struct{ name, write, want string }{
		{"a text line", "level=INFO msg=" + held + "\n", NotJSON + "\n"},
		{"a JSON value that is no object", `["` + held + `"]` + "\n", NotJSON + "\n"},
		{"an object cut short", `{"message":"` + held + "\n", NotJSON + "\n"},
		{"a text line with no placeholder", "a line\n", NotJSON + "\n"},
		{"two lines, one of them a text", `{"message":"` + held + `"}` + "\nplain " + held + "\n", `{"message":"` + Mark + `"}` + "\n" + NotJSON + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if n, err := Writer(&out).Write([]byte(c.write)); err != nil || n != len(c.write) {
				t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(c.write))
			}
			if out.String() != c.want {
				t.Errorf("the mask wrote %q; want %q", out.String(), c.want)
			}
		})
	}
}

// TestTheMaskCannotSeeAByteSlice: what a byte mask cannot see, stated as a row. The JSON handler
// writes a []byte in base64, at any depth, so a placeholder in one reaches the line as base64,
// and the mask hands the line on as the handler wrote it. The handler-level mask before it did
// not see one either: it read a []byte's JSON, which is the same base64. The broker logs no []byte.
func TestTheMaskCannotSeeAByteSlice(t *testing.T) {
	plain, masked := brokerLines(t, nil, "a line", "value", []byte(held))
	if masked != plain || !strings.Contains(masked, base64.StdEncoding.EncodeToString([]byte(held))) {
		t.Errorf("the line is %q; want the handler's own, %q, which holds the placeholder's base64", masked, plain)
	}
}

// TestHidePeerBytesHidesFromTheFirstQuotationMark: each line's text from its first quotation mark
// to its end is written as PeerBytesShown, whatever pairs of quotation marks it holds, so a stray
// quotation mark before a run cannot pair with the run's opening one and leave the run in clear.
// The control is that row read the way a pairing of quotation marks reads it: the real value is
// left in clear, so the row fails for any reading that pairs them.
func TestHidePeerBytesHidesFromTheFirstQuotationMark(t *testing.T) {
	stray := `a"b, then malformed HTTP response "` + realValue + `"`
	for _, c := range []struct{ name, text, want string }{
		{"no quotation mark", "dial tcp 10.0.0.1:443: connection refused", "dial tcp 10.0.0.1:443: connection refused"},
		{"one run", `net/http: HTTP/1.x transport connection broken: malformed HTTP response "echo-` + realValue + `"`,
			"net/http: HTTP/1.x transport connection broken: malformed HTTP response " + PeerBytesShown},
		{"a run and words after it", `Unsolicited response received on idle HTTP channel starting with "ok` + realValue + `"; err=<nil>`,
			"Unsolicited response received on idle HTTP channel starting with " + PeerBytesShown},
		{"an escaped quotation mark in a run", `malformed MIME header line: "{\"authorization\":\"` + realValue + `\"}"`, "malformed MIME header line: " + PeerBytesShown},
		{"a stray quotation mark before a run", stray, "a" + PeerBytesShown},
		{"each line on its own", "first \"" + realValue + "\"\nsecond line\nthird \"" + realValue, "first " + PeerBytesShown + "\nsecond line\nthird " + PeerBytesShown},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := HidePeerBytes(c.text); got != c.want {
				t.Errorf("HidePeerBytes(%q) = %q; want %q", c.text, got, c.want)
			}
		})
	}
	paired := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).ReplaceAllLiteralString(stray, PeerBytesShown)
	if !strings.Contains(paired, realValue) {
		t.Fatalf("the control: a reading that pairs quotation marks hid the run behind a stray one: %q", paired)
	}
}

// TestStandardLogCutsSplitsAndMasksALibrarysLine: the log package's output once the broker has set
// up its logging (StandardLog) writes each write as one line of the logger: a write of one line
// as its message, cut to answer.FieldBytes; a write of several as its first line, cut, and the
// rest as its stack, whole; and a write that ends in a blank line as no stack. Each run of bytes
// a library quotes from a peer goes first (LibraryLines), and through the broker's handler each
// placeholder is masked.
func TestStandardLogCutsSplitsAndMasksALibrarysLine(t *testing.T) {
	var out bytes.Buffer
	library := log.New(StandardLog(slog.New(logging.NewCloudHandler(Writer(&out)))), "", 0)
	long := held + " " + strings.Repeat("x", 300)
	library.Print(long)
	library.Print(long + "\nsecond line " + held + "\nthird line")
	library.Print(`Unsolicited response received on idle HTTP channel starting with "ok` + realValue + `"; err=<nil>`)
	library.Print("a line and a blank one\n")
	var lines []map[string]any
	for _, text := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("a line is no JSON object: %q: %v", text, err)
		}
		lines = append(lines, line)
	}
	if len(lines) != 4 {
		t.Fatalf("the four writes are %d lines; want four: %q", len(lines), out.String())
	}
	cut := Mark + answer.Clip(long)[len(held):]
	if lines[0]["message"] != cut || lines[0]["stack"] != nil || lines[0]["severity"] != "INFO" {
		t.Errorf("the one-line write is %v; want its message %q, cut to %d bytes and masked, at INFO, with no stack", lines[0], cut, answer.FieldBytes)
	}
	if lines[1]["message"] != cut || lines[1]["stack"] != "second line "+Mark+"\nthird line" {
		t.Errorf("the write of three lines is %v; want its first line cut and masked and the rest, whole and masked, as its stack", lines[1])
	}
	if want := "Unsolicited response received on idle HTTP channel starting with " + PeerBytesShown; lines[2]["message"] != want {
		t.Errorf("the quoting library line's message = %q; want %q", lines[2]["message"], want)
	}
	if _, stacked := lines[3]["stack"]; stacked || lines[3]["message"] != "a line and a blank one" {
		t.Errorf("the write that ends in a blank line is %v; want its line and no stack", lines[3])
	}
}
