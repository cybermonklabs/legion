package charts

import (
	"math/big"
	"strings"
	"testing"
)

// TestJSONIsReadAsPythonReadsIt checks the values the checks see: ints of any size apart from
// floats, a float past a float's range as an infinity, keys in their order, the last of a key given
// twice, and the escapes of a string, a lone surrogate among them.
func TestJSONIsReadAsPythonReadsIt(t *testing.T) {
	read := func(text string) any {
		t.Helper()
		value, err := decode([]byte(text))
		if err != nil {
			t.Fatalf("decode %s: %v", text, err)
		}
		return value
	}
	for text, want := range map[string]string{
		`0`: "0", `-0`: "0", `7`: "7", `-12`: "-12", `10000000000000000000000000000000000000`: "10000000000000000000000000000000000000",
	} {
		if got, ok := read(text).(*big.Int); !ok || got.String() != want {
			t.Errorf("%s reads %v (%T), want the int %s", text, read(text), read(text), want)
		}
	}
	for text, want := range map[string]float64{`1.5`: 1.5, `-0.25`: -0.25, `1e2`: 100, `1E+2`: 100, `2.5e-1`: 0.25, `0.0`: 0, `1e-400`: 0} {
		if got, ok := read(text).(float64); !ok || got != want {
			t.Errorf("%s reads %v (%T), want the float %v", text, read(text), read(text), want)
		}
	}
	if got := read(`[1e400, -1e400]`).([]any); !finiteOrInfinite(got[0], 1) || !finiteOrInfinite(got[1], -1) {
		t.Errorf("1e400 and -1e400 read %v", got)
	}
	if got := read(" \t\r\n[ true , false ,null ,{ } ,[ ] ]\n").([]any); len(got) != 5 || got[0] != true || got[1] != false || got[2] != nil {
		t.Errorf("a list of words and empty containers reads %v", got)
	}
	fields := read(`{"b": 1, "a": 2, "b": 3}`).(*object)
	if strings.Join(fields.keys, ",") != "b,a" || fields.get("b").(*big.Int).Int64() != 3 {
		t.Errorf("a key given twice reads %v in order %v", fields.values, fields.keys)
	}
	escapes := read(`"\"\\\/\b\f\n\r\t \u00e9\u0041 \ud83d\ude00 \uD83D\uDE00"`).(string)
	if want := "\"\\/\b\f\n\r\t é" + "A 😀 😀"; escapes != want {
		t.Errorf("the escapes of a string read %q, want %q", escapes, want)
	}
	for text, want := range map[string]string{
		`"\ud800"`: surrogate(0xD800), `"\udbff"`: surrogate(0xDBFF), `"\udc00"`: surrogate(0xDC00), `"\udfff"`: surrogate(0xDFFF),
		`"\ud800\u0041"`: surrogate(0xD800) + "A", `"\ud800\ud800"`: surrogate(0xD800) + surrogate(0xD800),
		`"\udc00\ud800"`: surrogate(0xDC00) + surrogate(0xD800), `"\ud800x"`: surrogate(0xD800) + "x",
		`"\ud800\\u"`: surrogate(0xD800) + `\u`,
	} {
		if got := read(text).(string); got != want {
			t.Errorf("%s reads %q, want %q", text, got, want)
		}
	}
	deep := strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth)
	read(deep)
	read(strings.Repeat(`{"a": `, maxDepth) + "1" + strings.Repeat("}", maxDepth))
}

func finiteOrInfinite(value any, sign int) bool {
	infinite, ok := value.(float64)
	return ok && infinity(infinite) == sign
}

// TestJSONThatPythonDoesNotReadIsAnError checks the bytes that decode refuses: what is no JSON,
// what Python refuses (an integer of more than 4,300 digits) and what no request body holds (NaN).
func TestJSONThatPythonDoesNotReadIsAnError(t *testing.T) {
	digits := func(count int) string { return strings.Repeat("1", count) }
	for name, text := range map[string]string{
		"no text": "", "only space": " \n", "an open list": "[", "a missing value": `[1,]`, "a missing item": `[,1]`,
		"a list that does not end": "[1", "two values": "[] []", "a trailing comma": `{"a": 1,}`,
		"a key that is no string": `{1: 2}`, "a key that does not end": `{"a`, "no colon": `{"a" 1}`, "no value": `{"a":}`, "an open object": `{"a": 1`,
		"a leading zero": "[01]", "a leading zero after a minus": "[-01]", "a lone minus": "[-]", "a minus and a letter": "[-a]",
		"a bare fraction": "[.5]", "a point and no digit": "[1.]", "an exponent and no digit": "[1e]", "an exponent sign and no digit": "[1e+]",
		"NaN": "NaN", "Infinity": "[Infinity]", "minus Infinity": "[-Infinity]", "a word": "[tru]", "a quote": "'a'",
		"an unterminated string": `"abc`, "a string ending in a backslash": `"abc\`, "a control character": "\"a\x01b\"",
		"a raw newline": "\"a\nb\"", "an unknown escape": `"\x"`, "a short escape": `"\u00e"`, "a short escape at the end": `"\u00`,
		"a bad hex escape": `"\u00zz"`, "a bad escape after a high surrogate": `"\ud800\uzzzz"`,
		"a signed hex escape": `"\u+123"`, "invalid UTF-8": "[\"\xff\"]", "a byte order mark": "\ufeff[]",
		"an integer of 4301 digits": digits(4301), "a negative integer of 4301 digits": "-" + digits(4301),
		"an integer of 4301 digits in a list": "[" + digits(4301) + "]", "an integer of 5000 digits in an object": `{"a": ` + digits(5000) + "}",
		"a list nested too deep":    strings.Repeat("[", maxDepth+1) + strings.Repeat("]", maxDepth+1),
		"an object nested too deep": strings.Repeat(`{"a": `, maxDepth+1) + "1" + strings.Repeat("}", maxDepth+1),
	} {
		if value, err := decode([]byte(text)); err == nil {
			t.Errorf("%s: decode reads %v, want an error", name, value)
		}
	}
	for name, text := range map[string]string{
		"an integer of 4300 digits":                 digits(4300),
		"a negative integer of 4300 digits":         "-" + digits(4300),
		"a float of 5000 digits and a fraction":     digits(5000) + ".5",
		"a float of 5000 digits and an exponent":    digits(5000) + "e1",
		"a string of 5000 digits":                   `"` + digits(5000) + `"`,
		"a zero before a fraction":                  "0.5",
		"an integer of 4300 digits that start in 0": "0",
	} {
		if _, err := decode([]byte(text)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestAnIntegerOfMoreThan4300DigitsIsNoJSONValueToValidate checks that Validate answers an error,
// as for any bytes Python's json.loads refuses, and no refusal, for an integer past Python's limit
// wherever it stands in a declaration; an integer of 4,300 digits is read, and refused as no finite
// number, as the platform refuses it.
func TestAnIntegerOfMoreThan4300DigitsIsNoJSONValueToValidate(t *testing.T) {
	declare := func(number string) string {
		return `[{"id": "a", "title": "T", "kind": "xy", "y": {"series": "s", "min": ` + number + `}}]`
	}
	for _, number := range []string{strings.Repeat("9", 4301), "-" + strings.Repeat("9", 4301), strings.Repeat("9", 9000)} {
		if refusals, err := Validate([]byte(declare(number))); err == nil {
			t.Errorf("an integer of %d digits answers no error but %s", len(strings.TrimPrefix(number, "-")), show(refusals))
		}
	}
	for _, number := range []string{strings.Repeat("9", 4300), "-" + strings.Repeat("9", 4300)} {
		got := validate(t, declare(number))
		want := []Refusal{{Field: "charts[0].y.min", Message: "charts[0].y.min must be a finite number"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("an integer of 4300 digits: got %s, want %s", show(got), show(want))
		}
	}
	if got := validate(t, declare(strings.Repeat("9", 5000)+".5")); len(got) != 1 || got[0].Field != "charts[0].y.min" {
		t.Errorf("a float of 5000 digits: got %s", show(got))
	}
}

// TestALoneSurrogateEscapeIsRefusedAsText checks, beyond the corpus, that a lone surrogate in a
// string is a character that is not printable and a pair is the one character it spells.
func TestALoneSurrogateEscapeIsRefusedAsText(t *testing.T) {
	declare := func(title string) string {
		return `[{"id": "a", "title": "` + title + `", "kind": "xy", "y": {"series": "s"}}]`
	}
	for _, title := range []string{`\ud800`, `a\udc00b`, `\ud800\u0041`, `\ud83d\ude00\ud83d`} {
		want := []Refusal{{Field: "charts[0].title", Message: "charts[0].title must be single-line plain text (nonempty)"}}
		if got := validate(t, declare(title)); len(got) != 1 || got[0] != want[0] {
			t.Errorf("title %s: got %s, want %s", title, show(got), show(want))
		}
	}
	for _, title := range []string{`\ud83d\ude00`, `\uD83D\uDE00 pair`, `Wait \u00e9`} {
		if got := validate(t, declare(title)); len(got) != 0 {
			t.Errorf("title %s: got %s, want none", title, show(got))
		}
	}
}
