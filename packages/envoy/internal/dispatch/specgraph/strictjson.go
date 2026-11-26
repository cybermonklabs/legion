package specgraph

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// decodeJSON decodes data, one JSON value, into v, and form is the format as a person reads it,
// such as approvedForm, which it names for a value of another type. It is the one reader of every
// JSON file of a format of this package (approved.json, approvedFile, and each JSON file of a
// scenario), and it refuses each text that the format does not hold:
//
//   - a byte that is not UTF-8 (RFC 8259 §8.1: JSON text "MUST be encoded using UTF-8"): "it is not
//     UTF-8";
//   - a member that v's type does not name, also one whose name differs from the format's in the
//     case of a letter, since encoding/json/v2 matches a name to a field with regard to case:
//     "member "<JSON pointer>" is not in the format";
//   - a member name that one object gives two times: "member "<JSON pointer>" is in one object two
//     times";
//   - no value, or text after the value: "it holds no JSON value", "text after the JSON value";
//   - a value of another type than the form needs, such as a string in the place of a number or a
//     time that time.Time does not read as RFC 3339: "it is not in the form <form>: the value at
//     "<JSON pointer>" is of another type", and "it is not in the form <form>" for the whole text.
//     The words are the package's, since encoding/json/v2 names a Go type, and picks its verb,
//     "cannot" or "unable to", once in each process;
//   - a JSON null at any place, a member, a value of a map, an element of a list or the whole text,
//     also one that v keeps as JSON text: "it is not in the form <form>: the value at "<JSON
//     pointer>" is null", and "it is not in the form <form>: it is null" for the whole text. A null
//     says nothing, and the decoder would read it as the zero value of its place, a real value of
//     the form, such as the empty label of a file or no run of a job; no member of a format takes
//     null;
//   - in each of those lines, "<JSON pointer>" is the place as quoted prints it: whole up to 100
//     bytes, else its first and its last 50 bytes with … between them, since a map of a format
//     takes the names that its writer picks, and a long name would make the line as long;
//   - a text that is not JSON, in the words of encoding/json/jsontext, which name the place too.
//
// So no text that the format does not hold reads as a text that it holds: without these refusals,
// a name in other letters, a name given two times, a null or text after the value would read as
// the value of another text of the format.
//
// A map of the format, such as the paths of a commit's tree, takes any name, in its own letters,
// one time. The report of go test -json is no format of this package: ReadGoTestJSON reads it as
// go test writes it, and passes over the members that it does not name.
func decodeJSON(data []byte, v any, form string) error {
	if !utf8.Valid(data) {
		return errors.New("it is not UTF-8")
	}
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	err := json.UnmarshalDecode(dec, v, json.RejectUnknownMembers(true))
	var semantic *json.SemanticError
	var syntactic *jsontext.SyntacticError
	switch {
	case errors.Is(err, io.EOF):
		return errors.New("it holds no JSON value")
	case errors.As(err, &semantic) && errors.Is(err, json.ErrUnknownName):
		return fmt.Errorf("member %s is not in the format", quoted(string(semantic.JSONPointer)))
	case errors.As(err, &semantic) && semantic.JSONPointer == "":
		return fmt.Errorf("it is not in the form %s", form)
	case errors.As(err, &semantic):
		return fmt.Errorf("it is not in the form %s: the value at %s is of another type", form, quoted(string(semantic.JSONPointer)))
	case errors.As(err, &syntactic) && errors.Is(err, jsontext.ErrDuplicateName):
		return fmt.Errorf("member %s is in one object two times", quoted(string(syntactic.JSONPointer)))
	case err != nil:
		return err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("text after the JSON value")
	}
	switch place, null, err := nullPlace(data); {
	case err != nil:
		return err
	case null && place == "":
		return fmt.Errorf("it is not in the form %s: it is null", form)
	case null:
		return fmt.Errorf("it is not in the form %s: the value at %s is null", form, quoted(place))
	}
	return nil
}

// nullPlace is the JSON pointer of the first null of data, a JSON text that decodeJSON read whole,
// and whether data holds one; err is a fault of the reader other than the end of data, since a text
// that it could not read is no text with no null.
func nullPlace(data []byte) (place string, null bool, err error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		token, err := dec.ReadToken()
		switch {
		case errors.Is(err, io.EOF):
			return "", false, nil
		case err != nil:
			return "", false, err
		case token.Kind() == 'n':
			return string(dec.StackPointer()), true, nil
		}
	}
}

// decodeJSONFrom reads r, at most fileBound bytes of it (readBounded), and decodes it with
// decodeJSON.
func decodeJSONFrom(r io.Reader, v any, form string) error {
	data, err := readBounded(r, fileBound)
	if err != nil {
		return err
	}
	return decodeJSON(data, v, form)
}

// fileBound is the most bytes of one JSON file of a format that the package reads (decodeJSONFrom,
// so approved.json, history.json and ties.json, and each other JSON file of a scenario). 16 MiB is
// far above any such file, and a file over it is refused, before any of it is decoded, so that a
// file which grew past any use cannot take the memory of the process. A spec file of a scenario has
// the bound of a spec file instead, notes.MaxFileSize (readScenarioFiles).
const fileBound = 16 << 20

// readBounded is all of r, or an error that names bound where r holds more than bound bytes, read
// up to one byte past the bound.
func readBounded(r io.Reader, bound int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(bound)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > bound {
		return nil, fmt.Errorf("it holds more than %d bytes, the most that the reader reads of such a file", bound)
	}
	return data, nil
}

// quotedBound is the most bytes of a value of the input that quoted prints whole. encoding/json/v2
// and encoding/json/jsontext cut a JSON pointer to about 100 bytes in their own errors
// (jsonwire.TruncatePointer(pointer, 100) in Go 1.27's src/encoding/json), so a refusal in the
// package's words is no longer than the library's was, and still names the place.
const quotedBound = 100

// Cut is s, a value of the input that a line prints, such as a JSON pointer, a node id or a line
// of a file: whole when it holds at most quotedBound bytes, else its first and its last
// quotedBound/2 bytes, each cut at a whole UTF-8 character, with … in place of the rest. So the
// start of the value and its last part stay, and a line grows with no value of the input. Each
// line of the package and of spec-check that names such a value prints it through Cut, bare or
// quoted; a key, such as Problem.Node, stays whole where it is kept.
func Cut(s string) string {
	if len(s) <= quotedBound {
		return s
	}
	start, end := quotedBound/2, len(s)-quotedBound/2
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	return s[:start] + "…" + s[end:]
}

// quoted is Cut(s) as a Go string literal, so each control character in it is an escape, as %q
// writes it.
func quoted(s string) string {
	return strconv.Quote(Cut(s))
}
