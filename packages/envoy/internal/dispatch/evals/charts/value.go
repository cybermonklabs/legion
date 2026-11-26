package charts

import (
	"cmp"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// The checks read a declaration as Python's json.loads builds it, because their answers depend on
// it: a JSON number with a fraction or an exponent is a float and any other is an int of any size
// (10^400 stays an int, which no float holds), an object keeps its keys in the order they came,
// and messages name some keys in that order. A decoded value is one of:
//
//	nil        null
//	bool       true or false
//	*big.Int   an int
//	float64    a float, ±Inf for one past a float's range, as Python's float() reads it
//	string     a string
//	[]any      a list
//	*object    an object

// object is a JSON object in its keys' order. A key given twice keeps its first place and its last
// value, as a Python dict does.
type object struct {
	keys   []string
	values map[string]any
}

func newObject() *object {
	return &object{values: map[string]any{}}
}

func (o *object) set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// has is Python's `key in o`.
func (o *object) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

// get is Python's o.get(key): nil for a missing key and for a null alike.
func (o *object) get(key string) any {
	return o.values[key]
}

// getOr is Python's o.get(key, fallback): the fallback only for a missing key, never for a null.
func (o *object) getOr(key string, fallback any) any {
	if value, ok := o.values[key]; ok {
		return value
	}
	return fallback
}

func (o *object) size() int {
	return len(o.keys)
}

// unknown is Python's sorted(set(o) - known): o's keys outside known, in code point order.
func (o *object) unknown(known ...string) []string {
	var fields []string
	for _, key := range o.keys {
		if !slices.Contains(known, key) {
			fields = append(fields, key)
		}
	}
	slices.Sort(fields)
	return fields
}

// only is Python's {key: value for key, value in o.items() if key in keep}.
func (o *object) only(keep ...string) *object {
	kept := newObject()
	for _, key := range o.keys {
		if slices.Contains(keep, key) {
			kept.set(key, o.values[key])
		}
	}
	return kept
}

// isNumber is Python's `type(value) in (int, float)`.
func isNumber(value any) bool {
	switch value.(type) {
	case *big.Int, float64:
		return true
	}
	return false
}

// intFloat is Python's float() of an int: the nearest float, ±Inf where Python raises OverflowError.
func intFloat(whole *big.Int) float64 {
	value, _ := new(big.Float).SetInt(whole).Float64()
	return value
}

// finite is the platform's _finite: an int or a float, never a bool, that a float holds and that
// is neither NaN nor infinite.
func finite(value any) bool {
	switch value := value.(type) {
	case *big.Int:
		return !math.IsInf(intFloat(value), 0)
	case float64:
		return !math.IsInf(value, 0) && !math.IsNaN(value)
	}
	return false
}

// asFloat is Python's float() of a number that finite passed.
func asFloat(value any) float64 {
	if whole, ok := value.(*big.Int); ok {
		return intFloat(whole)
	}
	return value.(float64)
}

// compare orders two numbers exactly, as Python compares an int with a float: -1, 0 or 1. Each is
// an int, a float or a bool (Python's bool is an int).
func compare(a, b any) int {
	aInf, bInf := infinity(a), infinity(b)
	if aInf != 0 || bInf != 0 {
		return cmp.Compare(aInf, bInf)
	}
	return exact(a).Cmp(exact(b))
}

func infinity(value any) int {
	if value, ok := value.(float64); ok && math.IsInf(value, 0) {
		if value > 0 {
			return 1
		}
		return -1
	}
	return 0
}

// exact is a number as a fraction, which holds an int of any size and every float exactly.
func exact(value any) *big.Rat {
	switch value := value.(type) {
	case *big.Int:
		return new(big.Rat).SetInt(value)
	case bool:
		if value {
			return big.NewRat(1, 1)
		}
		return new(big.Rat)
	}
	return new(big.Rat).SetFloat64(value.(float64))
}

// above is Python's a > b of two numbers.
func above(a, b any) bool { return compare(a, b) > 0 }

// truthy is Python's bool(value).
func truthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case *big.Int:
		return value.Sign() != 0
	case float64:
		return value != 0
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	}
	return value.(*object).size() > 0
}

// equal is Python's a == b where one side is a string or a number, which is every comparison the
// checks make: bools, ints and floats compare as numbers (True == 1 == 1.0), strings as strings,
// and nothing else equals either.
func equal(a, b any) bool {
	if numeric(a) && numeric(b) {
		return compare(a, b) == 0
	}
	text, ok := a.(string)
	return ok && is(b, text)
}

// numeric is Python's isinstance(value, (int, float)), which a bool is too.
func numeric(value any) bool {
	switch value.(type) {
	case bool, *big.Int, float64:
		return true
	}
	return false
}

// is is Python's value == text for a string text: true only for that string.
func is(value any, text string) bool {
	s, ok := value.(string)
	return ok && s == text
}

// isOneOf is Python's `value in (texts...)`.
func isOneOf(value any, texts ...string) bool {
	s, ok := value.(string)
	return ok && slices.Contains(texts, s)
}

// contains is Python's `value in list`.
func contains(list []any, value any) bool {
	return slices.ContainsFunc(list, func(item any) bool { return equal(item, value) })
}

// asObject is Python's isinstance(value, dict).
func asObject(value any) (*object, bool) {
	fields, ok := value.(*object)
	return fields, ok
}

// objectOr is Python's `value if isinstance(value, dict) else {}`.
func objectOr(value any) *object {
	if fields, ok := value.(*object); ok {
		return fields
	}
	return newObject()
}

// listOr is Python's `value if isinstance(value, list) else []`.
func listOr(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

// dumps is Python's json.dumps(value), as messages print a name: ", " and ": " between items, and
// each character outside printable ASCII as \uXXXX.
func dumps(value any) string {
	var out strings.Builder
	writeJSON(&out, value)
	return out.String()
}

func writeJSON(out *strings.Builder, value any) {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case *big.Int:
		out.WriteString(value.String())
	case float64:
		out.WriteString(floatRepr(value))
	case string:
		writeString(out, value)
	case []any:
		out.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				out.WriteString(", ")
			}
			writeJSON(out, item)
		}
		out.WriteByte(']')
	case *object:
		out.WriteByte('{')
		for index, key := range value.keys {
			if index > 0 {
				out.WriteString(", ")
			}
			writeString(out, key)
			out.WriteString(": ")
			writeJSON(out, value.values[key])
		}
		out.WriteByte('}')
	}
}

func writeString(out *strings.Builder, text string) {
	out.WriteByte('"')
	for char := range codePoints(text) {
		switch char {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			switch {
			case ' ' <= char && char <= '~':
				out.WriteRune(char)
			case char > 0xFFFF:
				char -= 0x10000
				fmt.Fprintf(out, `\u%04x\u%04x`, 0xD800+(char>>10), 0xDC00+(char&0x3FF))
			default:
				fmt.Fprintf(out, `\u%04x`, char)
			}
		}
	}
	out.WriteByte('"')
}

// floatRepr is Python's repr() of a float, which json.dumps writes: the shortest digits that read
// back as the same float, in exponent form below 1e-4 and from 1e16 up, else with a ".0" where it
// has no fraction.
func floatRepr(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	exponent, _ := strconv.Atoi(scientific[strings.IndexByte(scientific, 'e')+1:])
	if exponent < -4 || exponent >= 16 {
		return scientific
	}
	fixed := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed
}
