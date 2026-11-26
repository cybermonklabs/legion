package logmask

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/logging"
)

// tinker and second are two kinds' own placeholder prefixes (proxyrules.Kind.PlaceholderPrefix):
// tinker's tml-, which html- holds, and sk-, which task- holds. tinkerHeld, tinkerOther and
// secondHeld are placeholders the release mints with them, each the prefix and
// placeholder.Length characters of placeholder.Alphabet, and hash 32 hexadecimal digits, which
// are characters of the alphabet too.
const (
	tinker = "tml-"
	second = "sk-"
	hash   = "0123456789abcdef0123456789abcdef"
)

var (
	tinkerHeld  = tinker + "QqWwEeRrTtYyUuIiOoPpAaSsDdFfGgHh"
	tinkerOther = tinker + "0123456789ZzXxCcVvBbNnMmLlKkJjHh"
	secondHeld  = second + "MmNnBbVvCcXxZz9876543210LlKkJjHh"
)

// withPrefixes adds prefixes to the mask (AddPrefixes) for t, and puts back the prefixes the mask
// read before when t ends. Every Writer of the package reads them, so no test that adds prefixes
// runs in parallel with another test.
func withPrefixes(t *testing.T, prefixes ...string) {
	t.Helper()
	before := current.Load()
	t.Cleanup(func() { current.Store(before) })
	AddPrefixes(prefixes)
}

// wholeAt is the test's own reading of whether text holds prefix, in any ASCII case, followed by
// placeholder.Length characters of placeholder.Alphabet: a whole placeholder of that prefix if
// nothing before it counted. It reads no pattern of the mask's.
func wholeAt(text, prefix string) bool {
	for i := 0; i+len(prefix)+placeholder.Length <= len(text); i++ {
		tail := text[i+len(prefix) : i+len(prefix)+placeholder.Length]
		if strings.EqualFold(text[i:i+len(prefix)], prefix) && strings.Trim(tail, placeholder.Alphabet) == "" {
			return true
		}
	}
	return false
}

// valueThrough is the line the broker's own handler writes for one record whose value v is text,
// alone and through the mask, and v as the masked line holds it.
func valueThrough(t *testing.T, text string) (plain, masked, v string) {
	t.Helper()
	plain, masked = brokerLines(t, nil, "a line", "v", text)
	var line struct{ V string }
	if err := json.Unmarshal([]byte(masked), &line); err != nil {
		t.Fatalf("the masked line is no JSON object (%v): %q", err, masked)
	}
	return plain, masked, line.V
}

// TestTheKindPlaceholdersAreWhole: the premise of every test of this file, that each kind
// placeholder it plants is its prefix and placeholder.Length characters of the alphabet, as the
// release mints one (placeholder.Mint), and that hash is placeholder.Length characters of it too.
func TestTheKindPlaceholdersAreWhole(t *testing.T) {
	for _, c := range []struct{ prefix, held string }{{tinker, tinkerHeld}, {tinker, tinkerOther}, {second, secondHeld}, {"", hash}} {
		tail, ok := strings.CutPrefix(c.held, c.prefix)
		if !ok || len(tail) != placeholder.Length || strings.Trim(tail, placeholder.Alphabet) != "" {
			t.Errorf("%q is no prefix %q and %d characters of the alphabet", c.held, c.prefix, placeholder.Length)
		}
	}
}

// TestTheMaskMasksAKindsPlaceholderWhereItReachesALine: once AddPrefixes has added two kinds'
// prefixes, a placeholder of either is masked in each place a command's text reaches a line -
// the message, a string value, a key, an error, a group's name - after a quotation mark, which
// the handler writes as \", and after a control byte, which it writes as a \u escape, and in
// upper case, as the default prefix is read in any case. Each masked line is byte for byte the
// line the handler writes for the same record with Mark in the placeholder's place, which reads
// no pattern of the mask's; each row's premise is that the handler's own line holds the
// placeholder.
func TestTheMaskMasksAKindsPlaceholderWhereItReachesALine(t *testing.T) {
	withPrefixes(t, tinker, second)
	for _, c := range []struct {
		name   string
		spell  func(string) string
		record func(text string) (string, []any)
	}{
		{"the message", nil, func(s string) (string, []any) { return "a call naming " + s, nil }},
		{"a string value", nil, func(s string) (string, []any) { return "a line", []any{"path", "/v1/models/" + s + "/run"} }},
		{"a key", nil, func(s string) (string, []any) { return "a line", []any{s, "v"} }},
		{"an error", nil, func(s string) (string, []any) {
			return "a line", []any{"error", errors.New("dial " + s + ": refused")}
		}},
		{"a group's name", nil, func(s string) (string, []any) { return "a line", []any{slog.Group(s, "k", "v")} }},
		{"a value after a quotation mark", nil, func(s string) (string, []any) { return "a line", []any{"v", `"` + s} }},
		{"a key after a control byte", nil, func(s string) (string, []any) { return "a line", []any{"\x01" + s, "v"} }},
		{"the placeholder in upper case", strings.ToUpper, func(s string) (string, []any) { return "a line", []any{"host", s} }},
	} {
		for _, held := range []string{tinkerHeld, secondHeld} {
			t.Run(c.name+", "+held, func(t *testing.T) {
				spelled := held
				if c.spell != nil {
					spelled = c.spell(held)
				}
				message, attrs := c.record(spelled)
				plain, masked := brokerLines(t, nil, message, attrs...)
				if !strings.Contains(decodedText(t, plain), spelled) {
					t.Fatalf("the premise: the handler's own line does not hold %q, so the row tests nothing: %q", spelled, plain)
				}
				var want bytes.Buffer
				message, attrs = c.record(Mark)
				lineOf(t, logging.NewCloudHandler(&want), nil, message, attrs...)
				if masked != want.String() {
					t.Errorf("the mask wrote %q; want %q, the line with %s where the placeholder was", masked, want.String(), Mark)
				}
			})
		}
	}
}

// TestAKindsPrefixOpensAPlaceholderOnlyWhole: a kind's own prefix is short, and a word can hold it,
// so it opens a placeholder only where the release could have minted one: followed by
// placeholder.Length characters of the alphabet, which are masked with it, whatever follows them,
// and with no ASCII letter or digit right before it. So tinker's tml- inside html- and a hash, and
// sk- inside task-, are written as they came, and so are the prefix alone and the prefix with one
// character too few. The html- row's premise is the control: by the test's own reading the text
// holds tml- and placeholder.Length characters of the alphabet, so only the character before the
// prefix keeps it from being masked. A character past ASCII is no letter here: the mask fails
// closed, and masks a placeholder after ſ.
func TestAKindsPrefixOpensAPlaceholderOnlyWhole(t *testing.T) {
	withPrefixes(t, tinker, second)
	for _, c := range []struct{ name, text, want string }{
		{"tml- inside html- and a hash", "/assets/html-" + hash + ".js", "/assets/html-" + hash + ".js"},
		{"tml- inside HTML- and a hash", "/assets/HTML-" + hash, "/assets/HTML-" + hash},
		{"sk- inside task- and a hash", "task-" + hash, "task-" + hash},
		{"a placeholder after a digit", "v2" + tinkerHeld, "v2" + tinkerHeld},
		{"the prefix alone", "keys open with " + tinker + " and " + second, "keys open with " + tinker + " and " + second},
		{"the prefix and one character too few", tinkerHeld[:len(tinkerHeld)-1] + " on", tinkerHeld[:len(tinkerHeld)-1] + " on"},
		{"the prefix and the minted length", "from " + tinkerHeld + " on", "from " + Mark + " on"},
		{"the prefix and more than the minted length", tinkerHeld + "Zz", Mark + "Zz"},
		{"a placeholder that opens the text", tinkerHeld, Mark},
		{"a placeholder after a hyphen", "x-" + tinkerHeld, "x-" + Mark},
		{"a placeholder after an underscore", "key_" + secondHeld, "key_" + Mark},
		{"a placeholder after a slash", "/v1/" + tinkerHeld + "/run", "/v1/" + Mark + "/run"},
		{"a placeholder after ſ", "\u017f" + tinkerHeld, "\u017f" + Mark},
		{"a placeholder whose characters hold ſ and the Kelvin sign", tinker + "AaBb\u017fCc\u212aDd" + strings.Repeat("Ee", 11), Mark},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "tml- inside html- and a hash" && !wholeAt(c.text, tinker) {
				t.Fatalf("the control: by the test's own reading %q holds no %q and %d characters of the alphabet, so the row does not test the character before the prefix", c.text, tinker, placeholder.Length)
			}
			plain, masked, v := valueThrough(t, c.text)
			if v != c.want {
				t.Fatalf("the mask wrote v as %q; want %q: %q", v, c.want, masked)
			}
			if c.want == c.text && masked != plain {
				t.Errorf("the line holds no placeholder, and the mask changed it: %q; want %q", masked, plain)
			}
		})
	}
}

// TestPlaceholdersOfDifferentPrefixesWrittenTogetherAreEachMasked: each placeholder is a Mark of
// its own whichever prefix it opens with, and none leaves a character of another in clear. A
// kind's placeholder written right after another placeholder has a letter or digit before it,
// but that character is the other placeholder's, which the mask writes as Mark: it is read as
// the line is written, so the kind's placeholder is masked too, as is one that opens inside the
// characters that follow the default prefix.
func TestPlaceholdersOfDifferentPrefixesWrittenTogetherAreEachMasked(t *testing.T) {
	withPrefixes(t, tinker, second)
	for _, c := range []struct{ name, text, want string }{
		{"a kind's placeholder and a release's", tinkerHeld + held, Mark + Mark},
		{"a release's placeholder and a kind's", held + tinkerHeld, Mark + Mark},
		{"a minted grant's placeholder and a kind's", mintedHeld + secondHeld, Mark + Mark},
		{"two kinds' placeholders", tinkerHeld + secondHeld, Mark + Mark},
		{"two kinds' placeholders the other way", secondHeld + tinkerHeld, Mark + Mark},
		{"two placeholders of one kind", tinkerHeld + tinkerOther, Mark + Mark},
		{"three placeholders of three prefixes", secondHeld + held + tinkerHeld, Mark + Mark + Mark},
		{"two kinds' placeholders in upper case", strings.ToUpper(tinkerHeld + secondHeld), Mark + Mark},
		{"two placeholders between words", "from " + tinkerHeld + "/" + held + " on", "from " + Mark + "/" + Mark + " on"},
		{"the default prefix before a kind's placeholder", placeholder.Prefix + tinkerHeld, Mark + Mark},
		{"a kind's placeholder inside the characters after the default prefix", placeholder.Prefix + "Ab9" + secondHeld, Mark + Mark},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, masked, v := valueThrough(t, c.text)
			if v != c.want {
				t.Fatalf("the mask wrote v as %q; want %q: %q", v, c.want, masked)
			}
		})
	}
}

// TestAKindsPrefixHeldInAJSONEscapeIsMasked: a line is read as its strings decode. A kind's
// prefix that holds a character JSON may write with an escape other than \u, here a slash written
// \/, is in the decoded text and not in the line's bytes, so a line that holds such an escape is
// read string by string; a prefix spelled in \u escapes is decoded as the default's is.
func TestAKindsPrefixHeldInAJSONEscapeIsMasked(t *testing.T) {
	withPrefixes(t, tinker, "x/")
	tail := strings.TrimPrefix(tinkerHeld, tinker)
	for _, c := range []struct{ name, write, want string }{
		{"a slash written as an escape", `{"v":"x\/` + tail + `"}`, `{"v":"` + Mark + `"}`},
		{"a slash written as it is", `{"v":"x/` + tail + `"}`, `{"v":"` + Mark + `"}`},
		{"a prefix spelled in \\u escapes", `{"v":"\u0074\u006d\u006c-` + tail + `"}`, `{"v":"` + Mark + `"}`},
		{"a prefix in a key, after an escaped quotation mark", `{"a\"tml-` + tail + `":1}`, `{"a\"` + Mark + `":1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := Writer(&out).Write([]byte(c.write + "\n")); err != nil {
				t.Fatal(err)
			}
			if out.String() != c.want+"\n" {
				t.Errorf("the mask wrote %q; want %q", out.String(), c.want+"\n")
			}
		})
	}
}

// TestALineThatHoldsNoPrefixIsReadNoFurther: the mask decodes no string of a line of ASCII alone,
// with no \u escape, that holds no prefix it reads in any ASCII case (mayHoldPlaceholder), and
// added prefixes keep that so: with two kinds' prefixes added, a clean call line, a line that holds
// html and task with no hyphen, and a line with a quotation mark the handler writes as \" are read
// no further, and a line that holds either kind's prefix in any ASCII case, whole or not, or the
// default's, is read string by string. A kind's prefix that holds a slash makes a line with any
// escape read, and one that holds the Kelvin sign is looked for as the k an ASCII line holds,
// which Go's case folding reads it as, so that line's placeholder is masked.
func TestALineThatHoldsNoPrefixIsReadNoFurther(t *testing.T) {
	withPrefixes(t, tinker, second)
	for _, c := range []struct {
		line string
		read bool
	}{
		{`{"message":"a call","path":"/v1/things","host":"api.example.com"}`, false},
		{`{"v":"html5 task list"}`, false},
		{`{"v":"a \"quoted\" path"}`, false},
		{`{"v":"TmL-x"}`, true},
		{`{"v":"html-x"}`, true},
		{`{"SK-":1}`, true},
		{`{"v":"AGENT-SECRETS-PLACEHOLDER-"}`, true},
		{`{"v":"\u0001"}`, true},
	} {
		if got := mayHoldPlaceholder([]byte(c.line)); got != c.read {
			t.Errorf("mayHoldPlaceholder(%q) = %v; want %v", c.line, got, c.read)
		}
	}
	withPrefixes(t, "x/", "\u212ay-")
	if quoted := `{"v":"a \"quoted\" path"}`; !mayHoldPlaceholder([]byte(quoted)) {
		t.Errorf("with a prefix that holds a slash, mayHoldPlaceholder(%q) = false; want true", quoted)
	}
	kelvin := `{"v":"ky-` + strings.TrimPrefix(tinkerHeld, tinker) + `"}`
	var out bytes.Buffer
	if _, err := Writer(&out).Write([]byte(kelvin + "\n")); err != nil || out.String() != `{"v":"`+Mark+`"}`+"\n" {
		t.Errorf("with the prefix \u212ay-, the mask wrote %q (%v); want %q", out.String(), err, `{"v":"`+Mark+`"}`+"\n")
	}
}

// TestAddPrefixesWithNoKindPrefixChangesNothing: a file whose kinds set no prefix adds none, so
// AddPrefixes with no prefix, with "", or with the default prefix in any case writes every line as
// the mask wrote it before: a release's placeholder masked, and a kind's placeholder, a hash after
// a slash and a clean line as they came. "" read as a prefix would open a placeholder before every
// run of placeholder.Length characters of the alphabet, the hash's among them.
func TestAddPrefixesWithNoKindPrefixChangesNothing(t *testing.T) {
	lines := []string{
		`{"v":"from ` + held + ` on"}`,
		`{"v":"from ` + tinkerHeld + ` on"}`,
		`{"v":"/objects/` + hash + `"}`,
		`{"v":"a clean line"}`,
	}
	write := func() []string {
		var out []string
		for _, line := range lines {
			var buf bytes.Buffer
			if _, err := Writer(&buf).Write([]byte(line + "\n")); err != nil {
				t.Fatal(err)
			}
			out = append(out, buf.String())
		}
		return out
	}
	before := write()
	if want := `{"v":"from ` + Mark + ` on"}` + "\n"; before[0] != want || before[1] != lines[1]+"\n" || before[2] != lines[2]+"\n" {
		t.Fatalf("the premise: before AddPrefixes the mask wrote %q; want a release's placeholder masked and the rest as they came", before)
	}
	withPrefixes(t)
	AddPrefixes(nil)
	AddPrefixes([]string{"", placeholder.Prefix, strings.ToUpper(placeholder.Prefix)})
	if after := write(); strings.Join(after, "") != strings.Join(before, "") {
		t.Errorf("after AddPrefixes with no kind prefix the mask wrote %q; want %q, as before", after, before)
	}
}

// TestAddPrefixesWhileLinesAreWritten: AddPrefixes may run while other goroutines write lines,
// and every line written after it returns is read with its prefixes. Run under -race, the row
// also shows that a line's read of the prefixes and AddPrefixes' write of them do not race.
func TestAddPrefixesWhileLinesAreWritten(t *testing.T) {
	before := current.Load()
	t.Cleanup(func() { current.Store(before) })
	line := []byte(`{"v":"` + tinkerHeld + `"}` + "\n")
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for range 4 {
		started := make(chan struct{})
		writers.Go(func() {
			w := Writer(io.Discard)
			for i := 0; ; i++ {
				if _, err := w.Write(line); err != nil {
					t.Error(err)
					return
				}
				if i == 0 {
					close(started)
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		})
		<-started
	}
	AddPrefixes([]string{tinker})
	var out bytes.Buffer
	_, err := Writer(&out).Write(line)
	close(stop)
	writers.Wait()
	if err != nil || out.String() != `{"v":"`+Mark+`"}`+"\n" {
		t.Errorf("the line written after AddPrefixes returned is %q (%v); want %q", out.String(), err, `{"v":"`+Mark+`"}`+"\n")
	}
}
