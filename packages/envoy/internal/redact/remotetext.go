package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// RemoteText is one field another party wrote - a refusal's words, a code, a body - held so that
// it redacts itself: every way Go prints it prints Field of it, never the field. Its text sits
// behind an unexported *string, so fmt prints that pointer where it prints a value holding a
// RemoteText in an unexported field, and its own methods where the field is exported or the value
// is printed directly: String, GoString and Format (every verb), MarshalJSON and MarshalText, and
// LogValue. The pointer is to a string and not to a struct: under a verb that is wrong for a
// pointer (%s, %q), fmt prints the pointer's target at the top level, and it spells out the
// fields of a struct there, while it prints a *string as an address whatever the verb.
//
// Only this type's methods read the text, and each reads it in one of three ways alone: as the
// argument of the rule (Field), as an operand of a comparison or a parse whose answer is a bool,
// a closed enum or a parsed value, or in the one assignment construction and decoding make. No
// method answers the text: code that tells fields apart compares through IsEmpty, Equal,
// EqualFold, HasPrefix and Contains, reads a value through Time, Int64 and OAuthCode, and sends a
// field back to whoever wrote it through SendBack, which answers String's text. A field that has
// to be split is split before it is held (NewOAuthRefusal). The zero RemoteText is the empty
// field.
type RemoteText struct {
	text *string
	// carried is the credentials the request this field answers carried, which every printer
	// redacts from the text before the rule reads it (WithoutTokens); nil for none.
	carried *[]string
}

// NewRemoteText is text, a field another party wrote, as a RemoteText.
func NewRemoteText(text string) RemoteText { return RemoteText{text: &text} }

// String is the field as a message may print it (Field).
func (t RemoteText) String() string {
	if t.text == nil {
		return ""
	}
	return fieldWithout(*t.text, t.carried)
}

// fieldWithout is Field of text with each of carried redacted from it first (Tokens): the rule's
// reading of a field whose own request's credentials an answer can echo.
func fieldWithout(text string, carried *[]string) string {
	if carried != nil {
		text = Tokens(text, -1, *carried...)
	}
	return Field(text)
}

// ruledWithout is ruled of text with each of carried redacted from it first (Tokens).
func ruledWithout(text string, carried *[]string) string {
	if carried != nil {
		text = Tokens(text, -1, *carried...)
	}
	return ruled(text)
}

// GoString is String, Go-quoted, for %#v and a caller that asks for Go syntax.
func (t RemoteText) GoString() string { return strconv.Quote(t.String()) }

// Format prints String under the verb, flags, width and precision it is given, as fmt prints that
// string, so no verb prints the field.
func (t RemoteText) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), t.String())
}

// LogValue is String, so a structured log field prints the field as a message may.
func (t RemoteText) LogValue() slog.Value { return slog.StringValue(t.String()) }

// MarshalJSON is String as a JSON string.
func (t RemoteText) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// MarshalText is String.
func (t RemoteText) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalJSON decodes a JSON string into the field, as encoding/json decodes one into a string.
// A JSON null leaves the field as it is. Any other JSON value is the type error encoding/json
// makes of it for a string field, and encoding/json goes on to the fields after it as it does for
// a string field; the error it answers names no struct field, since an Unmarshaler's error
// carries none.
func (t *RemoteText) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] != '"' {
		return &json.UnmarshalTypeError{Value: jsonKind(data[0]), Type: reflect.TypeFor[string]()}
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*t = NewRemoteText(text)
	return nil
}

// UnmarshalText takes data as the field.
func (t *RemoteText) UnmarshalText(data []byte) error {
	*t = NewRemoteText(string(data))
	return nil
}

// jsonKind names the JSON value that starts with c, as encoding/json names it in a type error.
func jsonKind(c byte) string {
	switch c {
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "bool"
	}
	return "number"
}

// WithoutTokens is the field, which every printer prints with each of tokens redacted first
// (Tokens) and which compares as it came: for a client of this repository that reads a refused
// answer's words itself, with the credentials its own request carried (Carried), which whatever
// answered in its server's place can echo back.
func (t RemoteText) WithoutTokens(tokens ...string) RemoteText {
	if t.text == nil || len(tokens) == 0 {
		return t
	}
	carried := tokens
	if t.carried != nil {
		carried = append(append([]string(nil), *t.carried...), tokens...)
	}
	t.carried = &carried
	return t
}

// IsEmpty reports whether the field is empty. It answers no text.
func (t RemoteText) IsEmpty() bool { return t.text == nil || *t.text == "" }

// Equal reports whether the field is s, compared as it came. It answers no text.
func (t RemoteText) Equal(s string) bool {
	if t.text == nil {
		return s == ""
	}
	return *t.text == s
}

// EqualFold reports whether the field is s under Unicode case folding (strings.EqualFold),
// compared as it came. It answers no text.
func (t RemoteText) EqualFold(s string) bool {
	if t.text == nil {
		return s == ""
	}
	return strings.EqualFold(*t.text, s)
}

// HasPrefix reports whether the field begins with prefix, compared as it came.
func (t RemoteText) HasPrefix(prefix string) bool {
	if t.text == nil {
		return prefix == ""
	}
	return strings.HasPrefix(*t.text, prefix)
}

// Contains reports whether the field holds sub, compared as it came.
func (t RemoteText) Contains(sub string) bool {
	if t.text == nil {
		return sub == ""
	}
	return strings.Contains(*t.text, sub)
}

// Time is the field parsed as a time in layout (time.Parse), and whether it parsed. A field that
// does not parse answers the zero time and false, and no error, since time.Parse's error quotes the
// field.
func (t RemoteText) Time(layout string) (time.Time, bool) {
	if t.text == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(layout, *t.text)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// Int64 is the field parsed as a base-10 integer, as json.Number's Int64 reads a number
// (strconv.ParseInt), and whether it parsed. A field that does not parse answers 0 and false, and
// no error, since strconv's error quotes the field.
func (t RemoteText) Int64() (int64, bool) {
	if t.text == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(*t.text, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// OAuthCode is the field read as an OAuth error code, decided from the field as it came: a code
// OAuthCode names, OAuthOther for any other text, and OAuthNone for an empty field.
func (t RemoteText) OAuthCode() OAuthCode {
	if t.text == nil || *t.text == "" {
		return OAuthNone
	}
	for code, name := range oauthCodeNames {
		if name != "" && *t.text == name {
			return OAuthCode(code)
		}
	}
	return OAuthOther
}

// SendBack is the field to send back to the party that wrote it - an id it named, which the next
// request to it carries - and true, where the rule prints the field as it came. The text it
// answers is the rule's, String's, so nothing leaves the field that a print of it does not show.
// An empty field, one the rule changes and one it cannot read answer "" and false, and are sent
// nowhere.
func (t RemoteText) SendBack() (string, bool) {
	if t.text == nil || *t.text == "" {
		return "", false
	}
	shown := Field(*t.text)
	if shown != *t.text {
		return "", false
	}
	return shown, true
}

// RemoteNumber is a number another party wrote - a JSON number, or a JSON string holding one, as
// encoding/json reads either into a json.Number - held as the RemoteText of its literal, so it
// prints only as the rule reads it and is read through Int64. A JSON string that holds no number
// is an error that quotes nothing of it, where json.Number's error quotes the string; any other
// JSON value is the type error encoding/json makes of it for a json.Number.
type RemoteNumber struct{ RemoteText }

// UnmarshalJSON decodes a JSON number, or a JSON string holding one, into the number. A JSON null
// leaves it as it is.
func (n *RemoteNumber) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) == 0 || bytes.Equal(data, []byte("null")):
		return nil
	case data[0] == '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if !numberLiteral(text) {
			return errNotANumber
		}
		n.RemoteText = NewRemoteText(text)
		return nil
	case data[0] == '-' || '0' <= data[0] && data[0] <= '9':
		n.RemoteText = NewRemoteText(string(data))
		return nil
	}
	return &json.UnmarshalTypeError{Value: jsonKind(data[0]), Type: reflect.TypeFor[json.Number]()}
}

// errNotANumber is a JSON string, decoded into a RemoteNumber, that holds no number. It names no
// part of the string.
var errNotANumber = errors.New("json: a string that holds no number, for a number")

// numberLiteral reports whether s is one JSON number: it starts with '-' or a digit, which no
// other JSON value does, ends with a digit, and is valid JSON.
func numberLiteral(s string) bool {
	return s != "" && (s[0] == '-' || '0' <= s[0] && s[0] <= '9') &&
		'0' <= s[len(s)-1] && s[len(s)-1] <= '9' && json.Valid([]byte(s))
}

// NewOAuthRefusal is an OAuth error answer's code and description, each held as a RemoteText. The
// broker's device authorization endpoint answers a refusal of its own as invalid_request described
// "<CODE>: <message>", which is CODE and message. The split is made on the two fields as they
// came, before either is held, so no RemoteText is ever cut, and only where the rule reads the
// whole description as clean (Remote): a ": " can fall inside a URL the description quotes, and
// each half read on its own could then read as clean where the whole does not. Each part is held
// with every one of carried, the credentials the refused request carried, redacted (Tokens).
func NewOAuthRefusal(code, description string, carried ...string) (RemoteText, RemoteText) {
	held := func(field string) RemoteText { return NewRemoteText(Tokens(field, -1, carried...)) }
	if _, readable := Remote(description); readable && code == "invalid_request" {
		if c, m, ok := strings.Cut(description, ": "); ok {
			return held(c), held(m)
		}
	}
	return held(code), held(description)
}

// NewRetrieveError is x/oauth2's token-endpoint error for an answer of status with code and
// description: what an error type over the broker's OAuth refusal unwraps to, so errors.As and
// this package's printable read it as the answer it is. x/oauth2's type holds strings, so it holds
// the rule's reading of each field and never the field: its ErrorCode and ErrorDescription are
// what Remote answers of code and description, WordsNotReadable where it cannot read one, before
// OneLine quotes it, as OAuth2's answer holds them, and its ErrorURI and Body are empty. The value
// printed whole - by its own Error, which quotes each field, under any fmt verb, or by
// encoding/json - prints nothing the rule does not let print, and sanitized, which reads each
// field by the rule again where the refusal's sentence holds it, finds the sentence's quoted
// reading unchanged. A caller that tells refusals apart reads the code through RefusalCode, which
// the error over this answer decides from the code as it came.
func NewRetrieveError(status int, code, description RemoteText) *oauth2.RetrieveError {
	return &oauth2.RetrieveError{
		Response:         &http.Response{StatusCode: status, Status: statusLine(status)},
		ErrorCode:        code.reading(),
		ErrorDescription: description.reading(),
	}
}

// reading is what Remote answers of the field, or WordsNotReadable where it cannot read it, not
// yet on one line (ruled): the form an *oauth2.RetrieveError holds, whose Error quotes it.
func (t RemoteText) reading() string {
	if t.text == nil {
		return ""
	}
	return ruledWithout(*t.text, t.carried)
}

// Logged is control (3b)'s planted comparison.
func (t RemoteText) Logged(s string) bool {
	if t.text == nil {
		return s == ""
	}
	println(*t.text)
	return *t.text == s
}
