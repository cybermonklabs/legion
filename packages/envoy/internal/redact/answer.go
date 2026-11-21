package redact

import (
	"bytes"
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
// ErrAnswerTooLarge; a read that fails is an Answer with the status and no body, and the read's own
// error, a transport failure, which quotes nothing the answer wrote.
func ReadAnswer(resp *http.Response, limit int64) (Answer, error) {
	a := &answer{status: resp.StatusCode, rec: answerRecord(resp)}
	if resp.Body == nil || resp.Body == http.NoBody {
		return Answer{a}, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Answer{a}, err
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
// hold. A read past limit fails with ErrAnswerTooLarge. Closing the stream closes resp's body.
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
	return n, err
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
// slice, a json.RawMessage or an interface value - is refused, since its text would leave the
// answer as plain text. Its error is fixed text: it names the destination's type and, where it
// can, the member that did not decode and the JSON kind it held, and never a value of the answer,
// since encoding/json's errors, and the errors of the types it decodes into (time.Time's among
// them), quote the value they could not read.
func (a Answer) Decode(v any) error {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return &decodeError{fmt.Sprintf("redact: an answer decodes into a non-nil pointer, not %T", v)}
	}
	if whole(rv.Type().Elem()) {
		return &decodeError{fmt.Sprintf("redact: an answer does not decode into %T, which would hold the body whole", v)}
	}
	data := a.bytes()
	err := json.Unmarshal(data, v)
	if a.a != nil && holdsBox(rv.Type(), map[reflect.Type]bool{}) {
		recordBoxes(rv, a.a.carried())
	}
	if err != nil {
		return &decodeError{decodeText(err, data, rv.Type().Elem())}
	}
	return nil
}

// whole reports whether a value of t takes a JSON body whole as text: a string, a byte slice
// (json.RawMessage among them) or an interface value.
func whole(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Interface:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return false
}

// decodeError is Decode's error: fixed text that quotes no value of the answer. It unwraps to
// nothing, so no caller reaches the decoder's own error, which quotes one.
type decodeError struct{ text string }

func (e *decodeError) Error() string { return e.text }

// decodeText is the fixed text of err, encoding/json's error decoding data into a value of type
// t: the destination's type, the member that did not decode and, where encoding/json names it,
// the JSON kind it held; never a value.
func decodeText(err error, data []byte, t reflect.Type) string {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("the answer is not JSON that %s decodes from (at byte %d)", typeName(t), syntax.Offset)
	case errors.As(err, &typed) && (typed.Field != "" || failingMember(data, t) == ""):
		kind, _, _ := strings.Cut(typed.Value, " ")
		return fmt.Sprintf("the answer's %s is a JSON %s where %s holds %s", member(typed.Field), kind, typeName(t), typeName(typed.Type))
	}
	// A type error that names no field, from a member's own UnmarshalJSON - a RemoteText's, for a
	// member that holds no JSON string - to which encoding/json adds none, names its member here.
	if name := failingMember(data, t); name != "" {
		return fmt.Sprintf("the answer's %q does not decode where %s holds %s", name, typeName(t), typeName(fieldType(t, name)))
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

// member is a decoded field's path as a message names it, "value" for the top level.
func member(field string) string {
	if field == "" {
		return "value"
	}
	return fmt.Sprintf("%q", field)
}

// failingMember is the JSON name of the first member of data, an object decoded into the struct
// type t, whose value does not decode into its field's type, or "": what encoding/json does not
// name for an error of a type's own UnmarshalJSON or UnmarshalText (time.Time's, a RemoteNumber's).
// The name is t's own, from its field's json tag, never the answer's.
func failingMember(data []byte, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) != nil {
		return ""
	}
	for i := range t.NumField() {
		f := t.Field(i)
		name := jsonName(f)
		raw, ok := members[name]
		if !ok || name == "" {
			continue
		}
		if json.Unmarshal(raw, reflect.New(f.Type).Interface()) != nil {
			return name
		}
	}
	return ""
}

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

// fieldType is the type of t's field whose JSON name is name.
func fieldType(t reflect.Type, name string) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := range t.NumField() {
		if jsonName(t.Field(i)) == name {
			return t.Field(i).Type
		}
	}
	return nil
}

// remoteTextType is the box's type, which recordBoxes stops at.
var remoteTextType = reflect.TypeFor[RemoteText]()

// recordBoxes holds every RemoteText v reaches - through a pointer, a struct's exported field (an
// embedded one too), a slice, an array, a map or an interface value - with carried redacted from
// it (RemoteText.WithoutTokens). It reaches each box as a *RemoteText and hands it the record
// through the box's own method: it reads no box's storage, and descends into no box. A map's
// values are copied, recorded and set back, since a map value has no address.
func recordBoxes(v reflect.Value, carried []string) {
	if len(carried) == 0 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			recordBoxes(v.Elem(), carried)
		}
	case reflect.Interface:
		// encoding/json fills an interface value with a map, a slice or a scalar, never a box; it
		// decodes into a box behind one only where the caller set a pointer there first.
		if !v.IsNil() && v.Elem().Kind() == reflect.Pointer {
			recordBoxes(v.Elem(), carried)
		}
	case reflect.Struct:
		if v.Type() == remoteTextType {
			if v.CanAddr() {
				box := v.Addr().Interface().(*RemoteText)
				*box = box.WithoutTokens(carried...)
			}
			return
		}
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() && holdsBox(f.Type, map[reflect.Type]bool{}) {
				recordBoxes(v.Field(i), carried)
			}
		}
	case reflect.Slice, reflect.Array:
		if !holdsBox(v.Type().Elem(), map[reflect.Type]bool{}) {
			return
		}
		for i := range v.Len() {
			recordBoxes(v.Index(i), carried)
		}
	case reflect.Map:
		if !holdsBox(v.Type().Elem(), map[reflect.Type]bool{}) {
			return
		}
		for _, key := range v.MapKeys() {
			value := reflect.New(v.Type().Elem()).Elem()
			value.Set(v.MapIndex(key))
			recordBoxes(value, carried)
			v.SetMapIndex(key, value)
		}
	}
}

// holdsBox reports whether a value of t can hold a RemoteText encoding/json decoded: directly, or
// through a pointer, a slice, an array, a map or a struct's exported field. An interface value is
// no such holder, since encoding/json fills one with a map, a slice or a scalar (recordBoxes).
func holdsBox(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == remoteTextType {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return holdsBox(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if t.Field(i).IsExported() && holdsBox(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}
