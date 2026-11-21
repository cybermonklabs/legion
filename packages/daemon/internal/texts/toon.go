package texts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is a port of encode of @toon-format/toon 4.1.1 (dist/index.mjs of its tarball),
// with the default options: an indent of two spaces and the comma as the delimiter. It ports the
// functions that decide the form: isSafeUnquoted with its numeric-like pattern, escapeString,
// isValidUnquotedKey, encodeKey, formatHeader and the tabular test (extractTabularFields). It
// encodes three shapes and refuses every other shape:
//
//   - an object of primitive fields: a key: value line for each field;
//   - a list of primitives: key[n]: a,b on one line;
//   - a list of uniform flat records: a key[n]{f,g}: header, then one indented row per record.
//
// A primitive is nil (null), a string, a bool or an int64. Render makes these values from a
// record, so a time is already a string here.

const (
	toonIndent    = "  "
	toonDelimiter = ","
	nullLiteral   = "null"
)

// object is an object for the encoder: its members in order.
type object []member

// member is one field of an object. Its value is a primitive, or a []any whose items are all
// primitives or all objects.
type member struct {
	key   string
	value any
}

// numericLike is NUMERIC_LIKE_PATTERN of 4.1.1.
var numericLike = regexp.MustCompile(`(?i)^[+-]?\d+(?:\.\d+)?(?:e[+-]?\d+)?$`)

// unquotedKey is the pattern of isValidUnquotedKey of 4.1.1, /^[A-Z_][\w.]*$/i, which in
// JavaScript holds ASCII letters alone. Go's case folding under (?i) would also take a letter
// such as U+212A (the Kelvin sign, which folds to k), so the letters are spelled out.
var unquotedKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// encodeObject writes obj as the lines of a document root, as encodeObjectLines does at depth 0.
func encodeObject(obj object) ([]string, error) {
	lines := make([]string, 0, len(obj))
	for _, m := range obj {
		var err error
		if lines, err = encodeMember(lines, m); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// encodeMember appends the lines of one member of the root, as encodeKeyValuePairLines does.
func encodeMember(lines []string, m member) ([]string, error) {
	if err := assertWellFormed(m.key, "key"); err != nil {
		return nil, err
	}
	if items, ok := m.value.([]any); ok {
		return encodeList(lines, m.key, items)
	}
	value, err := encodePrimitive(m.value)
	if err != nil {
		return nil, fmt.Errorf("texts: field %s: %w", m.key, err)
	}
	return append(lines, encodeKey(m.key)+": "+value), nil
}

// encodeList appends the lines of a list member, as encodeArrayLines does for the shapes this
// encoder takes.
func encodeList(lines []string, key string, items []any) ([]string, error) {
	if len(items) == 0 {
		return append(lines, encodeKey(key)+": []"), nil
	}
	if isListOfPrimitives(items) {
		values, err := encodeAndJoinPrimitives(items)
		if err != nil {
			return nil, fmt.Errorf("texts: list %s: %w", key, err)
		}
		return append(lines, formatHeader(len(items), key, nil)+" "+values), nil
	}
	rows := make([]object, len(items))
	for i, item := range items {
		row, ok := item.(object)
		if !ok {
			return nil, fmt.Errorf("texts: list %s: item %d is neither a primitive nor a record of a list of records", key, i)
		}
		rows[i] = row
	}
	fields, ok := tabularFields(rows)
	if !ok {
		return nil, fmt.Errorf("texts: list %s: its records are not uniform flat records, and TOON would write them as list items", key)
	}
	lines = append(lines, formatHeader(len(rows), key, fields))
	for i, row := range rows {
		leaves := make([]any, len(fields))
		for j, field := range fields {
			leaves[j], _ = row.get(field)
		}
		values, err := encodeAndJoinPrimitives(leaves)
		if err != nil {
			return nil, fmt.Errorf("texts: list %s: row %d: %w", key, i, err)
		}
		lines = append(lines, toonIndent+values)
	}
	return lines, nil
}

// tabularFields is extractTabularFields of 4.1.1 for flat rows: the keys of the first row, where
// that row has a key, every row has exactly those keys, and every value under them is a
// primitive. A column of objects, which 4.1.1 would write as a nested header, is not flat, so it
// fails the test here.
func tabularFields(rows []object) ([]string, bool) {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil, false
	}
	fields := make([]string, len(rows[0]))
	for i, m := range rows[0] {
		fields[i] = m.key
	}
	for _, row := range rows {
		if len(row) != len(fields) {
			return nil, false
		}
		for _, field := range fields {
			value, ok := row.get(field)
			if !ok || !isPrimitive(value) {
				return nil, false
			}
		}
	}
	return fields, true
}

// get returns the value of the member named key.
func (o object) get(key string) (any, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

func isPrimitive(value any) bool {
	switch value.(type) {
	case nil, string, bool, int64:
		return true
	}
	return false
}

func isListOfPrimitives(items []any) bool {
	for _, item := range items {
		if !isPrimitive(item) {
			return false
		}
	}
	return true
}

// encodeAndJoinPrimitives is encodeAndJoinPrimitives of 4.1.1.
func encodeAndJoinPrimitives(values []any) (string, error) {
	encoded := make([]string, len(values))
	for i, value := range values {
		var err error
		if encoded[i], err = encodePrimitive(value); err != nil {
			return "", err
		}
	}
	return strings.Join(encoded, toonDelimiter), nil
}

// encodePrimitive is encodePrimitive of 4.1.1.
func encodePrimitive(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return nullLiteral, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case string:
		if err := assertWellFormed(v, "string value"); err != nil {
			return "", err
		}
		return encodeStringLiteral(v), nil
	}
	return "", fmt.Errorf("a value of type %T is not a primitive", value)
}

// assertWellFormed is assertNoLoneSurrogate of 4.1.1. A Go string holds UTF-8, where an
// unpaired surrogate can only be an invalid sequence, so a string that is not valid UTF-8 is
// refused.
func assertWellFormed(value, context string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("cannot encode a %s that is not valid UTF-8: %q", context, value)
	}
	return nil
}

// encodeStringLiteral is encodeStringLiteral of 4.1.1.
func encodeStringLiteral(value string) string {
	if isSafeUnquoted(value) {
		return value
	}
	return `"` + escapeString(value) + `"`
}

// encodeKey is encodeKey of 4.1.1.
func encodeKey(key string) string {
	if isValidUnquotedKey(key) {
		return key
	}
	return `"` + escapeString(key) + `"`
}

// isValidUnquotedKey is isValidUnquotedKey of 4.1.1.
func isValidUnquotedKey(key string) bool {
	return unquotedKey.MatchString(key)
}

// isSafeUnquoted is isSafeUnquoted of 4.1.1 with the comma as the delimiter.
func isSafeUnquoted(value string) bool {
	switch {
	case value == "":
		return false
	case value[0] == ' ' || value[0] == '\t' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t':
		return false
	case value == "true" || value == "false" || value == nullLiteral || numericLike.MatchString(value):
		return false
	case strings.Contains(value, ":"):
		return false
	case strings.ContainsAny(value, `"\`):
		return false
	case strings.ContainsAny(value, "[]{}"):
		return false
	case hasControlCharacter(value):
		return false
	case strings.Contains(value, toonDelimiter):
		return false
	case value[0] == '-' || value[0] == '#':
		return false
	}
	return true
}

// hasControlCharacter is the test /[\u0000-\u001F]/ of 4.1.1.
func hasControlCharacter(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 {
			return true
		}
	}
	return false
}

// escapeString is escapeString of 4.1.1: the backslash, the double quote, the new line, the
// carriage return and the tab take a backslash escape, and every other control character below
// U+0020 takes a \u escape of four lower-case hexadecimal digits.
func escapeString(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	for i := range len(value) {
		c := value[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// formatHeader is formatHeader of 4.1.1 with a key, the comma as the delimiter and no keyed
// form: the key, the length in brackets, the field names in braces where the list is a table,
// and a colon.
func formatHeader(length int, key string, fields []string) string {
	header := encodeKey(key) + "[" + strconv.Itoa(length) + "]"
	if fields != nil {
		names := make([]string, len(fields))
		for i, field := range fields {
			names[i] = encodeKey(field)
		}
		header += "{" + strings.Join(names, toonDelimiter) + "}"
	}
	return header + ":"
}
