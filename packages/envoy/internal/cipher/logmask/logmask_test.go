package logmask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// held and other are placeholders as the broker mints them, digitsFirst and digitsInside two
// whose characters past the prefix hold digits, from the first on and further in, and realValue a
// value a swap puts in, built from parts so that no scanner reads it as a key.
var (
	held         = placeholder.Prefix + "AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPp"
	other        = placeholder.Prefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"
	digitsFirst  = placeholder.Prefix + "0123AbCd4567EfGh89IjKlMnOpQrStUv"
	digitsInside = placeholder.Prefix + "AbCd0123EfGh4567IjKl89MnOpQrStUv"
	resultHeld   = placeholder.ResultPrefix + "RrEeSsUuLlTtGgRrAaNnTt0123456789"
	realValue    = strings.Join([]string{"sk", "live", "REAL", "VALUE", "7f3c9a1e5b2d"}, "-")
	at           = time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)
)

// holdsPlaceholder is the test's own reading of whether text holds a placeholder: the prefix
// anywhere in it, in any case as Unicode's simple folding reads case (strings.EqualFold, which
// reads ſ as s). It reads no pattern of the mask's, so a check of the mask does not lean on the
// mask's own reading.
func holdsPlaceholder(text string) bool {
	for i := range text {
		if _, ok := prefixAt(text[i:]); ok {
			return true
		}
	}
	return false
}

// prefixAt is how many bytes of text the prefix it opens with takes, read rune by rune in any
// case, and false where text opens with no prefix.
func prefixAt(text string) (int, bool) {
	n := 0
	for _, want := range placeholder.Prefix {
		r, size := utf8.DecodeRuneInString(text[n:])
		if size == 0 || !strings.EqualFold(string(r), string(want)) {
			return 0, false
		}
		n += size
	}
	return n, true
}

// tailsAfterPrefix is each run of placeholder.Length characters of the alphabet the broker
// mints from (placeholder.Alphabet) that text writes right after a prefix, with how many
// times it is written there: what follows the prefix of a placeholder written whole.
func tailsAfterPrefix(text string) map[string]int {
	tails := map[string]int{}
	for i := range text {
		n, ok := prefixAt(text[i:])
		if !ok || len(text)-i-n < placeholder.Length {
			continue
		}
		tail := text[i+n : i+n+placeholder.Length]
		if !strings.ContainsFunc(tail, func(r rune) bool { return !strings.ContainsRune(placeholder.Alphabet, r) }) {
			tails[tail]++
		}
	}
	return tails
}

// countOverlapping is how many times s holds sub, counting each place it starts.
func countOverlapping(s, sub string) int {
	n := 0
	for i := 0; ; i++ {
		next := strings.Index(s[i:], sub)
		if next < 0 {
			return n
		}
		n++
		i += next
	}
}

// isSubsequence is whether every byte of sub is in s, in the same order.
func isSubsequence(sub, s string) bool {
	for i := range len(sub) {
		at := strings.IndexByte(s, sub[i])
		if at < 0 {
			return false
		}
		s = s[at+1:]
	}
	return true
}

// tailRune is whether r is a character of a placeholder past its prefix: one of the alphabet the
// broker mints from (placeholder.Alphabet), or a letter Unicode's simple folding reads as
// one of it, as the prefix is read.
func tailRune(r rune) bool {
	for _, c := range placeholder.Alphabet {
		if strings.EqualFold(string(r), string(c)) {
			return true
		}
	}
	return false
}

// tailRun is the run of tail characters (tailRune) text opens with, at most most of them where most
// is not negative, and ending where a prefix starts.
func tailRun(text string, most int) string {
	n := 0
	for i, r := range text {
		if _, opens := prefixAt(text[i:]); n == most || opens || !tailRune(r) {
			return text[:i]
		}
		n++
	}
	return text
}

// runsAfterMarks is, for each Mark text holds, the run of tail characters it writes right after
// it, with how many times each run is written there.
func runsAfterMarks(text string) map[string]int {
	runs := map[string]int{}
	for i := strings.Index(text, Mark); i >= 0; {
		rest := text[i+len(Mark):]
		runs[tailRun(rest, -1)]++
		next := strings.Index(rest, Mark)
		if next < 0 {
			break
		}
		i += len(Mark) + next
	}
	return runs
}

// leftPastPlaceholders is each run of tail characters a string that came in may have written
// right after a Mark once its placeholders are masked: for each prefix, the run that follows the
// placeholder.Length tail characters after it, up to the next prefix, and for each Mark
// the string wrote itself, the run after it (runsAfterMarks).
func leftPastPlaceholders(text string) map[string]int {
	left := runsAfterMarks(text)
	for i := range text {
		if n, ok := prefixAt(text[i:]); ok {
			rest := text[i+n:]
			left[tailRun(rest[len(tailRun(rest, placeholder.Length)):], -1)]++
		}
	}
	return left
}

// lineOf has handler write one record with message and attrs, through with where a row sets it
// (the handler a row gets from WithAttrs or WithGroup), into whatever handler writes to. A record
// is made at one time, so two lines of it compare byte for byte.
func lineOf(t *testing.T, handler slog.Handler, with func(slog.Handler) slog.Handler, message string, attrs ...any) {
	t.Helper()
	if with != nil {
		handler = with(handler)
	}
	record := slog.NewRecord(at, slog.LevelWarn, message, 0)
	record.Add(attrs...)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("the handler refused the record: %v", err)
	}
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

// tokensOf is line as its tokens, read by encoding/json's decoder with each number as written, or
// an error where line is not one JSON object: an object, and nothing after it but white space.
func tokensOf(line string) ([]span, error) {
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	var out []span
	previous, depth := int64(0), 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if len(out) == 0 || depth != 0 {
				return nil, errors.New("no whole JSON object")
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(out) == 0 && token != json.Delim('{') {
			return nil, fmt.Errorf("a JSON value that is no object: %v", token)
		}
		if len(out) > 0 && depth == 0 {
			return nil, errors.New("more after the JSON object")
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		offset := decoder.InputOffset()
		s := span{raw: line[previous:offset]}
		s.text, s.isString = token.(string)
		out = append(out, s)
		previous = offset
	}
}

// maskedAsClaimed is the mask's claim for one line, against the line that came in, each read as
// its tokens (tokensOf): the masked line is one JSON object of as many tokens; each token whose
// decoded text holds no placeholder (holdsPlaceholder) is written byte for byte as it came, with
// what comes before it; and a line that held none is written as it came. Each string that held one
// is a string, after the same separator, that holds no prefix and holds Mark where a placeholder
// was; with each Mark taken out it is a subsequence of the string that came in, shorter by at most
// a result grant's prefix and placeholder.Length characters for each Mark; no run of the minted alphabet
// the string wrote right after a prefix (tailsAfterPrefix) is written more often than the string
// wrote it with no prefix before it; and no Mark is followed by any run of a placeholder's
// characters (tailRune) but one the string itself wrote past a placeholder's
// placeholder.Length or after a Mark of its own (leftPastPlaceholders). So a caller's text
// changes no token but its own, the mask takes out no more than the placeholders, and no
// placeholder's characters are left in clear. It reads no pattern of the mask's. It answers how
// many strings the mask wrote again, or why the masked line breaks the claim.
func maskedAsClaimed(plain, masked string) (int, error) {
	before, err := tokensOf(plain)
	if err != nil {
		return 0, fmt.Errorf("the line that came in is no JSON object (%v): %q", err, plain)
	}
	after, err := tokensOf(masked)
	if err != nil {
		return 0, fmt.Errorf("the masked line is no JSON object (%v): %q", err, masked)
	}
	if len(before) != len(after) {
		return 0, fmt.Errorf("the masked line has %d tokens and the line that came in %d: %q, %q", len(after), len(before), masked, plain)
	}
	rewritten := 0
	for i := range before {
		if !before[i].isString || !holdsPlaceholder(before[i].text) {
			if after[i].raw != before[i].raw {
				return 0, fmt.Errorf("the masked line writes %q where %q came, which holds no placeholder: %q, %q", after[i].raw, before[i].raw, masked, plain)
			}
			continue
		}
		rewritten++
		sep := before[i].raw[:strings.IndexByte(before[i].raw, '"')]
		if !after[i].isString || !strings.HasPrefix(after[i].raw, sep+`"`) || holdsPlaceholder(after[i].text) || !strings.Contains(after[i].text, Mark) {
			return 0, fmt.Errorf("the masked line writes %q where %q came; want a string after the same %q that holds %s and no placeholder", after[i].raw, before[i].raw, sep, Mark)
		}
		marks, rest := strings.Count(after[i].text, Mark), strings.ReplaceAll(after[i].text, Mark, "")
		if !isSubsequence(rest, before[i].text) {
			return 0, fmt.Errorf("the masked string %q, with each %s taken out, is no subsequence of %q: the mask wrote text the string did not hold", after[i].text, Mark, before[i].text)
		}
		most := marks * (utf8.RuneCountInString(placeholder.ResultPrefix) + placeholder.Length)
		if removed := utf8.RuneCountInString(before[i].text) - utf8.RuneCountInString(rest); removed > most {
			return 0, fmt.Errorf("the mask took %d characters out of %q for %d marks; want at most %d, a result grant's prefix and %d characters for each: %q", removed, before[i].text, marks, most, placeholder.Length, after[i].text)
		}
		for tail, afterPrefix := range tailsAfterPrefix(before[i].text) {
			if written, own := countOverlapping(after[i].text, tail), countOverlapping(before[i].text, tail)-afterPrefix; written > own {
				return 0, fmt.Errorf("the masked string %q writes %q %d times, which %q writes right after a prefix; want at most %d, the times it is written with no prefix before it", after[i].text, tail, written, before[i].text, own)
			}
		}
		left := leftPastPlaceholders(before[i].text)
		for run, written := range runsAfterMarks(after[i].text) {
			if run != "" && written > left[run] {
				return 0, fmt.Errorf("the masked string %q writes %q right after %s %d times; want at most %d, the times %q writes it past a placeholder's %d characters or after a %s of its own: a placeholder's characters are left in clear", after[i].text, run, Mark, written, left[run], before[i].text, placeholder.Length, Mark)
			}
		}
	}
	if rewritten == 0 && masked != plain {
		return 0, fmt.Errorf("the line holds no placeholder, and the mask changed it: %q; want %q", masked, plain)
	}
	return rewritten, nil
}

// checkMasked is maskedAsClaimed failing t, for the line the handler wrote alone and the line it
// wrote through the mask.
func checkMasked(t *testing.T, plain, masked string) int {
	t.Helper()
	rewritten, err := maskedAsClaimed(plain, masked)
	if err != nil {
		t.Fatal(err)
	}
	return rewritten
}

// decodedText is every string of line, one JSON object, keys included, decoded and joined.
func decodedText(t *testing.T, line string) string {
	t.Helper()
	tokens, err := tokensOf(line)
	if err != nil {
		t.Fatalf("the line is no JSON object (%v): %q", err, line)
	}
	var texts []string
	for _, s := range tokens {
		if s.isString {
			texts = append(texts, s.text)
		}
	}
	return strings.Join(texts, "\n")
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
			if !holdsPlaceholder(decodedText(t, plain)) {
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

// TestTheMaskMasksEachPlaceholderOfARun: a placeholder is the prefix and at most the minted
// length of letters and digits after it, and it stops where the next prefix starts. So two
// placeholders written together are two marks, as the prefix written once or twice before a
// placeholder is, and none of them leaves the characters of the placeholder after it in clear;
// letters past the minted length are no placeholder's, and are written as they came.
func TestTheMaskMasksEachPlaceholderOfARun(t *testing.T) {
	prefix := placeholder.Prefix
	for _, c := range []struct{ name, text, want string }{
		{"two placeholders written together", held + other, Mark + Mark},
		{"two placeholders in upper case written together", strings.ToUpper(held + other), Mark + Mark},
		{"the prefix written once before a placeholder", prefix + held, Mark + Mark},
		{"the prefix written twice before a placeholder", prefix + prefix + held, Mark + Mark + Mark},
		{"a placeholder and letters past the minted length", held + "QqRr", Mark + "QqRr"},
		{"a placeholder between words", "from " + held + "/" + other + " on", "from " + Mark + "/" + Mark + " on"},
		// Go's case folding reads ſ as s and the Kelvin sign as k, as the prefix is read.
		{"a placeholder whose letters hold ſ and the Kelvin sign", prefix + "AaBb\u017fCc\u212aDd" + strings.Repeat("Ee", 11), Mark},
		// A placeholder's characters are digits as well as letters, from the first on or further in.
		{"a placeholder whose characters start with digits", "from " + digitsFirst + " on", "from " + Mark + " on"},
		{"a placeholder whose characters hold digits further in", "from " + digitsInside + " on", "from " + Mark + " on"},
		// A CI run's result grant opens with the prefix and "result-", whose hyphen no minted
		// character is: its 32 characters are masked with it, in any case.
		{"a result grant's placeholder", "from " + resultHeld + " on", "from " + Mark + " on"},
		{"a result grant's placeholder in upper case", "from " + strings.ToUpper(resultHeld) + " on", "from " + Mark + " on"},
		{"a result grant's placeholder beside a release's", resultHeld + held, Mark + Mark},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain, masked := brokerLines(t, nil, "a line", "v", c.text)
			checkMasked(t, plain, masked)
			var line struct{ V string }
			if err := json.Unmarshal([]byte(masked), &line); err != nil || line.V != c.want {
				t.Fatalf("the mask wrote v as %q (%v); want %q: %q", line.V, err, c.want, masked)
			}
		})
	}
	// The string the mask writes again is JSON as slog's handler writes a string, HTML left as it
	// came, so a reader of the line reads the caller's own characters.
	t.Run("a string holding HTML, written as slog's handler writes one", func(t *testing.T) {
		_, masked := brokerLines(t, nil, "a line", "v", "<b>&"+held)
		if want := `"v":"<b>&` + Mark + `"`; !strings.Contains(masked, want) {
			t.Fatalf("the mask wrote %q; want %s in it", masked, want)
		}
	})
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
	for _, name := range []string{"\x1agent-secrets-placeholder-", "\x1a" + placeholder.Prefix, "\x1a" + held, "\x00" + held, "a\\" + held, "a\"" + held} {
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
		"\x1agent-secrets-placeholder-", "\x1a" + placeholder.Prefix, "%1A" + placeholder.Prefix, "\x1a" + held, held, strings.ToUpper(held),
		`\u0061gent-secrets-placeholder-x`, "\"" + held + "\"", "\\" + held, placeholder.Prefix[:10], "agent-secrets-placehold",
		"\xff\xfe" + held, "\x00\x01\x1f\x7f", "\u2028\u2029", "<b>&amp;" + held, "", "a\nb" + held, strings.Repeat(held, 3),
		held + other, placeholder.Prefix + held, "from " + digitsFirst + " on", "from " + digitsInside + " on",
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

// FuzzTheMaskWritesEachLineAsOneJSONObject is Writer for any bytes at all, not only the lines
// slog writes - raw escapes, invalid UTF-8, lone surrogates, several lines in one Write, text and
// JSON values that are no object - read against encoding/json's decoder (tokensOf) and no pattern
// of the mask's: the Write's lines come out one for one, each one JSON object; a line that came in
// as one is masked as maskedAsClaimed claims, so it changes no token but those that held a
// placeholder and a line that held none comes out byte for byte; and a line that came in as none
// is NotJSON. Its seeds run as rows of every go test.
func FuzzTheMaskWritesEachLineAsOneJSONObject(f *testing.F) {
	var line bytes.Buffer
	slog.New(logging.NewCloudHandler(&line)).Info("a line", "host", held+".example.net", "k", 1, "g", slog.GroupValue(slog.String("h", "v")))
	for _, seed := range []string{
		line.String(), strings.ReplaceAll(line.String(), held, "clean"), line.String() + line.String(),
		`{"m":"\u0061gent-secrets-placeholder-x"}`, `{"` + held + `":1}`, `{"m":"` + held + other + `"}`,
		`{"m":"` + placeholder.Prefix + held + `"}`, `{"m":"\ud800` + held + `"}`, "{\"m\":\"\xff" + held + "\xfe\"}",
		`{"m":"from ` + digitsFirst + ` on"}`, `{"m":"from ` + digitsInside + ` on"}`,
		`{"m":"agent-ſecrets-placeholder-x"}`, `{"m":"a\"b\\c\/d\u2028","n":[1,-2.5e3,true,null,{"o":"p"}]}`,
		`{"m":"<&>` + held + `"}`, `["` + held + `"]`, `"` + held + `"`, "level=INFO msg=" + held, "", "\n", "\n\n",
		`{"m":"x"} {"n":"y"}`, `{"m":"` + held, " \t{\"m\":\"" + held + "\"}\r", "{\"m\":\"\x01" + held + "\"}", `{"m":"\q` + held + `"}`,
		`{"m":"a\"` + held + `\\b"}`, `{"m":"\u0001` + held + `\n"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, p []byte) {
		var out bytes.Buffer
		if n, err := Writer(&out).Write(p); err != nil || n != len(p) {
			t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(p))
		}
		if len(p) == 0 {
			if out.Len() != 0 {
				t.Fatalf("an empty Write wrote %q", out.String())
			}
			return
		}
		in := strings.Split(strings.TrimSuffix(string(p), "\n"), "\n")
		written := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(written) != len(in) {
			t.Fatalf("a Write of %d lines came out as %d: %q, %q", len(in), len(written), p, out.String())
		}
		for i := range in {
			if _, err := tokensOf(written[i]); err != nil {
				t.Fatalf("line %d came out as no JSON object (%v): %q, from %q", i, err, written[i], in[i])
			}
			if _, err := tokensOf(in[i]); err != nil {
				if written[i] != NotJSON {
					t.Fatalf("line %d is no JSON object (%v) and came out as %q; want %s", i, err, written[i], NotJSON)
				}
				continue
			}
			if _, err := maskedAsClaimed(in[i], written[i]); err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
		}
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

// TestAWriteOfManyLinesKeepsEachLineAndTheCallersBytes: a Write can hold many lines, and the mask
// reads each on its own. Each row is one Write of many lines that mixes four kinds, in an order of
// its own: clean lines, lines that hold a placeholder, lines that spell one in \u escapes, and
// lines that are no JSON object; some rows are drawn at random, from a fixed seed. Each Write
// comes out as as many lines as went in, in their order, and each line as a Write of that line
// alone writes it: a clean line as it came, a line that held a placeholder masked, a line that is
// no JSON object as NotJSON. And the caller's buffer, to the end of its capacity, is byte for byte
// as it was: io.Writer's Write must not change p, so the mask writes the lines it changes into a
// buffer of its own, never over the bytes it reads.
func TestAWriteOfManyLinesKeepsEachLineAndTheCallersBytes(t *testing.T) {
	type line struct{ kind, in, want string }
	var clean, notJSON []line
	for _, in := range []string{
		`{"severity":"INFO","message":"airlock proxy: a call to a bound host","host":"api.example.com","status":200}`,
		`{"m":"a\"b\\c\/d\u2028","n":[1,-2.5e3,true,null,{"o":"p"}]}`,
		`{"message":"agent-secrets-placehold, the prefix cut short, and ünïcode"}`,
		" \t{\"m\":\"x\"}\r",
	} {
		clean = append(clean, line{"clean", in, in})
	}
	placeholders := []line{
		{"placeholder", `{"severity":"INFO","message":"a call","path":"/v1/` + held + `"}`, `{"severity":"INFO","message":"a call","path":"/v1/` + Mark + `"}`},
		{"placeholder", `{"` + held + `":1,"m":"` + strings.ToUpper(other) + ` and <&>` + digitsFirst + `"}`, `{"` + Mark + `":1,"m":"` + Mark + ` and <&>` + Mark + `"}`},
	}
	tail := held[len(placeholder.Prefix):]
	escaped := []line{
		{"\\u-escaped placeholder", `{"m":"\u0061gent-secrets-placeholder-` + tail + `"}`, `{"m":"` + Mark + `"}`},
		{"\\u-escaped placeholder", `{"` + escapedEach(held) + `":"v","n":2}`, `{"` + Mark + `":"v","n":2}`},
		{"\\u-escaped placeholder", `{"m":"a\"` + placeholder.Prefix + tail[:4] + escapedEach(tail[4:8]) + tail[8:] + `\n"}`, `{"m":"a\"` + Mark + `\n"}`},
	}
	for _, in := range []string{"level=INFO msg=" + held, `["` + held + `"]`, `{"message":"` + held, `{"m":"x"} {"n":"y"}`, "", `{"m":"\q"}`} {
		notJSON = append(notJSON, line{"not-JSON", in, NotJSON})
	}
	kinds := [][]line{clean, placeholders, escaped, notJSON}
	every := slices.Concat(kinds...)
	for _, l := range every {
		var alone bytes.Buffer
		if _, err := Writer(&alone).Write([]byte(l.in + "\n")); err != nil || alone.String() != l.want+"\n" {
			t.Fatalf("a Write of the %s line %q alone wrote %q, %v; want %q", l.kind, l.in, alone.String(), err, l.want+"\n")
		}
	}
	inTurn := func(rounds int) []line {
		var lines []line
		for r := range rounds {
			for _, kind := range kinds {
				lines = append(lines, kind[r%len(kind)])
			}
		}
		return lines
	}
	backwards := func(lines []line) []line {
		lines = slices.Clone(lines)
		slices.Reverse(lines)
		return lines
	}
	type row struct {
		name  string
		lines []line
	}
	rows := []row{
		{"every line, in the table's order", every},
		{"every line, last to first", backwards(every)},
		{"clean lines only", slices.Repeat(clean, 8)},
		{"lines that are no JSON object only", slices.Repeat(notJSON, 4)},
		{"many clean lines, then a placeholder", append(slices.Repeat(clean, 8), placeholders[0])},
		{"an escaped placeholder, then many clean lines", append([]line{escaped[1]}, slices.Repeat(clean, 8)...)},
		{"the kinds in turn", inTurn(12)},
		{"the kinds in turn, last to first", backwards(inTurn(12))},
	}
	draw := rand.New(rand.NewPCG(1, 2))
	for i := range 64 {
		lines := make([]line, 2+draw.IntN(47))
		for j := range lines {
			lines[j] = every[draw.IntN(len(every))]
		}
		rows = append(rows, row{fmt.Sprintf("drawn %d", i), lines})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var in []byte
			for _, l := range r.lines {
				in = append(append(in, l.in...), '\n')
			}
			// p is the caller's buffer with room past its end, as a bytes.Buffer's has; the Write
			// may change none of it.
			p := append(make([]byte, 0, len(in)+64), in...)
			whole := p[:cap(p)]
			for i := len(p); i < len(whole); i++ {
				whole[i] = '#'
			}
			before := bytes.Clone(whole)
			var out bytes.Buffer
			if n, err := Writer(&out).Write(p); err != nil || n != len(p) {
				t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(p))
			}
			for i := range whole {
				if whole[i] != before[i] {
					t.Errorf("the Write changed the caller's buffer from byte %d on, of %d and its room past them: %q, was %q",
						i, len(p), whole[i:min(i+48, len(whole))], before[i:min(i+48, len(before))])
					break
				}
			}
			got := out.String()
			written := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			if !strings.HasSuffix(got, "\n") || len(written) != len(r.lines) {
				t.Fatalf("a Write of %d lines, each ending in a newline, came out as %d: %q", len(r.lines), len(written), got)
			}
			for i, l := range r.lines {
				if written[i] != l.want {
					t.Fatalf("line %d of %d, a %s line, came out as %q; want %q, as a Write of it alone writes it", i, len(r.lines), l.kind, written[i], l.want)
				}
			}
		})
	}
}

// escapedEach is text, ASCII, with each of its bytes written as a JSON \u escape.
func escapedEach(text string) string {
	var escaped strings.Builder
	for i := range len(text) {
		fmt.Fprintf(&escaped, `\u%04x`, text[i])
	}
	return escaped.String()
}

// TestAStringTheMaskCannotDecodeIsWrittenAsTheMark: Writer reads only a line json.Valid passes, so
// every string it hands maskString decodes. A string that did not would be one the mask cannot
// read, so it cannot say what it holds: maskString writes Mark in its place, whole, rather than
// the string as it came. The rows hand maskString such strings directly, one of them with a
// placeholder before an escape JSON has none of.
func TestAStringTheMaskCannotDecodeIsWrittenAsTheMark(t *testing.T) {
	for _, token := range []string{`"` + held + `\q"`, `"\q"`, `"\u12"`} {
		if masked, rewritten := maskString([]byte(token)); !rewritten || string(masked) != `"`+Mark+`"` {
			t.Errorf("maskString(%q) = %q, %v; want %q, true", token, masked, rewritten, `"`+Mark+`"`)
		}
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
	lines := logtest.JSONLines(t, out.String())
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

// BenchmarkWriter is the mask's cost for one line of the shape the broker writes most, a call
// line of the airlock's proxy port with twelve fields: with no placeholder, with one in its path,
// with a quotation mark in its path, which the JSON handler writes as \", as it writes every
// quoted error, and with a control byte, which it writes as a \u escape.
func BenchmarkWriter(b *testing.B) {
	for _, c := range []struct{ name, path string }{
		{"a clean call line", "/v1/things"},
		{"a call line with a placeholder", "/v1/" + held},
		{"a call line with a quotation mark", `/v1/"things"`},
		{"a call line with a \\u escape", "/v1/\x01things"},
	} {
		b.Run(c.name, func(b *testing.B) {
			var line bytes.Buffer
			slog.New(logging.NewCloudHandler(&line)).Info("airlock proxy: a call to a bound host",
				"answer_id", "01HZX3K5V7Q9T2M4N6P8R0S2U4", "method", "GET", "host", "api.example.com", "path", c.path,
				"status", 200, "code", "HTTP_200", "grant", "g-7f3c9a1e", "enrollment", "e-5b2d", "session", "s-1a2b3c",
				"runtime", "r-4d5e6f", "slot", "primary", "duration_ms", 143)
			w := Writer(io.Discard)
			b.SetBytes(int64(line.Len()))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := w.Write(line.Bytes()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
