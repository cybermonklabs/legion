package texts

import (
	"strings"
	"testing"
)

// Each expected line is written by hand from the rules of encode of @toon-format/toon 4.1.1
// (isSafeUnquoted, escapeString, encodeKey), not taken from a run of it.
func TestTheEncoderQuotesEachHostileValueAsToonDoes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"true", "true", `v: "true"`},
		{"false", "false", `v: "false"`},
		{"null", "null", `v: "null"`},
		{"a leading hyphen", "-x", `v: "-x"`},
		{"a leading hash", "#x", `v: "#x"`},
		{"an exponent", "1e5", `v: "1e5"`},
		{"a leading zero", "0123", `v: "0123"`},
		{"a short commit that reads as a number", "123456e78901", `v: "123456e78901"`},
		{"a signed number", "+5", `v: "+5"`},
		{"a decimal", "1.5", `v: "1.5"`},
		{"a comma", "a,b", `v: "a,b"`},
		{"a colon", "a:b", `v: "a:b"`},
		{"a tab", "a\tb", `v: "a\tb"`},
		{"a new line", "a\nb", `v: "a\nb"`},
		{"a carriage return", "a\rb", `v: "a\rb"`},
		{"another control character", "a\x1bb", `v: "a\u001bb"`},
		{"a quote", `say "hi"`, `v: "say \"hi\""`},
		{"a backslash", `a\b`, `v: "a\\b"`},
		{"a bracket", "[x]", `v: "[x]"`},
		{"a brace", "{x}", `v: "{x}"`},
		{"a leading space", " a", `v: " a"`},
		{"a trailing space", "a ", `v: "a "`},
		{"a character outside the BMP", "a😀b", `v: a😀b`},
		{"an empty string", "", `v: ""`},
		{"an empty list", []any{}, `v: []`},
		{"a key and a hash inside", "cybermonklabs/legion#4211", `v: cybermonklabs/legion#4211`},
		{"a hyphen inside", "INFRA-3686", `v: INFRA-3686`},
		{"a hexadecimal commit", "5c2b01d9e8f7", `v: 5c2b01d9e8f7`},
		{"the bool true", true, `v: true`},
		{"a negative integer", int64(-5), `v: -5`},
		{"a nil value", nil, `v: null`},
	}
	for _, c := range cases {
		lines, err := encodeObject(object{{key: "v", value: c.value}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(lines, "\n"); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}

	keys := []struct{ key, want string }{
		{"pull_request", "pull_request: 1"},
		{"_ok.x", "_ok.x: 1"},
		{"a-b", `"a-b": 1`},
		{"9x", `"9x": 1`},
		{"a b", `"a b": 1`},
		{`k"q`, `"k\"q": 1`},
		// U+212A, the Kelvin sign, and U+017F, the long s, fold to k and s under Go's (?i), and
		// TOON's pattern, an ASCII one in JavaScript, takes neither.
		{"\u212a", "\"\u212a\": 1"},
		{"a\u017f", "\"a\u017f\": 1"},
	}
	for _, k := range keys {
		lines, err := encodeObject(object{{key: k.key, value: int64(1)}})
		if err != nil {
			t.Fatalf("key %q: %v", k.key, err)
		}
		if got := strings.Join(lines, "\n"); got != k.want {
			t.Errorf("key %q: got %s, want %s", k.key, got, k.want)
		}
	}

	if _, err := encodeObject(object{{key: "v", value: "a\xffb"}}); err == nil {
		t.Error("a string that is not valid UTF-8 was encoded; TOON refuses an unpaired surrogate")
	}
}

func TestTheEncoderWritesAListAndATableAsToonDoes(t *testing.T) {
	lines, err := encodeObject(object{
		{key: "kind", value: "ci-settled"},
		{key: "not_passed", value: []any{
			object{{key: "check", value: "daemon-go"}, {key: "conclusion", value: "failure"}},
			object{{key: "check", value: "ci:ok"}, {key: "conclusion", value: "fail, then"}},
		}},
		{key: "skipped", value: []any{"docs", "ci-filters", "a,b"}},
		{key: "passed", value: int64(12)},
		{key: "none", value: []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"kind: ci-settled",
		"not_passed[2]{check,conclusion}:",
		"  daemon-go,failure",
		`  "ci:ok","fail, then"`,
		`skipped[3]: docs,ci-filters,"a,b"`,
		"passed: 12",
		"none: []",
	}, "\n")
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTheEncoderRefusesAMapOfObjectsAndRowsThatDiffer(t *testing.T) {
	type row struct {
		Issue string `json:"issue"`
		Note  string `json:"note,omitempty"`
	}
	type withMap struct {
		Children map[string]row `json:"children"`
	}
	if text, err := Render(withMap{Children: map[string]row{"a": {Issue: "INFRA-1"}, "b": {Issue: "INFRA-2"}}}); err == nil {
		t.Errorf("a map of objects rendered as %q; TOON 4.1.1 writes it as a keyed table, which the records do not use", text)
	}

	type withRows struct {
		Rows []row `json:"rows"`
	}
	if text, err := Render(withRows{Rows: []row{{Issue: "INFRA-1", Note: "n"}, {Issue: "INFRA-2"}}}); err == nil {
		t.Errorf("rows with different keys rendered as %q; TOON 4.1.1 writes them as list items", text)
	}

	nested := object{{key: "rows", value: []any{
		object{{key: "issue", value: "INFRA-1"}, {key: "labels", value: []any{"a"}}},
	}}}
	if lines, err := encodeObject(nested); err == nil {
		t.Errorf("a row that holds a list encoded as %q; a table row is flat", lines)
	}

	mixed := object{{key: "items", value: []any{"a", object{{key: "issue", value: "INFRA-1"}}}}}
	if lines, err := encodeObject(mixed); err == nil {
		t.Errorf("a list of a primitive and a record encoded as %q", lines)
	}
}
