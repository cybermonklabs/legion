package legionfacts

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"
	"testing"
)

// TestAStoredObjectOfNumbersDecodesAsEncodingJSONDecodesIt: the read of a row's hours by column
// and its growth (decodeNumbers) answers, for every object of numbers, the map encoding/json
// decodes it to, key for key and each value to its last bit, in the form jsonb writes an object
// (a space after each colon and comma, its numbers as numeric writes them) and in any other
// whitespace and number form JSON allows: the empty object, one key, many keys, keys written with
// escapes and beyond ASCII, a key written twice (the last value stands, as encoding/json keeps
// it), negative values and negative zero, exponents, and integers past an int64.
//
// It refuses everything else with an error, every malformed input encoding/json refuses among
// them. Two inputs it refuses encoding/json takes, null and a null value, which it decodes to no
// map and to 0: neither column holds either, since each is `jsonb not null default '{}'` and is
// written from a Go map of numbers (upsert's storedMap), so a row that held one would be a row
// this package never wrote.
func TestAStoredObjectOfNumbersDecodesAsEncodingJSONDecodesIt(t *testing.T) {
	t.Parallel()
	var many strings.Builder
	many.WriteString("{")
	for index := range 20 {
		if index > 0 {
			many.WriteString(", ")
		}
		fmt.Fprintf(&many, `"column_%02d": %g`, 19-index, float64(index)*0.3125)
	}
	many.WriteString("}")
	decoded := []struct{ name, stored string }{
		{"the empty object", `{}`},
		{"the empty object in whitespace", " \t{ \n}\r\n"},
		{"one key", `{"implementing": 1.5}`},
		{"a row's hours as jsonb writes them", `{"queue": 1.5, "design": 2.25, "merging": 0.5, "testing": 0.75, "reviewing": 1.25, "implementing": 3.5}`},
		{"a row's growth as jsonb writes it", `{"slot_hours": 1, "work_hours": 1, "work_last_at": 1, "phase_hours.implementing": 1}`},
		{"many keys", many.String()},
		{"compact", `{"a":1,"b":2.5,"c":-3}`},
		{"whitespace everywhere", "\n{\t\"a\" :\r 1 ,\n \"b\":2\n}\n"},
		{"keys written with escapes", `{"a\"b": 1, "back\\slash": 2, "tab\tkey": 3, "\u00e9t\u00e9": 4, "\ud83d\ude00": 5, "\/": 6, "\b\f\n\r": 7}`},
		{"a lone surrogate, which decodes to the replacement character", `{"\ud800": 1}`},
		{"keys beyond ASCII", `{"été": 1, "✓": 2}`},
		{"the empty key", `{"": 1}`},
		{"a key written twice", `{"a": 1, "a": 2}`},
		{"negative values and negative zero", `{"a": -2.75, "b": -0, "c": -0.0}`},
		{"exponents", `{"a": 1e-7, "b": 2.5E+10, "c": -3e2, "d": 1.25e-300, "e": 5e-324, "f": 1.7976931348623157e308}`},
		{"a number under float64's least, which is 0", `{"a": 1e-400, "b": -1e-400}`},
		{"integers", `{"a": 0, "b": 42, "c": 12345678901234567890, "d": -7}`},
		{"numbers as numeric writes them", `{"a": 0.0000001, "b": 1000000000000000000000, "c": 0.30000000000000004}`},
	}
	for _, row := range decoded {
		var want map[string]float64
		if err := json.Unmarshal([]byte(row.stored), &want); err != nil {
			t.Fatalf("%s: encoding/json refuses %q: %v", row.name, row.stored, err)
		}
		got, err := decodeNumbers([]byte(row.stored))
		switch {
		case err != nil:
			t.Errorf("%s: decodeNumbers(%q) refuses it: %v; encoding/json decodes %v", row.name, row.stored, err, want)
		case got == nil:
			t.Errorf("%s: decodeNumbers(%q) answers no map; encoding/json decodes %v", row.name, row.stored, want)
		case !maps.EqualFunc(got, want, func(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }):
			t.Errorf("%s: decodeNumbers(%q) = %v; encoding/json decodes %v", row.name, row.stored, got, want)
		}
	}

	refused := []struct {
		name, stored string
		// taken is whether encoding/json decodes the input all the same.
		taken bool
	}{
		{"no input", ``, false},
		{"whitespace alone", ` `, false},
		{"null", `null`, true},
		{"a null value", `{"a": null}`, true},
		{"an array", `[1]`, false},
		{"a number", `1`, false},
		{"a string value", `{"a": "1"}`, false},
		{"a true value", `{"a": true}`, false},
		{"an object value", `{"a": {}}`, false},
		{"an array value", `{"a": [1]}`, false},
		{"a trailing comma", `{"a": 1,}`, false},
		{"a leading comma", `{, "a": 1}`, false},
		{"no colon", `{"a" 1}`, false},
		{"no comma", `{"a": 1 "b": 2}`, false},
		{"a key without quotes", `{a: 1}`, false},
		{"a number key", `{1: 1}`, false},
		{"a leading zero", `{"a": 01}`, false},
		{"a plus sign", `{"a": +1}`, false},
		{"no digit before the point", `{"a": .5}`, false},
		{"no digit after the point", `{"a": 1.}`, false},
		{"no digit in the exponent", `{"a": 1e}`, false},
		{"a sign alone", `{"a": -}`, false},
		{"NaN", `{"a": NaN}`, false},
		{"an infinity", `{"a": Infinity}`, false},
		{"a number past float64", `{"a": 1e400}`, false},
		{"something after the object", `{"a": 1} x`, false},
		{"a second close", `{"a": 1}}`, false},
		{"no close", `{"a": 1`, false},
		{"a key cut short", `{"a`, false},
		{"an open brace alone", `{`, false},
		{"a control character in a key", "{\"a\x01\": 1}", false},
		{"an escape JSON has none of", `{"\q": 1}`, false},
		{"a short unicode escape", `{"\u12": 1}`, false},
	}
	for _, row := range refused {
		var taken map[string]float64
		if err := json.Unmarshal([]byte(row.stored), &taken); (err == nil) != row.taken {
			t.Errorf("%s: encoding/json's answer to %q is %v, %v; the table says it takes it: %v", row.name, row.stored, taken, err, row.taken)
		}
		if got, err := decodeNumbers([]byte(row.stored)); err == nil {
			t.Errorf("%s: decodeNumbers(%q) = %v, want it refused", row.name, row.stored, got)
		}
	}
	t.Logf("%d objects decoded as encoding/json decodes them, %d inputs refused", len(decoded), len(refused))
}
