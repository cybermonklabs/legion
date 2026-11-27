package storetext

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// planted is the credential a far side's error carries in a URL, so that one search of an output
// finds any part of it the rule left.
const planted = "SECRET"

func TestTextMakesEveryTextStorableAndIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{name: "a NUL", text: "wedged\x00lock", want: "wedged\uFFFDlock"},
		{name: "a lone 0xff", text: "a\xffb", want: "a\uFFFDb"},
		{name: "a run of invalid bytes", text: "a\xff\xfe\xc3b", want: "a\uFFFDb"},
		{name: "a truncated character at the end", text: "caf\xc3", want: "caf\uFFFD"},
		{name: "a NUL between two invalid bytes", text: "\xff\x00\xff", want: "\uFFFD\uFFFD\uFFFD"},
		{name: "an invalid byte beside a written U+FFFD", text: "\xff\uFFFD", want: "\uFFFD\uFFFD"},
		{name: "the six characters of a NUL escape kept", text: `a\u0000b`, want: `a\u0000b`},
		{name: "valid text kept", text: "héllo, 世界 😀", want: "héllo, 世界 😀"},
		{name: "the empty text", text: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.text)
			if got != tc.want {
				t.Fatalf("Text(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
				t.Fatalf("Text(%q) = %q, want valid UTF-8 with no NUL", tc.text, got)
			}
			if again := Text(got); again != got {
				t.Fatalf("Text(%q) = %q, want the text kept as the rule left it", got, again)
			}
			if b := TextBytes([]byte(tc.text)); string(b) != tc.want {
				t.Fatalf("TextBytes(%q) = %q, want %q, what Text gives", tc.text, b, tc.want)
			}
		})
	}
}

func TestTextBytesReturnsItsArgumentWhenNothingChangesAndNeverWritesIntoIt(t *testing.T) {
	clean := []byte("clean text, 世界")
	if got := TextBytes(clean); &got[0] != &clean[0] || len(got) != len(clean) {
		t.Fatalf("TextBytes(%q) = %q, want its argument back", clean, got)
	}
	dirty := []byte("a\x00b\xffc")
	given := bytes.Clone(dirty)
	got := TextBytes(dirty)
	if string(got) != "a\uFFFDb\uFFFDc" || !bytes.Equal(dirty, given) {
		t.Fatalf("TextBytes(%q) = %q and left its argument %q, want %q and the argument as given", given, got, dirty, "a\uFFFDb\uFFFDc")
	}
}

func TestJSONMakesEveryStringOfAValueStorable(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{name: "a NUL escape in a value", text: `{"a":"x\u0000y"}`, want: "{\"a\":\"x\uFFFDy\"}"},
		{name: "a NUL escape in a key", text: `{"k\u0000":1}`, want: "{\"k\uFFFD\":1}"},
		{name: "a lone high surrogate", text: `["\ud800"]`, want: "[\"\uFFFD\"]"},
		{name: "a high surrogate before a character that is no low one", text: `["\ud83d\u0041"]`, want: "[\"\uFFFDA\"]"},
		{name: "a lone low surrogate", text: `["\udc00x"]`, want: "[\"\uFFFDx\"]"},
		{name: "a valid pair kept", text: `{"a":"\ud83d\ude00"}`, want: `{"a":"\ud83d\ude00"}`},
		{name: "a valid pair beside a NUL escape", text: `{"a":"\ud83d\ude00\u0000"}`, want: "{\"a\":\"😀\uFFFD\"}"},
		{name: "a raw NUL byte in a string", text: "{\"a\":\"x\x00y\"}", want: "{\"a\":\"x\uFFFDy\"}"},
		{name: "an invalid byte in a string", text: "{\"a\":\"\xff\"}", want: "{\"a\":\"\uFFFD\"}"},
		{name: "numbers kept as written", text: `{"big":1e400,"dec":0.10,"int":12345678901234567890,"n":"\u0000"}`, want: "{\"big\":1e400,\"dec\":0.10,\"int\":12345678901234567890,\"n\":\"\uFFFD\"}"},
		{name: "a duplicate key keeps its last value", text: `{"a":"\u0000","a":2}`, want: `{"a":2}`},
		{name: "a duplicate key keeps its last value cleaned", text: `{"a":1,"a":"\u0000"}`, want: "{\"a\":\"\uFFFD\"}"},
		{name: "two keys the rule makes one keep one value", text: `{"a\ufffd":1,"a\u0000":2}`, want: "{\"a\uFFFD\":1}"},
		{name: "nested values", text: `{"o":{"k":["\u0000",{"\udfff":null,"t":true}]}}`, want: "{\"o\":{\"k\":[\"\uFFFD\",{\"t\":true,\"\uFFFD\":null}]}}"},
		{name: "an escaped backslash before u0000 kept", text: `{"a":"\\u0000"}`, want: `{"a":"\\u0000"}`},
		{name: "an escaped backslash before a NUL escape", text: `{"a":"\\\u0000"}`, want: "{\"a\":\"\\\\\uFFFD\"}"},
		{name: "HTML characters not escaped", text: `{"a":"<b>&\u0000"}`, want: "{\"a\":\"<b>&\uFFFD\"}"},
		{name: "an invalid JSON text after the byte rule", text: "{\"a\":\"\\u0000\"\xff", want: "{\"a\":\"\\u0000\"\uFFFD"},
		{name: "a second value after the first is invalid JSON", text: `{"a":"\u0000"} {}`, want: `{"a":"\u0000"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			given := []byte(tc.text)
			got := JSON(given)
			if string(got) != tc.want {
				t.Fatalf("JSON(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if string(given) != tc.text {
				t.Fatalf("JSON(%q) wrote into its argument: %q", tc.text, given)
			}
			if !utf8.Valid(got) || bytes.IndexByte(got, 0) >= 0 {
				t.Fatalf("JSON(%q) = %q, want valid UTF-8 with no NUL", tc.text, got)
			}
			if again := JSON(got); !bytes.Equal(again, got) {
				t.Fatalf("JSON(%q) = %q, want the text kept as the rule left it", got, again)
			}
			if json.Valid(given) && (!json.Valid(got) || refusedEscape(got)) {
				t.Fatalf("JSON(%q) = %q, want valid JSON with no refused escape", tc.text, got)
			}
		})
	}
}

func TestJSONReturnsItsArgumentWhenNothingChanges(t *testing.T) {
	clean := []byte(`{"a":"\ud83d\ude00","b":"\\u0000","c":[1,0.10]}`)
	if got := JSON(clean); &got[0] != &clean[0] || len(got) != len(clean) {
		t.Fatalf("JSON(%q) = %q, want its argument back", clean, got)
	}
}

// JSON applies the rule at any depth of a value: a \u0000 escape and a lone surrogate escape
// nested forty containers deep, in arrays and objects in turn, become U+FFFD as they do at the
// top. This is what keeps JSON's own walk (storableValue) rather than Strings: Strings stops at
// maxDepth, and a decoded value costs it two levels for each container (the container and the
// interface that holds it), so it would leave these strings as Go decoded them, the NUL encoded
// as \u0000 again, and the server would refuse the whole value.
func TestJSONMakesEveryStringStorableAtAnyDepth(t *testing.T) {
	const containers = 40
	text, want := `{"k\u0000":"a\u0000b\udc00"}`, "{\"k\uFFFD\":\"a\uFFFDb\uFFFD\"}"
	for i := 1; i < containers; i++ {
		if i%2 == 1 {
			text, want = "["+text+"]", "["+want+"]"
		} else {
			text, want = `{"o":`+text+"}", `{"o":`+want+"}"
		}
	}
	got := JSON([]byte(text))
	if string(got) != want {
		t.Fatalf("JSON of a value %d containers deep = %q, want %q", containers, got, want)
	}
	if !json.Valid(got) || bytes.IndexByte(got, 0) >= 0 || refusedEscape(got) {
		t.Fatalf("JSON of a value %d containers deep = %q, want valid JSON with no NUL and no refused escape", containers, got)
	}
}

func TestChangesReportsWhetherTheRuleChangesAText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{name: "clean text", text: "a clean text, 世界 😀", want: false},
		{name: "the empty text", text: "", want: false},
		{name: "a NUL", text: "a\x00b", want: true},
		{name: "a lone 0xff", text: "a\xffb", want: true},
		{name: "a NUL escape", text: `{"a":"\u0000"}`, want: true},
		{name: "a NUL escape in no JSON", text: `a\u0000b`, want: true},
		{name: "a lone high surrogate escape", text: `"\ud800"`, want: true},
		{name: "a high surrogate escape at the end", text: `\udbff`, want: true},
		{name: "two high surrogate escapes before a low one", text: `"\ud83d\ud83d\ude00"`, want: true},
		{name: "a lone low surrogate escape", text: `"\uDC00"`, want: true},
		{name: "an escaped backslash before u0000", text: `"\\u0000"`, want: false},
		{name: "an escaped backslash before a NUL escape", text: `"\\\u0000"`, want: true},
		{name: "a valid pair", text: `"\ud83d\ude00"`, want: false},
		{name: "an escape that is not four hex digits", text: `"\u00"`, want: false},
		{name: "a backslash at the end", text: `a\`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Changes(tc.text); got != tc.want {
				t.Fatalf("Changes(%q) = %t, want %t", tc.text, got, tc.want)
			}
			if !tc.want && (Text(tc.text) != tc.text || string(JSON([]byte(tc.text))) != tc.text) {
				t.Fatalf("Changes(%q) = false, but the rule changes it", tc.text)
			}
		})
	}
}

func TestFarSideErrorBoundsTheCleanedTextKeepingItsStart(t *testing.T) {
	megabyte := strings.Repeat("x", 1<<20)
	long := "adopt refused: " + megabyte
	query := "https://h.example/p?token=" + strings.Repeat(planted, 20)
	for _, tc := range []struct {
		name, text, want string
	}{
		{
			name: "a megabyte of text",
			text: long,
			want: long[:FarSideErrorRunes-1] + "…",
		},
		{
			name: "a multi-byte character at the cut",
			text: strings.Repeat("a", FarSideErrorRunes-2) + strings.Repeat("é", 10),
			want: strings.Repeat("a", FarSideErrorRunes-2) + "é…",
		},
		{
			name: "a URL whose query crosses the cut",
			text: strings.Repeat("w", 2000) + " " + query + " tail " + megabyte,
			want: strings.Repeat("w", 2000) + " https://h.example/p tail " + megabyte[:FarSideErrorRunes-1-2000-len(" https://h.example/p tail ")] + "…",
		},
		{
			name: "a NUL and invalid bytes in a long text",
			text: "a\x00b\xff" + megabyte,
			want: "a\uFFFDb\uFFFD" + megabyte[:FarSideErrorRunes-1-4] + "…",
		},
		{
			name: "a text of exactly the bound kept whole",
			text: strings.Repeat("é", FarSideErrorRunes),
			want: strings.Repeat("é", FarSideErrorRunes),
		},
		{
			name: "a text one rune over the bound",
			text: strings.Repeat("é", FarSideErrorRunes+1),
			want: strings.Repeat("é", FarSideErrorRunes-1) + "…",
		},
		{
			name: "a short text kept whole",
			text: "dial tcp 127.0.0.1:9: connect: connection refused",
			want: "dial tcp 127.0.0.1:9: connect: connection refused",
		},
		{
			// The URL rule's own text of the URL, read from the rule, never written out here.
			name: "a short text loses its URL credentials",
			text: `Get "https://u:` + planted + `@h.example/x?token=` + planted + `": EOF`,
			want: redact.URLCredentials(`Get "https://u:` + planted + `@h.example/x?token=` + planted + `": EOF`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FarSideError(tc.text)
			if got != tc.want {
				t.Fatalf("FarSideError(%.80q…) = %.80q… of %d runes, want %.80q… of %d runes",
					tc.text, got, utf8.RuneCountInString(got), tc.want, utf8.RuneCountInString(tc.want))
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || utf8.RuneCountInString(got) > FarSideErrorRunes {
				t.Fatalf("FarSideError gave %d runes, want valid UTF-8 with no NUL and at most %d runes", utf8.RuneCountInString(got), FarSideErrorRunes)
			}
			if strings.Contains(got, "token") || strings.Contains(got, planted) {
				t.Fatalf("FarSideError(%.80q…) holds a part of the URL's credentials", tc.text)
			}
			if again := FarSideError(got); again != got {
				t.Fatalf("FarSideError of its own result gave %.80q…, want the result kept", again)
			}
		})
	}
}

// FarSideErrorEnds keeps a long text's start and its end around one "…", the whole at most
// FarSideErrorRunes runes, cut at runes and never inside a character, after the URL rule has read
// the whole text; a text of at most the bound is kept whole.
func TestFarSideErrorEndsKeepsBothEnds(t *testing.T) {
	keep := FarSideErrorRunes / 2
	middle := strings.Repeat("m", 1<<20)
	start := redact.URLCredentials("start https://u:" + planted + "@h.example/x ")
	for _, tc := range []struct {
		name, text, want string
	}{
		{
			name: "a megabyte between a start and a note at the end",
			text: "Command failed (exit 1)" + middle + "the git lock is removed in 16 minutes",
			want: "Command failed (exit 1)" + middle[:keep-1-len("Command failed (exit 1)")] + "…" +
				middle[:keep-len("the git lock is removed in 16 minutes")] + "the git lock is removed in 16 minutes",
		},
		{
			name: "multi-byte characters at both cuts",
			text: strings.Repeat("é", FarSideErrorRunes+5),
			want: strings.Repeat("é", keep-1) + "…" + strings.Repeat("é", keep),
		},
		{
			// The start as the URL rule writes it, read from the rule, never written out here.
			name: "a URL's credentials in the part the cut drops",
			text: "start https://u:" + planted + "@h.example/x " + middle + " end",
			want: start + middle[:keep-1-len(start)] + "…" + middle[:keep-len(" end")] + " end",
		},
		{
			name: "a text of exactly the bound kept whole",
			text: strings.Repeat("é", FarSideErrorRunes),
			want: strings.Repeat("é", FarSideErrorRunes),
		},
		{
			name: "a short text kept whole",
			text: "dial tcp 127.0.0.1:9: connect: connection refused",
			want: "dial tcp 127.0.0.1:9: connect: connection refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FarSideErrorEnds(tc.text)
			if got != tc.want {
				t.Fatalf("FarSideErrorEnds(%.80q…) = %.80q…%.80q of %d runes, want %.80q…%.80q of %d runes",
					tc.text, got, got[max(0, len(got)-80):], utf8.RuneCountInString(got),
					tc.want, tc.want[max(0, len(tc.want)-80):], utf8.RuneCountInString(tc.want))
			}
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > FarSideErrorRunes || strings.Contains(got, planted) {
				t.Fatalf("FarSideErrorEnds gave %d runes, want valid UTF-8 of at most %d runes with no credential", utf8.RuneCountInString(got), FarSideErrorRunes)
			}
		})
	}
}

// logLine is one record a JSON handler wrote for LogWhole.
type logLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Row   int    `json:"row"`
	Part  int    `json:"part"`
	Parts int    `json:"parts"`
	Error string `json:"error"`
}

// logged is every record the JSON handler wrote to buf.
func logged(t *testing.T, buf *bytes.Buffer) []logLine {
	t.Helper()
	var lines []logLine
	for dec := json.NewDecoder(buf); ; {
		var line logLine
		if err := dec.Decode(&line); errors.Is(err, io.EOF) {
			return lines
		} else if err != nil {
			t.Fatalf("the log holds a line that is no JSON: %v", err)
		}
		lines = append(lines, line)
	}
}

func TestLogWholeLogsAMegabyteInPartsThatJoinToTheWholeCleanedText(t *testing.T) {
	// A megabyte of characters of one, two and three bytes, so that the parts' bound falls inside
	// a character.
	text := "start https://h.example/?token=" + planted + " \x00\xff " + strings.Repeat("aé€", (1<<20)/len("aé€")) + " the last words"
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, redact.HandlerOptions(nil)))
	LogWhole(log, "outbox: the row's error", text, "row", 7)
	if strings.Contains(buf.String(), planted) {
		t.Fatal("the log holds the planted credential")
	}
	lines := logged(t, &buf)
	var whole strings.Builder
	movedBack := 0 // parts before the last that a character at the bound made shorter
	for i, line := range lines {
		if line.Level != "INFO" || line.Msg != "outbox: the row's error" || line.Row != 7 || line.Part != i+1 || line.Parts != len(lines) {
			t.Fatalf("line %d is %s %q row %d part %d of %d, want INFO, the message, row 7 and part %d of %d",
				i, line.Level, line.Msg, line.Row, line.Part, line.Parts, i+1, len(lines))
		}
		if len(line.Error) > 32<<10 || !utf8.ValidString(line.Error) {
			t.Fatalf("part %d is %d bytes (valid UTF-8: %t), want at most 32 KiB cut at a rune boundary",
				line.Part, len(line.Error), utf8.ValidString(line.Error))
		}
		if line.Part < line.Parts && len(line.Error) < 32<<10 {
			movedBack++
		}
		whole.WriteString(line.Error)
	}
	want := redact.URLCredentials(Text(text))
	if whole.String() != want {
		t.Fatalf("the %d parts join to %d bytes, want the %d bytes of the whole cleaned text", len(lines), whole.Len(), len(want))
	}
	if len(lines) < len(want)/(32<<10) || movedBack == 0 {
		t.Fatalf("the text was logged in %d parts, %d of them cut short of 32 KiB at a character, want at least %d parts and one cut so",
			len(lines), movedBack, len(want)/(32<<10))
	}
}

// A text FarSideError keeps whole is in the store whole, so LogWhole logs nothing of it.
func TestLogWholeLogsNothingOfATextFarSideErrorKeepsWhole(t *testing.T) {
	for _, text := range []string{"dial tcp 127.0.0.1:9: connect: connection refused", strings.Repeat("é", FarSideErrorRunes)} {
		var buf bytes.Buffer
		LogWhole(slog.New(slog.NewJSONHandler(&buf, nil)), "outbox: the row's error", text)
		if buf.Len() != 0 {
			t.Fatalf("LogWhole of %d runes logged %.200q, want nothing", utf8.RuneCountInString(text), buf.String())
		}
	}
}

// sample is a value of every shape Strings reads into: a string and a named string, a struct, a
// slice of structs, a map, a pointer, an interface, an array, bytes, and an unexported string.
type sample struct {
	Title    string
	Kind     sampleKind
	Inner    sampleInner
	Runs     []sampleInner
	Labels   map[string]string
	Parent   *sampleInner
	Observed any
	Heads    [2]string
	Body     json.RawMessage
	Bundle   []byte
	seen     string
}

type sampleInner struct {
	Name string
	Tags []string
}

type sampleKind string

// dirtySample is a sample holding a NUL or an invalid byte in every string Strings reaches, and
// in its bytes and its unexported field, which it leaves as they are. Each call makes a new one,
// so a test can hold the value it gave Strings beside the one it began as.
func dirtySample() sample {
	return sample{
		Title:    "root\x00title",
		Kind:     "kind\x00",
		Inner:    sampleInner{Name: "unit\x00tests", Tags: []string{"tag\xff"}},
		Runs:     []sampleInner{{Name: "lint"}, {Name: "unit\x00"}},
		Labels:   map[string]string{"key\x00": "value\x00", "kept": "as is"},
		Parent:   &sampleInner{Name: "parent\x00"},
		Observed: sampleInner{Name: "a\xffb"},
		Heads:    [2]string{"head\x00", "kept"},
		Body:     json.RawMessage("\"a\x00\""),
		Bundle:   []byte{0, 0xff},
		seen:     "seen\x00",
	}
}

func TestStringsReplacesEachNULAndInvalidByteItReachesWithAReplacementCharacter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		given, want sample
	}{
		{name: "a nested struct",
			given: sample{Inner: sampleInner{Name: "unit\x00tests", Tags: []string{"tag\x00"}}},
			want:  sample{Inner: sampleInner{Name: "unit\uFFFDtests", Tags: []string{"tag\uFFFD"}}}},
		{name: "a slice of structs",
			given: sample{Runs: []sampleInner{{Name: "lint"}, {Name: "unit\x00"}}},
			want:  sample{Runs: []sampleInner{{Name: "lint"}, {Name: "unit\uFFFD"}}}},
		{name: "a map with a NUL key and value",
			given: sample{Labels: map[string]string{"key\x00": "value\x00", "kept": "as is"}},
			want:  sample{Labels: map[string]string{"key\uFFFD": "value\uFFFD", "kept": "as is"}}},
		{name: "two keys the rule makes one keep the value of the greater original key",
			given: sample{Labels: map[string]string{"a\x00": "of the NUL", "a\uFFFD": "of U+FFFD", "b\xff": "of 0xff", "b\uFFFD": "of U+FFFD"}},
			want:  sample{Labels: map[string]string{"a\uFFFD": "of U+FFFD", "b\uFFFD": "of 0xff"}}},
		{name: "a pointer",
			given: sample{Parent: &sampleInner{Name: "parent\x00"}},
			want:  sample{Parent: &sampleInner{Name: "parent\uFFFD"}}},
		{name: "an interface field holding a struct",
			given: sample{Observed: sampleInner{Name: "a\xffb"}},
			want:  sample{Observed: sampleInner{Name: "a\uFFFDb"}}},
		{name: "an interface field holding a pointer to a string",
			given: sample{Observed: func() *string { s := "x\x00"; return &s }()},
			want:  sample{Observed: func() *string { s := "x\uFFFD"; return &s }()}},
		{name: "an array",
			given: sample{Heads: [2]string{"head\x00", "kept"}},
			want:  sample{Heads: [2]string{"head\uFFFD", "kept"}}},
		{name: "a string and a named string",
			given: sample{Title: "root\x00title", Kind: "kind\x00"},
			want:  sample{Title: "root\uFFFDtitle", Kind: "kind\uFFFD"}},
		{name: "bytes and an unexported field kept as they are",
			given: sample{Title: "\x00", Body: json.RawMessage("\"a\x00\""), Bundle: []byte{0, 0xff}, seen: "seen\x00"},
			want:  sample{Title: "\uFFFD", Body: json.RawMessage("\"a\x00\""), Bundle: []byte{0, 0xff}, seen: "seen\x00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Strings(tc.given)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Strings(%#v) = %#v, want %#v", tc.given, got, tc.want)
			}
			if again := Strings(got); !reflect.DeepEqual(again, got) {
				t.Fatalf("Strings(%#v) = %#v, want the value kept as the rule left it", got, again)
			}
		})
	}
	if got := Strings("a\x00b"); got != "a\uFFFDb" {
		t.Fatalf("Strings of a string = %q, want %q", got, "a\uFFFDb")
	}
	var fact any = sample{Title: "root\x00title"}
	if got := Strings(fact); !reflect.DeepEqual(got, any(sample{Title: "root\uFFFDtitle"})) {
		t.Fatalf("Strings of an interface holding a sample = %#v, want the sample cleaned", got)
	}
	if got := Strings[any](nil); got != nil {
		t.Fatalf("Strings of a nil interface = %#v, want nil", got)
	}
}

func TestStringsReturnsAValueHoldingNoNULAsItIsWithItsOwnSlicesMapsAndPointers(t *testing.T) {
	clean := sample{
		Title: "root title", Kind: "kind",
		Inner:    sampleInner{Name: "unit tests", Tags: []string{"tag"}},
		Runs:     []sampleInner{{Name: "lint"}, {Name: "unit"}},
		Labels:   map[string]string{"key": "value"},
		Parent:   &sampleInner{Name: "parent", Tags: []string{"p"}},
		Observed: &sampleInner{Name: "observed"},
		Heads:    [2]string{"head", "kept"},
		Body:     json.RawMessage(`"a\u0000"`),
		Bundle:   []byte{0, 0xff},
		seen:     "seen\x00",
	}
	shares := func(t *testing.T, got sample, what string) {
		t.Helper()
		if &got.Inner.Tags[0] != &clean.Inner.Tags[0] || &got.Runs[0] != &clean.Runs[0] || got.Parent != clean.Parent ||
			got.Observed.(*sampleInner) != clean.Observed.(*sampleInner) || &got.Bundle[0] != &clean.Bundle[0] || &got.Body[0] != &clean.Body[0] ||
			reflect.ValueOf(got.Labels).UnsafePointer() != reflect.ValueOf(clean.Labels).UnsafePointer() {
			t.Fatalf("Strings of %s = %#v, want each slice, map and pointer of %#v itself", what, got, clean)
		}
	}

	got := Strings(clean)
	if !reflect.DeepEqual(got, clean) {
		t.Fatalf("Strings(%#v) = %#v, want the value as it is", clean, got)
	}
	shares(t, got, "a value holding no NUL")
	var fact any = clean
	shares(t, Strings(fact).(sample), "an interface holding a value with no NUL")

	// Only the path to the string the rule changes is copied.
	dirty := clean
	dirty.Title = "root\x00title"
	got = Strings(dirty)
	if got.Title != "root\uFFFDtitle" {
		t.Fatalf("Strings(%#v).Title = %q, want %q", dirty, got.Title, "root\uFFFDtitle")
	}
	shares(t, got, "a value whose title holds a NUL")
}

func TestStringsLeavesTheCallersValueHoldingItsNULs(t *testing.T) {
	given := dirtySample()
	got := Strings(given)
	if !reflect.DeepEqual(given, dirtySample()) {
		t.Fatalf("Strings wrote into the value it was given: it is %#v, want %#v", given, dirtySample())
	}
	want := sample{
		Title:    "root\uFFFDtitle",
		Kind:     "kind\uFFFD",
		Inner:    sampleInner{Name: "unit\uFFFDtests", Tags: []string{"tag\uFFFD"}},
		Runs:     []sampleInner{{Name: "lint"}, {Name: "unit\uFFFD"}},
		Labels:   map[string]string{"key\uFFFD": "value\uFFFD", "kept": "as is"},
		Parent:   &sampleInner{Name: "parent\uFFFD"},
		Observed: sampleInner{Name: "a\uFFFDb"},
		Heads:    [2]string{"head\uFFFD", "kept"},
		Body:     json.RawMessage("\"a\x00\""),
		Bundle:   []byte{0, 0xff},
		seen:     "seen\x00",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Strings(%#v) = %#v, want %#v", dirtySample(), got, want)
	}
}

// node is a value that can point to itself.
type node struct {
	Name string
	Next *node
}

func TestStringsEndsAtItsDepthBoundOnACycleOfPointersHoldingNUL(t *testing.T) {
	cycle := &node{Name: "loop\x00"}
	cycle.Next = cycle
	got := Strings(cycle)
	if got == cycle || got.Name != "loop\uFFFD" || cycle.Name != "loop\x00" {
		t.Fatalf("Strings of a cycle holding a NUL = %p named %q, the cycle %p named %q; want a copy named %q and the cycle as it was",
			got, got.Name, cycle, cycle.Name, "loop\uFFFD")
	}
	steps := 0
	for at := got; at != cycle; at = at.Next {
		if at.Name != "loop\uFFFD" {
			t.Fatalf("the copy %d pointers from its start is named %q, want %q", steps, at.Name, "loop\uFFFD")
		}
		if steps++; steps > maxDepth {
			t.Fatalf("the copy runs on past %d pointers, want it to end at the cycle itself within its depth bound", maxDepth)
		}
	}
}
