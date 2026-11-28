package text

import (
	"strings"
	"testing"
)

func TestStorableReplacement(t *testing.T) {
	for input, want := range map[string]string{
		"clean":       "clean",
		"a\x00b":      "a\uFFFDb",
		"a\xffb\x00c": "a\uFFFDb\uFFFDc",
	} {
		if got := StorableReplacement(input); got != want {
			t.Errorf("StorableReplacement(%q) = %q, want %q", input, got, want)
		} else if !Storable(got) {
			t.Errorf("StorableReplacement(%q) = %q, which is not storable", input, got)
		}
	}
}

// NulInJSON finds the first string holding U+0000 wherever it stands in a JSON value: a member at
// the top, a member of a nested object, an element of an array, an element of an array inside an
// array, a member of an array's object, and a member's name, which it names with the six
// characters \u0000 so the place carries no U+0000. It reads every string the value writes, not
// the value a decoder keeps: a repeated member's earlier copy is found whatever the later one
// holds, since a later null leaves a string field as the earlier copy set it and a later object
// merges into a struct field's first copy. A bare string stands at prefix, "body" when prefix is
// empty, and a prefix names every place under it. It answers the string itself, so each caller
// says where in it the U+0000 stands in its own count. The controls find nothing: the same members
// with no U+0000, repeated members holding none, a string holding the six characters \u0000 as
// text, and a value that stops being JSON before its U+0000, which a decoder refuses on its own.
func TestNulInJSONFindsTheFirstStringHoldingANulWhereverItStands(t *testing.T) {
	for _, row := range []struct {
		name, prefix, data, place, value string
	}{
		{name: "a top-level member", data: `{"planReason":"fit\u0000"}`, place: "planReason", value: "fit\x00"},
		{name: "a nested object's member", data: `{"plan":{"source":"own\u0000er"}}`, place: "plan.source", value: "own\x00er"},
		{name: "an array element", data: `{"labels":["ok","b\u0000"]}`, place: "labels[1]", value: "b\x00"},
		{name: "a member of an array's object", data: `{"options":[{"label":"a"},{"label":"é\u0000"}]}`, place: "options[1].label", value: "é\x00"},
		{name: "a member of an object in an array's object", data: `{"ops":[{"attributes":{"title":"t\u0000"}}]}`, place: "ops[0].attributes.title", value: "t\x00"},
		{name: "an element of an array in an array", data: `{"grid":[["ok"],["ok","é\u0000"]]}`, place: "grid[1][1]", value: "é\x00"},
		{name: "a member a later null repeats", data: `{"planReason":"fit\u0000","planReason":null}`, place: "planReason", value: "fit\x00"},
		{name: "a member a later string repeats", data: `{"planReason":"fit\u0000","planReason":"fit"}`, place: "planReason", value: "fit\x00"},
		{name: "an object a later one repeats", data: `{"actor":{"origin":{"cwd":"a\u0000b"}},"actor":{"kind":"session"}}`, place: "actor.origin.cwd", value: "a\x00b"},
		{name: "a member's name", data: `{"a\u0000b":"x"}`, place: `a\u0000b`, value: "a\x00b"},
		{name: "the first of two", data: `{"title":"a\u0000","body":"b\u0000"}`, place: "title", value: "a\x00"},
		{name: "a bare string", data: `"a\u0000b"`, place: "body", value: "a\x00b"},
		{name: "a bare string under a prefix", prefix: "actor", data: `"a\u0000b"`, place: "actor", value: "a\x00b"},
		{name: "a member under a prefix", prefix: "actor", data: `{"id":"s\u0000"}`, place: "actor.id", value: "s\x00"},
	} {
		t.Run(row.name, func(t *testing.T) {
			place, value, found := NulInJSON(row.prefix, []byte(row.data))
			if !found || place != row.place || value != row.value {
				t.Fatalf("NulInJSON(%q, %s) = %q, %q, %t; want %q, %q, true", row.prefix, row.data, place, value, found, row.place, row.value)
			}
			if strings.Contains(place, "\x00") {
				t.Fatalf("the place %q carries the U+0000 itself", place)
			}
		})
	}

	for _, data := range []string{
		`{"planReason":"fit","plan":{"source":"owner"},"labels":["ok","b"],"tasks":[{"text":"a"},{"text":"é"}],"grid":[["ok"],["b"]]}`,
		`{"planReason":"fit\\u0000"}`,
		`{"planReason":"fit\\u0000","planReason":null,"plan":{"source":"owner"},"plan":{},"grid":[["ok","b"]]}`,
		`{"title":"ok",}"\u0000"`,
		`["ok" "\u0000"]`,
	} {
		if place, value, found := NulInJSON("", []byte(data)); found {
			t.Errorf("NulInJSON(%s) = %q, %q, true; want nothing found", data, place, value)
		}
	}
}
