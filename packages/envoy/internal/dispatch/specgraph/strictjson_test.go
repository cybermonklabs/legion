package specgraph

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"unicode"
)

// longName is a name of 262,144 bytes, as the writer of a map of a format can pick it, with a line
// break near its start and a tab near its end; jsonLongName is it as a JSON string.
var (
	longName     = "KEY\n" + strings.Repeat("y", 1<<18-8) + "\tEND"
	jsonLongName = `"KEY\n` + strings.Repeat("y", 1<<18-8) + `\tEND"`
)

// decodeJSON prints a JSON pointer whole up to 100 bytes, and else its first and its last 50
// bytes with … between them, each cut at a whole character, so a refusal stays one short line
// that names the place, also under a name of 256 KiB that a map of the format takes: for a value
// of another type, a member that the format does not name, and a name that one object gives two
// times. Each line holds no raw control character.
func TestDecodeJSONCutsALongPlace(t *testing.T) {
	t.Parallel()
	y := func(n int) string { return strings.Repeat("y", n) }
	for _, row := range []struct {
		name, json string
		v          any
		want       string
	}{
		{"a value of another type under a long name of a map", `{"c1":{` + jsonLongName + `:7}}`, &map[string]map[string][]int{},
			`it is not in the form <form>: the value at "/c1/KEY\n` + y(42) + `…` + y(46) + `\tEND" is of another type`},
		{"a long member that the format does not name", `{"nodes":[{` + jsonLongName + `:1}]}`, &approvedFile{},
			`member "/nodes/0/KEY\n` + y(37) + `…` + y(46) + `\tEND" is not in the format`},
		{"a long name that one object gives two times", `{` + jsonLongName + `:1,` + jsonLongName + `:2}`, &map[string]int{},
			`member "/KEY\n` + y(45) + `…` + y(46) + `\tEND" is in one object two times`},
		{"a place of 100 bytes, whole", `{"c1":{"` + y(96) + `":7}}`, &map[string]map[string][]int{},
			`it is not in the form <form>: the value at "/c1/` + y(96) + `" is of another type`},
		{"a place of 101 bytes, cut", `{"c1":{"` + y(97) + `":7}}`, &map[string]map[string][]int{},
			`it is not in the form <form>: the value at "/c1/` + y(46) + `…` + y(50) + `" is of another type`},
		{"a cut at a whole character", `{"c1":{"` + y(45) + `é` + y(100) + `é` + y(49) + `":7}}`, &map[string]map[string][]int{},
			`it is not in the form <form>: the value at "/c1/` + y(45) + `…` + y(49) + `" is of another type`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := decodeJSON([]byte(row.json), row.v, "<form>")
			if err == nil {
				t.Fatal("decodeJSON accepted the text")
			}
			expectOneShortLine(t, err.Error(), row.want)
		})
	}
}

// A JSON null says nothing, so decodeJSON refuses it at every place of every file of a format, with
// the JSON pointer of its place: a member of an object, a value of a map, an element of a list and
// the whole text, whatever type v gives that place, since the decoder would read null as the zero
// value of the type, a real value of the form. It refuses a null in a value that it keeps as JSON
// text too, such as an element of a file of a list. The text "null" in a string is a string.
func TestDecodeJSONRefusesNullAtEachPlace(t *testing.T) {
	t.Parallel()
	y := func(n int) string { return strings.Repeat("y", n) }
	for _, row := range []struct {
		name, json string
		v          any
		want       string // "" for no refusal
	}{
		{"a member of an object", `{"commits":null}`, &FixtureHistory{}, `it is not in the form <form>: the value at "/commits" is null`},
		{"a value of a map", `{"c1":{"packages/envoy/go.mod":null}}`, &map[string]map[string]string{},
			`it is not in the form <form>: the value at "/c1/packages~1envoy~1go.mod" is null`},
		{"a map of a map", `{"c1":null}`, &map[string]map[string]string{}, `it is not in the form <form>: the value at "/c1" is null`},
		{"an element of a list", `["a",null]`, &[]string{}, `it is not in the form <form>: the value at "/1" is null`},
		{"the whole text", `null`, &approvedFile{}, `it is not in the form <form>: it is null`},
		{"a member of an element that the reader keeps as JSON text", `[{"job":"go-checks","leg":null}]`, &[]jsontext.Value{},
			`it is not in the form <form>: the value at "/0/leg" is null`},
		{"a null under a long name of a map", `{"c1":{` + jsonLongName + `:null}}`, &map[string]map[string]string{},
			`it is not in the form <form>: the value at "/c1/KEY\n` + y(42) + `…` + y(46) + `\tEND" is null`},
		{"control: the same map with a string", `{"c1":{"packages/envoy/go.mod":"1"}}`, &map[string]map[string]string{}, ""},
		{"control: the text null in a string", `{"c1":{"packages/envoy/go.mod":"null"}}`, &map[string]map[string]string{}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := decodeJSON([]byte(row.json), row.v, "<form>")
			switch {
			case row.want == "" && err != nil:
				t.Fatalf("decodeJSON refused the text: %v", err)
			case row.want == "":
			case err == nil:
				t.Fatal("decodeJSON accepted the text")
			default:
				expectOneShortLine(t, err.Error(), row.want)
			}
		})
	}
}

// nullPlace reads a text again that decodeJSON read whole, so a text that it cannot read is an
// error, not a text with no null: an error read as "no null" would let a null through.
func TestNullPlaceGivesAnErrorForATextThatItCannotRead(t *testing.T) {
	t.Parallel()
	if place, null, err := nullPlace([]byte(`{"a":[1,`)); err == nil || null {
		t.Errorf(`nullPlace({"a":[1,) = %q, %t, %v; want an error`, place, null, err)
	}
	if place, null, err := nullPlace([]byte(`{"a":[1,null]}`)); err != nil || !null || place != "/a/1" {
		t.Errorf(`control: nullPlace({"a":[1,null]}) = %q, %t, %v; want "/a/1", true and no error`, place, null, err)
	}
	if place, null, err := nullPlace([]byte(`{"a":[1]}`)); err != nil || null {
		t.Errorf(`control: nullPlace({"a":[1]}) = %q, %t, %v; want no null and no error`, place, null, err)
	}
}

// expectOneShortLine checks that a refusal's line is want, under 600 bytes, with no raw control
// character.
func expectOneShortLine(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("the line is %d bytes, %.300q…\nwant %d bytes, %.300q…", len(got), got, len(want), want)
	}
	if len(got) >= 600 {
		t.Errorf("the line is %d bytes, want under 600", len(got))
	}
	if i := strings.IndexFunc(got, unicode.IsControl); i >= 0 {
		t.Errorf("the line holds the control character %q at byte %d", got[i], i)
	}
}
