package leakguard

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// graphQLQuery is the field of a `gh api graphql` call that holds the query: a field of the argv
// (`-f query=…`), the field a file is read into (`-F query=@<path>`, GhFile.Field), the field
// standard input is read into (`-F query=@-`, stdinRecords), or the member of a JSON body
// (`--input`). gh reads every other field of a `graphql` call but `operationName` as a variable,
// and sends it beside the query under any other spelling of the endpoint; either way its value is
// read as the field it is.
const graphQLQuery = "query"

// graphQLRecords is record read as a GraphQL query: one record for each string literal of its
// text, decoded, labelled "<record>, GraphQL string <k>" with k counted from 1, the record's label
// cut once before the literals are labelled (cutLabel), and true; or false where its text does not
// lex (graphQLStrings), which its caller reads as written (queryRecords).
//
// A query is a document GitHub parses, as a JSON body is (jsonFields). GitHub stores the value of
// each literal its arguments carry, decoded: the title and the body of a `createIssue` mutation
// are two strings GitHub stores apart, and a `\u006c` in a literal is the letter l in the stored
// text. The rest of a query is the schema's own words — operation, field, argument and variable
// names, enum values, numbers and punctuation — which shape the request and are stored as no
// text of the caller's. So a literal is a record of its own, and the query text is read as no
// record: the raw text would join the title and the body on one line, and would show a rule the
// escape rather than the letter. The shared check reads what GitHub stores, one value at a time,
// for the same reason (dispatch://INFRA-4383, R1). A query with no literal lexes to no record,
// since GitHub stores nothing of it.
func graphQLRecords(record Record) ([]Record, bool) {
	literals, ok := graphQLStrings(record.Text)
	if !ok {
		return nil, false
	}
	records := make([]Record, 0, len(literals))
	base := cutLabel(record.Label) + ", GraphQL string "
	for k, literal := range literals {
		records = append(records, Record{Label: base + strconv.Itoa(k+1), Text: literal})
	}
	return records, true
}

// queryRecords is record read as a GraphQL query, one literal at a time (graphQLRecords), and
// record itself where its text does not lex, as every query was read before literals were: a
// guard that cannot tell where a literal ends cannot tell which part of the text GitHub would
// store.
func queryRecords(record Record) []Record {
	if literals, lexed := graphQLRecords(record); lexed {
		return literals
	}
	return []Record{record}
}

// graphQLStrings is the decoded value of each string literal of source, a GraphQL document, in
// the order the document writes them, and false where source does not lex: a literal with no
// end, a line break inside a `"…"` literal, or an escape the GraphQL specification does not
// have (its section 2.9.4, String Value). Both forms of literal are read:
//
//   - `"…"`, whose escapes are `\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`, `\uXXXX` (two of
//     them for a code point past U+FFFF, as a surrogate pair) and `\u{…}`, any number of hex
//     digits naming one Unicode scalar value;
//   - `"""…"""`, a block string, whose one escape is `\"""` and whose value is its lines with
//     their common indentation and the blank lines around them taken off (graphQLBlockValue).
//
// A `#` outside a literal starts a comment, which runs to the end of its line and holds no
// literal: a `"` inside it opens nothing. Everything else outside a literal is skipped without a
// check of its own, since a character no GraphQL token has makes a document GitHub refuses, and a
// refused document stores no text.
func graphQLStrings(source string) ([]string, bool) {
	var literals []string
	for i := 0; i < len(source); {
		switch {
		case source[i] == '#':
			for i < len(source) && source[i] != '\n' && source[i] != '\r' {
				i++
			}
		case strings.HasPrefix(source[i:], `"""`):
			value, length, ok := graphQLBlockString(source[i+len(`"""`):])
			if !ok {
				return nil, false
			}
			literals = append(literals, value)
			i += len(`"""`) + length
		case source[i] == '"':
			value, length, ok := graphQLString(source[i+len(`"`):])
			if !ok {
				return nil, false
			}
			literals = append(literals, value)
			i += len(`"`) + length
		default:
			i++
		}
	}
	return literals, true
}

// graphQLString is the value of the `"…"` literal whose text, past its opening quote, begins s,
// and the length of that text with its closing quote. A literal with no escape is its own text,
// and one with escapes is its runs of plain text with each escape decoded between them.
func graphQLString(s string) (string, int, bool) {
	var value strings.Builder
	escaped := false
	// run is where the plain text not yet written to value starts.
	run := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			if !escaped {
				return s[:i], i + 1, true
			}
			value.WriteString(s[run:i])
			return value.String(), i + 1, true
		case '\n', '\r':
			return "", 0, false
		case '\\':
			if i+1 == len(s) {
				return "", 0, false
			}
			value.WriteString(s[run:i])
			escaped = true
			switch one, single := graphQLEscapes[s[i+1]]; {
			case single:
				value.WriteByte(one)
				i += 2
			case s[i+1] == 'u':
				r, length, ok := graphQLUnicode(s[i+2:])
				if !ok {
					return "", 0, false
				}
				value.WriteRune(r)
				i += 2 + length
			default:
				return "", 0, false
			}
			run = i
		default:
			// The literal's own bytes, as written: a byte that is no UTF-8 is one GitHub's parser
			// refuses or stores as it stands, and either way the rules read what is there.
			i++
		}
	}
	return "", 0, false
}

// graphQLEscapes are the escapes of a `"…"` literal that stand for one character, by the letter
// after the backslash.
var graphQLEscapes = map[byte]byte{
	'"':  '"',
	'\\': '\\',
	'/':  '/',
	'b':  '\b',
	'f':  '\f',
	'n':  '\n',
	'r':  '\r',
	't':  '\t',
}

// graphQLUnicode is the code point of the `\u` escape whose text, past the `\u`, begins s, and
// the length of that text. It is `{…}` with one or more hex digits, or four hex digits; four that
// name a leading surrogate are followed by a second `\u` and four digits that name a trailing one,
// and the two are one code point. A surrogate anywhere else, and a value past U+10FFFF, is no
// Unicode scalar value and does not lex.
func graphQLUnicode(s string) (rune, int, bool) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 2 {
			return 0, 0, false
		}
		value, ok := graphQLHex(s[1:end])
		if !ok || !utf8.ValidRune(value) {
			return 0, 0, false
		}
		return value, end + 1, true
	}
	if len(s) < 4 {
		return 0, 0, false
	}
	leading, ok := graphQLHex(s[:4])
	if !ok {
		return 0, 0, false
	}
	if leading < 0xD800 || leading > 0xDFFF {
		return leading, 4, true
	}
	if leading > 0xDBFF || len(s) < 10 || s[4:6] != `\u` {
		return 0, 0, false
	}
	trailing, ok := graphQLHex(s[6:10])
	if !ok || trailing < 0xDC00 || trailing > 0xDFFF {
		return 0, 0, false
	}
	return (leading-0xD800)<<10 + (trailing - 0xDC00) + 0x10000, 10, true
}

// graphQLHex is the value of digits, hex digits of either case, and false for anything else or
// for a value past the last code point. strconv reads no sign, prefix or underscore in base 16,
// and an empty digits is an error of its own, which no caller hands it: graphQLUnicode refuses
// `{}` and hands four digits otherwise.
func graphQLHex(digits string) (rune, bool) {
	value, err := strconv.ParseUint(digits, 16, 32)
	if err != nil || value > utf8.MaxRune {
		return 0, false
	}
	return rune(value), true
}

// graphQLBlockString is the value of the block string whose text, past its opening `"""`, begins
// s, and the length of that text with its closing `"""`. Its raw text is every character up to
// that close, with each `\"""` read as `"""`.
func graphQLBlockString(s string) (string, int, bool) {
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], `\"""`):
			i += len(`\"""`)
		case strings.HasPrefix(s[i:], `"""`):
			return graphQLBlockValue(strings.ReplaceAll(s[:i], `\"""`, `"""`)), i + len(`"""`), true
		default:
			i++
		}
	}
	return "", 0, false
}

// graphQLBlockValue is a block string's value, from its raw text, as the specification's
// BlockStringValue reads it: the lines, split at each line terminator; the least indentation of
// any line after the first that holds more than spaces and tabs, taken off each line after the
// first; then the leading and trailing lines of spaces and tabs alone dropped; and the lines left
// joined with line feeds.
func graphQLBlockValue(raw string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"), "\n")
	common := -1
	for _, line := range lines[1:] {
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < len(line) && (common < 0 || indent < common) {
			common = indent
		}
	}
	if common > 0 {
		for i := 1; i < len(lines); i++ {
			lines[i] = lines[i][min(common, len(lines[i])):]
		}
	}
	blank := func(line string) bool { return strings.Trim(line, " \t") == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
