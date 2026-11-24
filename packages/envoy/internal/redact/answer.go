package redact

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
)

// Answer is the body of an HTTP answer as every client of this repository reads it (ReadAnswer):
// another party's bytes, which whatever answered in the server's place can have written to echo
// the request back, credentials and all. It is opaque: its one field is unexported and points at
// the bytes through pointers of their own (answer), so fmt, encoding/json and slog print the
// status, addresses or nothing, and its four methods and Holds are the only ways out of it.
//
//   - Decode fills a typed value from the body, holding every RemoteText it decodes with the
//     request's record (Carried) so the value's words print with the request's credentials
//     redacted; its error names the member that did not decode and quotes no value.
//   - Words is the body as another party's words, a RemoteText that already holds that record.
//   - Status is the answer's HTTP status code.
//   - StreamTo hands the bytes to a writer, for a body that is data and never prints: a file the
//     answer downloads, an answer a proxy forwards.
//   - Holds answers a test of the bytes as they came, for a caller that decides on what the
//     answer says - a phrase of its wording (ContainsFold), a type it names - and keeps nothing.
//
// The answer census (cmd/redact-gate) fails on any read of a response's Body outside this
// package but ReadAnswer's, so no client reads an answer's bytes any other way.
type Answer struct {
	a *answer
}

// answer is what an Answer holds: the status, the bytes read, and the record of the requests
// that led to the answer, whose credentials are read once, when Decode or Words needs them. The
// bytes and the credentials sit behind pointers of their own: a verb fmt cannot apply to an
// Answer's pointer (%s, %q) prints the answer it points at, and a pointer inside it as an address.
type answer struct {
	status int
	data   *[]byte
	rec    *record
	once   sync.Once
	tokens *[]string
}

// ErrAnswerTooLarge is the error ReadAnswer wraps when a body holds more than its limit. The
// Answer it comes with holds the first limit bytes, so a caller that cuts a long answer short
// reads them on, and one that refuses it refuses it.
var ErrAnswerTooLarge = errors.New("redact: the answer's body is longer than its reader's limit")

// ReadAnswer reads resp's body, at most limit bytes, as an Answer, with the record of the
// requests that led to resp (Carried). It does not close the body; its caller does. A body that
// holds more than limit bytes is the Answer of its first limit bytes and an error that wraps
// ErrAnswerTooLarge; a read that fails is an Answer with the status and no body, and the read's
// error as a message may print it (unreadable): a transport failure's own error as it came, and
// any other by its class, unwrapping to it, since net/http's text of a body it could not read
// can quote a line the answer wrote.
func ReadAnswer(resp *http.Response, limit int64) (Answer, error) {
	a := &answer{status: resp.StatusCode, rec: answerRecord(resp)}
	if resp.Body == nil || resp.Body == http.NoBody {
		return Answer{a}, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Answer{a}, unreadable(err)
	}
	if int64(len(data)) > limit {
		data = data[:limit]
		a.data = &data
		return Answer{a}, fmt.Errorf("%w (%d bytes)", ErrAnswerTooLarge, limit)
	}
	a.data = &data
	return Answer{a}, nil
}

// StreamAnswer is resp's body as a stream of at most limit bytes, for a body that is data and is
// read as it arrives - a tar unpacked, an event stream followed, a stored object served on - and
// never prints: the streaming form of Answer.StreamTo, for a body too large or too long-lived to
// hold. A read past limit fails with ErrAnswerTooLarge, and a read that fails otherwise with its
// error as ReadAnswer's prints it (unreadable). Closing the stream closes resp's body.
func StreamAnswer(resp *http.Response, limit int64) io.ReadCloser {
	return &answerStream{body: resp.Body, left: limit}
}

// answerStream is StreamAnswer's stream: the body, and how many bytes it may still give.
type answerStream struct {
	body io.ReadCloser
	left int64
}

func (s *answerStream) Read(p []byte) (int, error) {
	if s.body == nil || s.body == http.NoBody {
		return 0, io.EOF
	}
	if s.left <= 0 {
		// One byte more tells a body that ends at the limit from one that runs past it.
		var one [1]byte
		if n, _ := s.body.Read(one[:]); n > 0 {
			return 0, ErrAnswerTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	n, err := s.body.Read(p)
	s.left -= int64(n)
	return n, unreadable(err)
}

func (s *answerStream) Close() error {
	if s.body == nil {
		return nil
	}
	return s.body.Close()
}

// carried is the credentials the answer's requests carried, read once.
func (a *answer) carried() []string {
	a.once.Do(func() {
		var tokens []string
		if a.rec != nil {
			tokens = a.rec.tokens()
		}
		a.tokens = &tokens
	})
	return *a.tokens
}

// bytes is the answer's body, empty for the zero Answer.
func (a Answer) bytes() []byte {
	if a.a == nil || a.a.data == nil {
		return nil
	}
	return *a.a.data
}

// Status is the answer's HTTP status code, and 0 for the zero Answer.
func (a Answer) Status() int {
	if a.a == nil {
		return 0
	}
	return a.a.status
}

// Words is the body, its surrounding whitespace dropped, as another party's words: a RemoteText
// that prints only as the rule reads it, with every credential the answer's requests carried
// redacted first, and compares as it came.
func (a Answer) Words() RemoteText {
	if a.a == nil {
		return NewRemoteText("")
	}
	return NewRemoteText(strings.TrimSpace(string(a.bytes()))).WithoutTokens(a.a.carried()...)
}

// Said is text a caller read from this answer's bytes (StreamTo) - a key or a value of a body it
// reads by its own grammar - as another party's words with the record of the answer's requests: a
// RemoteText that prints only as the rule reads it, every credential those requests carried
// redacted first, and compares as it came. Text the answer does not hold is no answer's words, and
// a caller hands Said none.
func (a Answer) Said(text string) RemoteText {
	if a.a == nil {
		return NewRemoteText(text)
	}
	return NewRemoteText(text).WithoutTokens(a.a.carried()...)
}

// StreamTo writes the body to w, for a body that is data and never prints: a file the answer
// downloads, an answer a proxy forwards to the party that asked. It answers what w.Write answers.
func (a Answer) StreamTo(w io.Writer) (int64, error) {
	n, err := w.Write(a.bytes())
	return int64(n), err
}

// Holds answers test of a's body as it came, read in place and kept nowhere: a decision a client
// makes on what the answer says, before anything of it prints, such as whether GitHub's wording
// names its rate limit (ContainsFold). A decision is made on the words as they came and never on
// the rule's reading of them (Words().String()), which can be WordsNotReadable for a body whose one
// URL sits beside a JSON quote, or can have a credential the request carried cut out of a word the
// decision looks for; the rule is for what leaves the process. test is given the body once, whole,
// and must keep nothing of it.
func Holds(a Answer, test func(body []byte) bool) bool { return test(a.bytes()) }

// ContainsFold reports whether body holds phrase, which must be lower-case ASCII, in whatever
// case it is written, and copies nothing: Holds' test for a phrase of another party's wording.
// Every answer a decision reads runs through it, most of them good ones of up to a client's
// megabyte, and bytes.ToLower over such a body allocated and copied the whole of it to find a
// phrase that is almost never there. The body is scanned for the phrase's first byte in either
// case instead and only those windows compared, which on a megabyte of answer measured about half
// the time the copy alone took.
func ContainsFold(body []byte, phrase string) bool {
	first, wanted := string([]byte{phrase[0], phrase[0] - 'a' + 'A'}), []byte(phrase)
	for from := 0; from+len(phrase) <= len(body); from++ {
		at := bytes.IndexAny(body[from:len(body)-len(phrase)+1], first)
		if at < 0 {
			return false
		}
		from += at
		if bytes.EqualFold(body[from:from+len(phrase)], wanted) {
			return true
		}
	}
	return false
}

// Decode fills v, which must point at a typed value, from the body as JSON, and holds every
// RemoteText it decodes, at any depth of v, with the record of the answer's requests (Carried):
// a field another party wrote prints with the request's credentials redacted, as Words prints the
// body, and compares as it came. A destination that would take the body whole - a string, a byte
// slice, a json.RawMessage or an interface value, behind however many pointers - is refused, since
// its text would leave the answer as plain text. So is one that holds a member encoding/json would
// fill with the answer's text as it came (plainMember), at any depth it decodes to: a byte slice,
// a json.RawMessage, or an interface value the caller did not set to a non-nil pointer. So is one
// that would fill a box no record can reach (unreached), before anything is decoded into it: a
// box in a map's key, which encoding/json fills through UnmarshalText and which has no address.
// So is one that embeds a RemoteText itself under an unexported name (an alias of it), whose box
// no record can reach either (recordBoxes); that box is left as encoding/json filled it, and the
// error says the destination holds it. Its error is fixed text: it names the destination's type
// and, where it can, the member that did not decode and the JSON kind it held, and never a value
// of the answer, since encoding/json's errors, and the errors of the types it decodes into
// (time.Time's among them), quote the value they could not read; it unwraps to nothing, so no
// caller reaches the decoder's own error.
func (a Answer) Decode(v any) error {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("redact: an answer decodes into a non-nil pointer, not %T", v)
	}
	if whole(rv.Type().Elem()) {
		return fmt.Errorf("redact: an answer does not decode into %T, which would hold the body whole", v)
	}
	if held := unreached(rv, map[followed]bool{}); held != "" {
		return fmt.Errorf("redact: an answer does not decode into %T, which holds %s, where no record of the answer reaches it", v, held)
	}
	if held := plainMember(rv, map[followed]bool{}); held != "" {
		return fmt.Errorf("redact: an answer does not decode into %T, which holds %s, whose text would leave the answer as plain text", v, held)
	}
	data := a.bytes()
	err := json.Unmarshal(data, v)
	if a.a != nil && holdsBox(rv.Type(), map[reflect.Type]bool{}) && !recordBoxes(rv, a.a.carried()) {
		return fmt.Errorf("redact: an answer does not decode into %T, which embeds a RemoteText under an unexported name that no record of the answer reaches", v)
	}
	if err != nil {
		return errors.New(decodeText(err, data, rv.Type().Elem()))
	}
	return nil
}

// whole reports whether a value of t takes a JSON body whole as text: a string, a byte slice
// (json.RawMessage among them) or an interface value, or a pointer to one, which encoding/json
// allocates and fills.
func whole(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String, reflect.Interface:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return false
}

// boxInKey is what unreached answers of a map whose key holds a box.
const boxInKey = "a RemoteText in a map's key"

// followed is a pointer unreached has followed: its target's address and type.
type followed struct {
	at uintptr
	t  reflect.Type
}

// unreached is what decoding into v would fill that no record of the answer reaches, as Decode's
// refusal names it, or "": a box in a map's key (holdsBox of the key's type), which encoding/json
// fills through the key's UnmarshalText and which has no address recordBoxes could hand the record
// through. It reads v as encoding/json decodes into it: the value a non-nil pointer points at, an
// interface value the caller set to a non-nil pointer (pointsAt), each struct field encoding/json
// decodes (decoded) and each element a slice or an array holds, since encoding/json decodes into
// each of those in place; and the type (unreachedType) of what it makes - the value of a nil
// pointer, a list's further elements, a map. seen holds the pointers it has followed, so a value
// that points back at itself ends the walk.
func unreached(v reflect.Value, seen map[followed]bool) string {
	switch v.Kind() {
	case reflect.Pointer:
		at := followed{v.Pointer(), v.Type()}
		switch {
		case v.IsNil():
			return unreachedType(v.Type().Elem(), map[reflect.Type]bool{})
		case seen[at]:
			return ""
		}
		seen[at] = true
		return unreached(v.Elem(), seen)
	case reflect.Interface:
		if pointsAt(v) {
			return unreached(v.Elem(), seen)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if decoded(v.Type().Field(i)) {
				if held := unreached(v.Field(i), seen); held != "" {
					return held
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if held := unreached(v.Index(i), seen); held != "" {
				return held
			}
		}
		return unreachedType(v.Type().Elem(), map[reflect.Type]bool{})
	case reflect.Map:
		return unreachedType(v.Type(), map[reflect.Type]bool{})
	}
	return ""
}

// unreachedType is unreached of a value of t that encoding/json makes: through a pointer, a
// slice's or an array's element, a map's value or a struct's field it decodes. An interface value
// it makes holds a JSON value of its own, never a box. A map whose key is an interface value is
// one encoding/json refuses to decode into.
func unreachedType(t reflect.Type, seen map[reflect.Type]bool) string {
	if seen[t] {
		return ""
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Map:
		if t.Key().Kind() != reflect.Interface && holdsBox(t.Key(), map[reflect.Type]bool{}) {
			return boxInKey
		}
		return unreachedType(t.Elem(), seen)
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return unreachedType(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); decoded(f) {
				if held := unreachedType(f.Type, seen); held != "" {
					return held
				}
			}
		}
	}
	return ""
}

// plainMember is what of v encoding/json would fill with the answer's text as it came, where no
// record of the answer reaches it, as Decode's refusal names it (plainText), or "": a byte slice,
// json.RawMessage among them, which it fills with a member's bytes, and an interface value, which
// it fills with a JSON value of its own, text and all. It reads v as unreached does, as
// encoding/json decodes into it - the value a non-nil pointer points at, an interface value the
// caller set to a non-nil pointer, each struct field it fills (filled) and each element a list
// holds, and the type (plainMemberType) of what it makes - and goes into no value whose type
// decodes its JSON itself (decodesItself), none of whose members encoding/json fills: what such a
// type keeps is its own code's, as what a RemoteText keeps is.
func plainMember(v reflect.Value, seen map[followed]bool) string {
	if held := plainText(v.Type()); held != "" && (v.Kind() != reflect.Interface || !pointsAt(v)) {
		return held
	}
	if decodesItself(v.Type()) {
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer:
		at := followed{v.Pointer(), v.Type()}
		switch {
		case v.IsNil():
			return plainMemberType(v.Type().Elem(), map[reflect.Type]bool{})
		case seen[at]:
			return ""
		}
		seen[at] = true
		return plainMember(v.Elem(), seen)
	case reflect.Interface:
		return plainMember(v.Elem(), seen)
	case reflect.Struct:
		for i := range v.NumField() {
			if filled(v.Type().Field(i)) {
				if held := plainMember(v.Field(i), seen); held != "" {
					return held
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if held := plainMember(v.Index(i), seen); held != "" {
				return held
			}
		}
		return plainMemberType(v.Type().Elem(), map[reflect.Type]bool{})
	case reflect.Map:
		return plainMemberType(v.Type().Elem(), map[reflect.Type]bool{})
	}
	return ""
}

// plainMemberType is plainMember of a value of t that encoding/json makes: what it holds through
// a pointer, a list's item, a map's value or a struct field it fills. An interface value it makes
// holds a JSON value of its own.
func plainMemberType(t reflect.Type, seen map[reflect.Type]bool) string {
	if held := plainText(t); held != "" {
		return held
	}
	if seen[t] || decodesItself(t) {
		return ""
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return plainMemberType(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); filled(f) {
				if held := plainMemberType(f.Type, seen); held != "" {
					return held
				}
			}
		}
	}
	return ""
}

// plainText is how Decode's refusal names a value of t that encoding/json fills with the answer's
// text as it came, or "": a byte slice, json.RawMessage among them, or an interface value.
func plainText(t reflect.Type) string {
	switch {
	case t.Kind() == reflect.Interface:
		return "an interface value"
	case t.Kind() != reflect.Slice || t.Elem().Kind() != reflect.Uint8:
		return ""
	case t.Name() != "":
		return "a " + t.String()
	}
	return "a byte slice"
}

// filled reports whether encoding/json fills field f of a struct: one it decodes (decoded) whose
// tag does not skip it.
func filled(f reflect.StructField) bool { return decoded(f) && f.Tag.Get("json") != "-" }

// decodesItself reports whether encoding/json hands a value of t its JSON whole, through the
// type's own UnmarshalJSON or UnmarshalText, and fills none of its members itself.
func decodesItself(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	return p.Implements(jsonUnmarshalerType) || p.Implements(textUnmarshalerType)
}

// decodeText is the fixed text of err, encoding/json's error decoding data into a value of type
// t: the destination's type, the member that did not decode as t names it (member) and, where
// encoding/json names it, the JSON kind it held; never a value.
func decodeText(err error, data []byte, t reflect.Type) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("the answer is not JSON that %s decodes from (at byte %d)", typeName(t), syntax.Offset)
	}
	name, field := failingMember(data, t)
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		kind, _, _ := strings.Cut(typed.Value, " ")
		// A type error that names no field, from a member's own UnmarshalJSON - a RemoteText's, for
		// a member that holds no JSON string - to which encoding/json adds none, names the member
		// failingMember finds, and that member's type.
		if typed.Field == "" && name != "" {
			return fmt.Sprintf("the answer's %q is a JSON %s where %s holds %s", name, kind, typeName(t), typeName(field))
		}
		return fmt.Sprintf("the answer's %s is a JSON %s where %s holds %s", member(typed.Field, t), kind, typeName(t), typeName(typed.Type))
	}
	if name != "" {
		return fmt.Sprintf("the answer's %q does not decode where %s holds %s", name, typeName(t), typeName(field))
	}
	return fmt.Sprintf("the answer does not decode into %s", typeName(t))
}

// typeName is how a message names a type: by its name, "a time.Time" or "a string", and an
// anonymous struct as "the value", whose definition would be the whole of the message.
func typeName(t reflect.Type) string {
	switch {
	case t == nil:
		return "the value"
	case t.Kind() == reflect.Pointer:
		return typeName(t.Elem())
	case t.Name() != "":
		return "a " + t.String()
	case t.Kind() == reflect.Slice:
		return "a list"
	case t.Kind() == reflect.Map:
		return "an object"
	}
	return "the value"
}

// member is the member that did not decode as a message names it: "value" for the top level,
// and otherwise field, encoding/json's path to it (UnmarshalTypeError.Field), as t, the
// destination's type, names it: each struct field by its own JSON name (memberField), each map
// key as "(key)" and each list item as "(item)". No segment of field is printed: since Go 1.27
// runs json v1 on v2, that path holds the answer's own map keys, and its own spelling of a
// member's name, which encoding/json matches in any case. The path ends at the first segment t
// does not name, and one whose first segment t does not name is "a member".
func member(field string, t reflect.Type) string {
	if field == "" {
		return "value"
	}
	var names []string
walk:
	for _, segment := range strings.Split(field, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Map:
			names, t = append(names, "(key)"), t.Elem()
		case reflect.Slice, reflect.Array:
			names, t = append(names, "(item)"), t.Elem()
		case reflect.Struct:
			f, ok := memberField(t, segment)
			if !ok {
				break walk
			}
			names, t = append(names, jsonName(f)), f.Type
		default:
			break walk
		}
	}
	if len(names) == 0 {
		return "a member"
	}
	return fmt.Sprintf("%q", strings.Join(names, "."))
}

// memberField is the field of the struct type t that encoding/json decodes the member name into,
// as it matches a name: the field of that JSON name (jsonName), else one whose JSON name is name
// in another case (strings.EqualFold), at t's own level or promoted from a struct embedded with no
// JSON name of its own; and whether there is one.
func memberField(t reflect.Type, name string) (reflect.StructField, bool) {
	folded, found := reflect.StructField{}, false
	for i := range t.NumField() {
		f := t.Field(i)
		if !decoded(f) {
			continue
		}
		inner := f.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); f.Anonymous && tag == "" && inner.Kind() == reflect.Struct {
			if promoted, ok := memberField(inner, name); ok {
				return promoted, true
			}
			continue
		}
		switch own := jsonName(f); {
		case own == "":
		case own == name:
			return f, true
		case !found && strings.EqualFold(own, name):
			folded, found = f, true
		}
	}
	return folded, found
}

// failingMember is the path to the first member of data, decoded into a value of type t, whose
// value does not decode, and that member's type, or "": what encoding/json does not name for an
// error of a type's own UnmarshalJSON or UnmarshalText (time.Time's, a RemoteText's, a
// RemoteNumber's). The path is t's own, as member names it: each struct field by its own JSON
// name, matched as encoding/json matches the answer's (memberField: in any case, or promoted from
// an embedded struct), each list item as "(item)" and each map value as "(key)"; never the
// answer's own spelling or keys. It reads data once, in the answer's order (failingValue).
func failingMember(data []byte, t reflect.Type) (string, reflect.Type) {
	path, held, err := failingValue(json.NewDecoder(bytes.NewReader(data)), t)
	if err != nil || held == nil {
		return "", nil
	}
	return strings.Join(path, "."), held
}

// failingValue reads the next JSON value of dec as encoding/json decodes it into a value of type
// t, and answers the type of the member that does not decode in it, or nil where it decodes, and
// the path to that member inside it, empty for the value itself; err is dec's own, for JSON it
// cannot read. A struct, a map or a list whose JSON is the object or the array it takes is read
// member by member (opens), so a member that decodes is read once and one that does not is gone
// into; a member a struct has no field for, and an array's item past its length, are passed over,
// as encoding/json passes them; every other value is decoded whole. JSON null decodes into
// anything, as encoding/json leaves the value as it is.
func failingValue(dec *json.Decoder, t reflect.Type) ([]string, reflect.Type, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	open := opens(t)
	if open == 0 {
		if dec.Decode(reflect.New(t).Interface()) != nil {
			return nil, t, nil
		}
		return nil, nil, nil
	}
	switch token, err := dec.Token(); {
	case err != nil:
		return nil, nil, err
	case token == nil:
		return nil, nil, nil
	case token != open:
		return nil, t, nil
	}
	for index := 0; dec.More(); index++ {
		var name string
		var into reflect.Type
		switch t.Kind() {
		case reflect.Struct:
			key, err := dec.Token()
			if err != nil {
				return nil, nil, err
			}
			text, _ := key.(string)
			if f, ok := memberField(t, text); ok {
				name, into = jsonName(f), f.Type
			}
		case reflect.Map:
			if _, err := dec.Token(); err != nil {
				return nil, nil, err
			}
			name, into = "(key)", t.Elem()
		default:
			if t.Kind() == reflect.Slice || index < t.Len() {
				name, into = "(item)", t.Elem()
			}
		}
		if into == nil {
			var passed json.RawMessage
			if err := dec.Decode(&passed); err != nil {
				return nil, nil, err
			}
			continue
		}
		if path, held, err := failingValue(dec, into); err != nil || held != nil {
			return append([]string{name}, path...), held, err
		}
	}
	_, err := dec.Token()
	return nil, nil, err
}

// opens is the delimiter of the JSON value encoding/json decodes a value of t from member by
// member: '{' for a struct or a map and '[' for a list, and 0 for every other value, which it
// decodes whole: one whose type decodes its JSON itself (decodesItself), a byte slice (from base64
// text), a scalar or an interface value.
func opens(t reflect.Type) json.Delim {
	if decodesItself(t) {
		return 0
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return '{'
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return 0
		}
		return '['
	case reflect.Array:
		return '['
	}
	return 0
}

// jsonUnmarshalerType and textUnmarshalerType are the interfaces through which encoding/json hands
// a type its JSON whole (decodesItself).
var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// jsonName is the member name encoding/json decodes field f from, "" for a field it skips.
func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	switch tag {
	case "-":
		return ""
	case "":
		return f.Name
	}
	return tag
}

// remoteTextType is the box's type, which recordBoxes stops at.
var remoteTextType = reflect.TypeFor[RemoteText]()

// decoded reports whether encoding/json decodes into field f of a struct: an exported field, or
// an embedded one whatever its type's export, whose exported fields encoding/json promotes.
func decoded(f reflect.StructField) bool { return f.IsExported() || f.Anonymous }

// recordBoxes holds every RemoteText v reaches - through a pointer, a struct's field encoding/json
// decodes (decoded: an exported one, or an embedded one of any type's export), a slice, an array,
// a map, or an interface value the caller set to a non-nil pointer, which encoding/json decodes
// into in place - with carried redacted from it (RemoteText.WithoutTokens). It reaches each box
// as a *RemoteText and hands it the record through the box's own method: it reads no box's
// storage, and descends into no box. A map's values are copied, recorded and set back, since a
// map value has no address; an interface value that holds no pointer, as every one encoding/json
// fills itself does, holds no box and is passed by. It answers false where it reached a box it
// cannot hand the record to - the box itself embedded under an unexported name (an alias of
// RemoteText), which reflect gives no address that can be used - after recording every other box.
func recordBoxes(v reflect.Value, carried []string) bool {
	if len(carried) == 0 {
		return true
	}
	recorded := true
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			recorded = recordBoxes(v.Elem(), carried)
		}
	case reflect.Interface:
		if pointsAt(v) {
			recorded = recordBoxes(v.Elem(), carried)
		}
	case reflect.Struct:
		if v.Type() == remoteTextType {
			if !v.CanAddr() {
				return true
			}
			if !v.CanInterface() {
				return false
			}
			box := v.Addr().Interface().(*RemoteText)
			*box = box.WithoutTokens(carried...)
			return true
		}
		for i := range v.NumField() {
			if f := v.Type().Field(i); decoded(f) && holdsBox(f.Type, map[reflect.Type]bool{}) {
				recorded = recordBoxes(v.Field(i), carried) && recorded
			}
		}
	case reflect.Slice, reflect.Array:
		if !holdsBox(v.Type().Elem(), map[reflect.Type]bool{}) {
			return true
		}
		for i := range v.Len() {
			recorded = recordBoxes(v.Index(i), carried) && recorded
		}
	case reflect.Map:
		if !holdsBox(v.Type().Elem(), map[reflect.Type]bool{}) {
			return true
		}
		for _, key := range v.MapKeys() {
			item := v.MapIndex(key)
			if item.Kind() == reflect.Interface && !pointsAt(item) {
				continue
			}
			value := reflect.New(v.Type().Elem()).Elem()
			value.Set(item)
			recorded = recordBoxes(value, carried) && recorded
			v.SetMapIndex(key, value)
		}
	}
	return recorded
}

// pointsAt reports whether v, an interface value, holds a non-nil pointer: the one kind of value
// in an interface that encoding/json decodes into rather than replaces.
func pointsAt(v reflect.Value) bool {
	return !v.IsNil() && v.Elem().Kind() == reflect.Pointer && !v.Elem().IsNil()
}

// holdsBox reports whether a value of t can hold a RemoteText encoding/json decoded: directly, or
// through a pointer, a slice, an array, a map, a struct's field encoding/json decodes (decoded),
// or an interface value, which can hold a pointer the caller set to one (recordBoxes).
func holdsBox(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == remoteTextType {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return holdsBox(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if decoded(t.Field(i)) && holdsBox(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}
