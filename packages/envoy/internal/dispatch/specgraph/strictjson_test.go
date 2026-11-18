package specgraph

import (
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
		{"a long member that the format does not name", `{"nodes":[{` + jsonLongName + `:1}]}`, &ApprovedFile{},
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
