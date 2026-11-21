package leakguard

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// jsonBody is the records of record, a request body GitHub parses as JSON: the strings of the
// document where its text is exactly one (jsonFields), and record itself, raw, where it is not.
// A text that is not one document is no body GitHub parses, and is read as the one record it
// was: a Markdown body named by `--input` by mistake is still read.
//
// Three of a gh call's records are such a body: `gh api --input <file>`, whose whole file is the
// request body; standard input named by `gh api --input -`; and the standard input of `gh
// workflow run --json`, the run's inputs as one JSON object. The shared check's exit-4 handshake
// has buffered either standard input. A `-F <name>=@<path>` field's file is not such a body,
// since gh makes one JSON string of its bytes, escaping them itself: the file holds the text
// GitHub shows.
func jsonBody(record Record, graphQL bool) []Record {
	if fields, ok := jsonFields(record, graphQL); ok {
		return fields
	}
	return []Record{record}
}

// jsonFields is the records of the strings record's text holds where that text is exactly one
// JSON document, and false where it is not: no JSON at all, a document with more after it, or
// two documents. A string is one record, an object's names and values alike, labelled
// "<record>, JSON <path>" and a name "<record>, JSON <path> (name)", the record's label cut once
// before the walk (cutLabel) and the path written as jq writes it (`.body`, `.comments[0].body`,
// `."a b"`, and `.` for the document itself), but for an element of a document that is itself an
// array, whose index stands with no `.` before it (`[1]`, `[0][0]`, where jq writes `.[1]`),
// within the bounds jsonLabel keeps. An empty string makes no record: it holds no text a rule can
// match.
//
// A body is read one string at a time because GitHub stores it one string at a time: a review's
// body and each of its comments' bodies are texts of their own, shown apart, and a rule that
// needs two words on one line (`<name>[^\n]*<word>`) matches only where one stored text holds
// both. A compact document puts every field on one line, so a raw read of it would join a name
// in the review's body to an unrelated word in its fifth comment, and refuse a write that
// publishes neither beside the other.
//
// No raw read of one document is needed beside its strings. Every text GitHub stores from a JSON
// body is a decoded string value, and the bytes between the strings — the punctuation, the
// numbers, `true`, `false`, `null` and the whitespace — hold none: a number and the three
// literals are values of the schema's, stored as no text of the caller's, and make no record. The
// decoded string is also the one a rule must read: a string's escapes hide its text from the
// rules, `\u0074` being a letter no pattern reads in the bytes, and an escape inside a name parts
// the halves of a word. The document is read as tokens rather than as a value, so every value of
// a duplicate key is read, though a parser keeps one of them: which one is GitHub's parser's to
// say, and the guard does not guess.
//
// In a `gh api graphql` call (graphQL), the document's top-level `query` is a GraphQL query, and
// is read one literal at a time (graphQLRecords), labelled "<record>, JSON .query, GraphQL string
// <k>".
func jsonFields(record Record, graphQL bool) ([]Record, bool) {
	decoder := json.NewDecoder(strings.NewReader(record.Text))
	// A number is kept as its text rather than parsed, so that one past a float64's range is a
	// number still, and not an error that sends the document to the raw read.
	decoder.UseNumber()
	var fields []Record
	base := cutLabel(record.Label)
	// open are the arrays and objects the next token stands inside, the outermost first, and path
	// the steps from the document to the innermost of them, one for each but the document itself.
	// A label is rendered from path when its string is read (jsonLabel), so that no string's
	// label costs the length of the path it stands at.
	var open []jsonOpen
	var path []string
read:
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		// at is the path of the value token is or opens: path and, inside an array or an object,
		// the step to that value.
		at := path
		if n := len(open); n > 0 {
			inner := &open[n-1]
			switch {
			case token == json.Delim('}') || token == json.Delim(']'):
				open = open[:n-1]
				if len(open) == 0 {
					break read
				}
				path = path[:len(path)-1]
				continue
			case inner.array:
				at = append(path, "["+strconv.Itoa(inner.next)+"]")
				inner.next++
			case inner.member == "":
				// The decoder hands an object nothing but a string where a name stands.
				name, _ := token.(string)
				inner.member = jqMember(name)
				if name != "" {
					fields = append(fields, Record{Label: jsonLabel(base, append(path, inner.member)) + " (name)", Text: name})
				}
				continue
			default:
				at = append(path, inner.member)
				inner.member = ""
			}
		}
		switch value := token.(type) {
		case json.Delim:
			if len(open) > 0 {
				path = at
			}
			open = append(open, jsonOpen{array: value == '['})
			continue
		case string:
			if value == "" {
				break
			}
			field := Record{Label: jsonLabel(base, at), Text: value}
			if graphQL && len(at) == 1 && at[0] == "."+graphQLQuery {
				fields = append(fields, queryRecords(field)...)
			} else {
				fields = append(fields, field)
			}
		}
		if len(open) == 0 {
			break
		}
	}
	// The document is the whole text only where what follows it is whitespace to the end.
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// jsonOpen is an array or an object of a JSON document being read (jsonFields): for an array,
// the index of its next element; and for an object, the step to the member whose value comes
// next (jqMember), "" where a name comes next.
type jsonOpen struct {
	array  bool
	next   int
	member string
}

// The bounds of a JSON path in a label (jsonLabel), so that a label stays short whatever the
// document is: a body can nest thousands of objects, and a label holding the whole path would
// cost each string of such a body the length of its path, which for the whole body grows with the
// square of its depth. A path of more than maxPathSteps steps is shown as its first
// pathStepsShownBefore steps, "…", and its last pathStepsShownAfter; and a step is shown to
// maxStepShown bytes (jqMember).
const (
	maxPathSteps         = 8
	pathStepsShownBefore = 2
	pathStepsShownAfter  = 5
	maxStepShown         = 64
)

// jsonLabel is the label of the string at path, its steps from the document, of the document
// record labels, within the bounds above.
func jsonLabel(record string, path []string) string {
	var label strings.Builder
	label.WriteString(record)
	label.WriteString(", JSON ")
	if len(path) == 0 {
		label.WriteString(".")
		return label.String()
	}
	if len(path) > maxPathSteps {
		for _, step := range path[:pathStepsShownBefore] {
			label.WriteString(step)
		}
		label.WriteString("…")
		path = path[len(path)-pathStepsShownAfter:]
	}
	for _, step := range path {
		label.WriteString(step)
	}
	return label.String()
}

// jqMember is the step of a jq path to the member name: `.name` for a name jq writes bare, a
// letter or an underscore and then letters, digits and underscores, and `."name"` quoted as JSON
// quotes it for every other, so that a name holding a newline or a quote cannot break the label
// it stands in. A step longer than maxStepShown bytes is cut there, on a rune boundary, and "…"
// marks the cut: a name can be a whole body long, and its step is in the label of every string
// beneath it.
func jqMember(name string) string {
	bare := name != ""
	for i, c := range name {
		letter := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		bare = bare && (letter || i > 0 && '0' <= c && c <= '9')
	}
	step := "." + name
	if !bare {
		var quoted strings.Builder
		encoder := json.NewEncoder(&quoted)
		encoder.SetEscapeHTML(false)
		// A string always encodes: the error is a writer's, and a Builder's write never fails.
		_ = encoder.Encode(name)
		step = "." + strings.TrimSuffix(quoted.String(), "\n")
	}
	if len(step) > maxStepShown {
		step = step[:runeBack(step, maxStepShown, 0)] + "…"
	}
	return step
}
