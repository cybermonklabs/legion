package redact_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/answertest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// refusedWith is a refusal whose message echoes the request's bearer.
func refusedWith(auth string) string { return `{"message":"the token ` + auth + ` was refused"}` }

// echoed is an embedded unexported struct whose exported member encoding/json promotes into the
// struct that embeds it.
type echoed struct {
	Message redact.RemoteText `json:"message"`
}

// TestDecodeHoldsABoxPromotedFromAnEmbeddedUnexportedStruct: encoding/json decodes the exported
// members of an embedded struct whatever the embedded type's own export, by value or behind a
// pointer the caller set, so a box promoted that way is another party's words like any other and
// prints with the request's bearer redacted. Captures redact.answer-embedded/<case>.
func TestDecodeHoldsABoxPromotedFromAnEmbeddedUnexportedStruct(t *testing.T) {
	answer := answerOf(t, http.StatusForbidden, refusedWith)
	var byValue struct{ echoed }
	if err := answer.Decode(&byValue); err != nil {
		t.Fatal(err)
	}
	byPointer := struct{ *echoed }{&echoed{}}
	if err := answer.Decode(&byPointer); err != nil {
		t.Fatal(err)
	}
	// Control: encoding/json filled both, so a print of them is a print of the answer's member.
	if !byValue.Message.Equal("the token Bearer "+redacttest.Token+" was refused") || !byPointer.Message.Equal("the token Bearer "+redacttest.Token+" was refused") {
		t.Fatal("control: Decode did not fill the promoted member, so this test reads nothing")
	}
	redacttest.RequireNone(t, "redact.answer-embedded/value", byValue.Message.String())
	redacttest.RequireNone(t, "redact.answer-embedded/pointer", byPointer.Message.String())
}

// said is the box under an unexported name of this package's own. Embedded, it is an embedded
// field of an unexported name, whose address reflect hands out only as a value that cannot be
// used, and the box's UnmarshalJSON is promoted to the struct that embeds it, so encoding/json
// decodes a JSON string body whole into it.
type said = redact.RemoteText

// Said is the same box under an exported name, which reflect addresses.
type Said = redact.RemoteText

// TestDecodeRefusesABoxEmbeddedUnderAnUnexportedName: a destination that embeds the box itself
// under an unexported name, by value or behind a pointer the caller set, holds a box no record can
// reach, since reflect gives no usable address of such a field to call the box's method through.
// Decode refuses it before it decodes anything, with fixed text that names no word of the answer,
// so the box stays empty, and whether or not the answer's request carried a credential: a type
// is refused for what it holds, never for what one answer's request carried. The control embeds
// the same box under an exported name, which Decode fills and records, so it prints with the
// request's bearer redacted.
func TestDecodeRefusesABoxEmbeddedUnderAnUnexportedName(t *testing.T) {
	answer := answerOf(t, http.StatusForbidden, func(auth string) string { return strconv.Quote("the token " + auth + " was refused") })
	uncarried := answertest.Of(http.StatusForbidden, strconv.Quote("the token was refused"))
	var byValue, uncarriedByValue struct{ said }
	byPointer := struct{ *said }{&said{}}
	for _, c := range []struct {
		name   string
		answer redact.Answer
		into   any
		box    *redact.RemoteText
	}{
		{"by-value", answer, &byValue, &byValue.said},
		{"no-credential", uncarried, &uncarriedByValue, &uncarriedByValue.said},
		{"behind-a-pointer", answer, &byPointer, byPointer.said},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.answer.Decode(c.into)
			switch {
			case err == nil:
				t.Fatal("Decode filled a box embedded under an unexported name, which no record of the answer reaches")
			case strings.Contains(err.Error(), redacttest.Token) || strings.Contains(err.Error(), "was refused"):
				t.Errorf("Decode's refusal %q quotes the answer", err)
			case !c.box.IsEmpty():
				t.Error("Decode refused the destination after it filled the box")
			}
		})
	}
	var exported struct{ Said }
	if err := answer.Decode(&exported); err != nil {
		t.Fatalf("control: Decode of the box embedded under an exported name = %v, want it filled", err)
	}
	if !exported.Said.Equal("the token Bearer " + redacttest.Token + " was refused") {
		t.Fatal("control: Decode did not fill the embedded box, so this test reads nothing")
	}
	redacttest.RequireNone(t, "redact.answer-embedded/exported-alias", exported.Said.String())
}

// TestDecodeRefusesAnEmbeddedPointerADecoderIsPromotedThrough: a struct that embeds a pointer to a
// type that decodes its JSON itself has that type's decoder promoted to it, and encoding/json calls
// the decoder through the pointer, which is nil in every such struct encoding/json makes and in
// one the caller left so: encoding/json panicked decoding into it. Decode refuses such a
// destination before it decodes anything, with fixed text that quotes no word of the answer, never
// a panic - the box behind the pointer, a list of such structs, a time.Time behind the pointer,
// and an embedded interface value of the decoder's methods, which holds nothing - and refuses the
// box so embedded where the caller set the pointer too, since encoding/json makes the same type
// with the pointer nil wherever it makes one. The control embeds the box by value, which Decode
// fills and records, so it prints with the request's bearer redacted. Captures
// redact.answer-promoted-pointer/<case>.
func TestDecodeRefusesAnEmbeddedPointerADecoderIsPromotedThrough(t *testing.T) {
	quoted := func(auth string) string { return strconv.Quote("the token " + auth + " was refused") }
	listed := func(auth string) string { return "[" + quoted(auth) + "]" }
	at := func(string) string { return `"2026-11-23T00:00:00Z"` }
	for _, c := range []struct {
		name, names string
		answer      func(auth string) string
		into        any
	}{
		{"box", "embeds a pointer", quoted, new(struct{ *redact.RemoteText })},
		{"box-list-item", "embeds a pointer", listed, new([]struct{ *redact.RemoteText })},
		{"box-the-caller-set", "embeds a pointer", quoted, &struct{ *redact.RemoteText }{&redact.RemoteText{}}},
		{"time", "embeds a pointer", at, new(struct{ *time.Time })},
		{"interface", "an interface value", quoted, new(struct{ json.Unmarshaler })},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := answerOf(t, http.StatusForbidden, c.answer)
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("Decode panicked: %v", p)
					}
				}()
				return answer.Decode(c.into)
			}()
			// fmt, unlike encoding/json, recovers a String method that panics on the nil pointer.
			printed := fmt.Sprintf("%s", c.into)
			redacttest.RequireNone(t, "redact.answer-promoted-pointer/"+c.name, fmt.Sprintf("%s | %v", printed, err))
			switch {
			case err == nil:
				t.Errorf("Decode filled %s, whose decoder encoding/json calls through an embedded pointer: %s", c.name, printed)
			case strings.Contains(err.Error(), "was refused") || !strings.Contains(err.Error(), c.names):
				t.Errorf("Decode's refusal %q quotes the answer or does not name what the destination holds", err)
			case strings.Contains(printed, "was refused"):
				t.Errorf("Decode refused %s after it filled it: %s", c.name, printed)
			}
		})
	}
	var byValue struct{ redact.RemoteText }
	if err := answerOf(t, http.StatusForbidden, quoted).Decode(&byValue); err != nil {
		t.Fatalf("control: Decode of the box embedded by value = %v, want it filled", err)
	}
	if !byValue.Equal("the token Bearer " + redacttest.Token + " was refused") {
		t.Fatal("control: Decode did not fill the embedded box, so this test reads nothing")
	}
	redacttest.RequireNone(t, "redact.answer-promoted-pointer/by-value", byValue.String())
}

// TestDecodeHoldsABoxBehindAnInterfaceTheCallerSet: encoding/json decodes a member into the value
// an interface field points at where the caller set a non-nil pointer there first (its indirect),
// so a box behind it is the answer's words like any other and prints with the request's bearer
// redacted. Captures redact.answer-interface.
func TestDecodeHoldsABoxBehindAnInterfaceTheCallerSet(t *testing.T) {
	answer := answerOf(t, http.StatusForbidden, func(auth string) string { return `{"detail":` + refusedWith(auth) + `}` })
	detail := &echoed{}
	var envelope struct {
		Detail any `json:"detail"`
	}
	envelope.Detail = detail
	if err := answer.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	// Control: encoding/json decoded into the caller's pointer rather than a map of its own.
	if envelope.Detail != any(detail) || !detail.Message.Equal("the token Bearer "+redacttest.Token+" was refused") {
		t.Fatalf("control: Decode filled %#v, not the caller's pointer, so this test reads nothing", envelope.Detail)
	}
	redacttest.RequireNone(t, "redact.answer-interface", detail.Message.String())
}

// TestAnAnswerRefusesAWholeBodyDestinationBehindPointers: a pointer to a pointer to a
// json.RawMessage, an interface value, a string or a byte slice takes the body whole as plain
// text as the pointer to it does, since encoding/json allocates each pointer it meets; Decode
// refuses each. Each is given an answer encoding/json fills it from - the refusal for the
// json.RawMessage and the interface value, the refusal's words as a JSON string for the string,
// and those words in base64 for the byte slice - and the capture holds what each kept, so one
// filled prints the bearer: the string's above all, which whole alone refuses, since a member
// that is a string is no plain-text member. The control is a struct behind the same two
// pointers, which it fills.
func TestAnAnswerRefusesAWholeBodyDestinationBehindPointers(t *testing.T) {
	refusal := answerOf(t, http.StatusOK, refusedWith)
	quoted := answerOf(t, http.StatusOK, func(auth string) string { return strconv.Quote("the token " + auth + " was refused") })
	encoded := answerOf(t, http.StatusOK, func(auth string) string {
		return `"` + base64.StdEncoding.EncodeToString([]byte("the token "+auth+" was refused")) + `"`
	})
	var raw *json.RawMessage
	var a *any
	var s **string
	var b *[]byte
	for name, c := range map[string]struct {
		answer redact.Answer
		into   any
	}{"raw": {refusal, &raw}, "any": {refusal, &a}, "string": {quoted, &s}, "bytes": {encoded, &b}} {
		if err := c.answer.Decode(c.into); err == nil {
			t.Errorf("Decode filled a pointer to a %s with the body whole", name)
		}
	}
	printed := ""
	if raw != nil {
		printed += string(*raw)
	}
	if a != nil {
		printed += fmt.Sprint(*a)
	}
	if s != nil && *s != nil {
		printed += **s
	}
	if b != nil {
		printed += string(*b)
	}
	redacttest.RequireNone(t, "redact.answer-whole-behind-pointers", printed)
	var control *echoed
	if err := refusal.Decode(&control); err != nil || control == nil || control.Message.IsEmpty() {
		t.Fatalf("control: Decode of a struct behind two pointers = %v, %+v; want it filled", err, control)
	}
}

// rawDetail holds a json.RawMessage member, which encoding/json fills with the member's bytes as
// they came.
type rawDetail struct {
	Detail json.RawMessage `json:"detail"`
}

// selfDecoded decodes its JSON itself, into an interface member, as a type of another module may
// (a JSON Web Key does): what it keeps is its own code's, and encoding/json fills none of its
// members.
type selfDecoded struct {
	Value any
}

func (s *selfDecoded) UnmarshalJSON(data []byte) error {
	s.Value = len(data)
	return nil
}

// rawEmbedded embeds a json.RawMessage, whose UnmarshalJSON is promoted to it: encoding/json hands
// rawEmbedded its JSON whole through that method, and the embedded RawMessage keeps the JSON as it
// came.
type rawEmbedded struct{ json.RawMessage }

// rawEmbeddedTwice embeds rawEmbedded, so the RawMessage's UnmarshalJSON is promoted to it
// through two embeddings.
type rawEmbeddedTwice struct{ rawEmbedded }

// rawAlias is json.RawMessage under an unexported name of this package's own. Embedded, its
// UnmarshalJSON is promoted all the same.
type rawAlias = json.RawMessage

// rawBytes is a byte slice of an unexported type, which encoding/json leaves alone where it is
// embedded: it fills no embedded field of an unexported type that is no struct.
type rawBytes []byte

// keySet is a JSON Web Key Set of one public signing key, as an issuer publishes it: its keys
// decode themselves (jose.JSONWebKey's UnmarshalJSON), as internal/oidc decodes a key set.
func keySet(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	set, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "ES256", Use: "sig"}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(set)
}

// TestAnAnswerRefusesADestinationThatHoldsAPlainTextMember: encoding/json fills a json.RawMessage
// or a byte slice with a member's bytes as they came, a byte array with the bytes a JSON array of
// numbers spells, which fmt prints as text, and an interface value with a JSON value of its own,
// text and all, so each keeps the answer's text with no record of the answer and prints a
// credential the answer echoes. Decode refuses a destination that holds one at any depth it
// decodes to - a struct's member, an embedded struct's, a list's item, an array's, a map's value,
// behind a nil pointer, in an interface value the caller set to a pointer, an interface value the
// caller set to a value (which encoding/json replaces), the destination a map or a list itself -
// before it decodes anything, with fixed text that quotes no word of the answer. So does it where
// the RawMessage's own UnmarshalJSON is promoted to a struct that embeds it, through which
// encoding/json hands that struct its JSON whole: the destination such a struct, a member of that
// type, a struct that embeds it in turn, and one that embeds the RawMessage under an unexported
// name. The controls are a json.RawMessage encoding/json does not fill (`json:"-"`, unexported, an
// embedded byte slice of an unexported type), an interface member of a type that decodes its JSON
// itself, and go-jose's key set, whose keys decode their JSON themselves, each of which Decode
// fills. Captures redact.answer-plain-member/<case>.
func TestAnAnswerRefusesADestinationThatHoldsAPlainTextMember(t *testing.T) {
	type anyDetail struct {
		Detail any `json:"detail"`
	}
	echo := func(auth string) string { return `{"detail":` + refusedWith(auth) + `}` }
	nested := func(auth string) string { return `{"detail":` + echo(auth) + `}` }
	said := func(auth string) string { return `{"detail":` + strconv.Quote("the token "+auth+" was refused") + `}` }
	encoded := func(auth string) string {
		return `{"detail":"` + base64.StdEncoding.EncodeToString([]byte("the token "+auth+" was refused")) + `"}`
	}
	listed := func(auth string) string { return `{"detail":[` + refusedWith(auth) + `]}` }
	numbered := func(auth string) string {
		var numbers []string
		for _, b := range []byte("the token " + auth + " was refused") {
			numbers = append(numbers, strconv.Itoa(int(b)))
		}
		return `{"detail":[` + strings.Join(numbers, ",") + `]}`
	}
	for _, c := range []struct {
		name   string
		answer func(auth string) string
		into   func() any
	}{
		{"raw", echo, func() any { return new(rawDetail) }},
		{"bytes", encoded, func() any {
			return new(struct {
				Detail []byte `json:"detail"`
			})
		}},
		{"byte-array", numbered, func() any {
			return new(struct {
				Detail [64]byte `json:"detail"`
			})
		}},
		{"interface", said, func() any { return new(anyDetail) }},
		{"map-value", echo, func() any {
			return new(struct {
				Detail map[string]any `json:"detail"`
			})
		}},
		{"list-item", listed, func() any {
			return new(struct {
				Detail []any `json:"detail"`
			})
		}},
		{"array-item", listed, func() any {
			return new(struct {
				Detail [1]any `json:"detail"`
			})
		}},
		{"embedded", echo, func() any { return new(struct{ rawDetail }) }},
		{"nil-pointer", nested, func() any {
			return new(struct {
				Detail *rawDetail `json:"detail"`
			})
		}},
		{"interface-set-to-a-pointer", nested, func() any { return &anyDetail{Detail: &rawDetail{}} }},
		{"interface-set-to-a-value", echo, func() any { return &anyDetail{Detail: rawDetail{}} }},
		{"map-destination", echo, func() any { return new(map[string]json.RawMessage) }},
		{"list-destination", func(auth string) string { return `[` + echo(auth) + `]` }, func() any { return new([]rawDetail) }},
		{"promoted", echo, func() any { return new(rawEmbedded) }},
		{"promoted-member", echo, func() any {
			return new(struct {
				Detail *rawEmbedded `json:"detail"`
			})
		}},
		{"promoted-twice", echo, func() any { return new(rawEmbeddedTwice) }},
		{"promoted-alias", echo, func() any { return new(struct{ rawAlias }) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			into := c.into()
			err := answerOf(t, http.StatusForbidden, c.answer).Decode(into)
			marshaled, _ := json.Marshal(into)
			printed := fmt.Sprintf("%s %s", into, marshaled)
			redacttest.RequireNone(t, "redact.answer-plain-member/"+c.name, fmt.Sprintf("%s | %v", printed, err))
			switch {
			case err == nil:
				t.Errorf("Decode filled %s, whose member keeps the answer's text as it came: %s", c.name, printed)
			case strings.Contains(err.Error(), "was refused") || !strings.Contains(err.Error(), "which holds"):
				t.Errorf("Decode's refusal %q quotes the answer or does not name what the destination holds", err)
			case strings.Contains(printed, "was refused"):
				t.Errorf("Decode refused %s after it filled it: %s", c.name, printed)
			}
		})
	}
	for _, c := range []struct {
		name string
		into any
	}{
		{"skipped", &struct {
			Detail  json.RawMessage   `json:"-"`
			Message redact.RemoteText `json:"message"`
		}{}},
		{"unexported", &struct {
			detail  json.RawMessage
			Message redact.RemoteText `json:"message"`
		}{}},
		{"decodes-itself", &struct {
			Message selfDecoded `json:"message"`
		}{}},
		{"unexported-embedded-bytes", &struct {
			rawBytes
			Message redact.RemoteText `json:"message"`
		}{}},
	} {
		if err := answerOf(t, http.StatusForbidden, refusedWith).Decode(c.into); err != nil {
			t.Errorf("control %s: Decode = %v, want it filled: encoding/json keeps no text as it came there", c.name, err)
		}
		redacttest.RequireNone(t, "redact.answer-plain-member-control/"+c.name, fmt.Sprintf("%s", c.into))
	}
	set := keySet(t)
	var keys jose.JSONWebKeySet
	if err := answerOf(t, http.StatusOK, func(string) string { return set }).Decode(&keys); err != nil || len(keys.Keys) != 1 || keys.Keys[0].KeyID != "k1" {
		t.Errorf("control key-set: Decode = %v, %d keys; want the one key, which decodes its JSON itself", err, len(keys.Keys))
	}
}

// boxedKey is a map key that holds the box behind a pointer it embeds, as a key of a struct type
// may: its UnmarshalText is the box's own, promoted through the pointer, which is nil in the zero
// key encoding/json makes, so encoding/json panicked decoding into it.
type boxedKey struct{ *redact.RemoteText }

// printedKeys is each key of every map v holds - through a pointer, an interface value, a list or
// a struct's member - as fmt prints it.
func printedKeys(v reflect.Value) string {
	var b strings.Builder
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			b.WriteString(printedKeys(v.Elem()))
		}
	case reflect.Slice:
		for i := range v.Len() {
			b.WriteString(printedKeys(v.Index(i)))
		}
	case reflect.Struct:
		for i := range v.NumField() {
			b.WriteString(printedKeys(v.Field(i)))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			fmt.Fprintf(&b, "%v ", key.Interface())
		}
	}
	return b.String()
}

// TestDecodeRefusesADestinationWhoseMapKeyHoldsABox: encoding/json fills a map's key through the
// key's UnmarshalText, which the box has, and a map's key has no address, so no record of the
// answer reaches a box there: a key that echoes the request's bearer prints it as it came. The box
// itself, and every type that holds it, is no map key (it holds its record behind a func, which
// makes it not comparable), but a key can hold it behind a pointer. Decode refuses such a
// destination before it decodes anything - a map keyed by a pointer to the box or to a
// RemoteNumber, a struct's member, a list of such maps, one behind a nil pointer, one behind an
// interface value the caller set to a pointer, and a struct key that embeds a pointer to the box,
// into which encoding/json panicked - with fixed text that quotes no word of the answer, and the
// destination holds no key. The control is the same answer decoded with the key as a plain string,
// which encoding/json fills. Captures redact.answer-box-key/<case>.
func TestDecodeRefusesADestinationWhoseMapKeyHoldsABox(t *testing.T) {
	for _, box := range []reflect.Type{reflect.TypeFor[redact.RemoteText](), reflect.TypeFor[redact.RemoteNumber]()} {
		if box.Comparable() {
			t.Errorf("%v is comparable, so a map can key on it, where no record reaches it", box)
		}
	}
	keyed := func(auth string) string { return `{"the token ` + auth + ` was refused":1}` }
	member := func(auth string) string { return `{"codes":` + keyed(auth) + `}` }
	listed := func(auth string) string { return `{"codes":[` + keyed(auth) + `]}` }
	var top map[*redact.RemoteText]int
	var field struct {
		Codes map[*redact.RemoteText]int `json:"codes"`
	}
	var number map[*redact.RemoteNumber]int
	var list struct {
		Codes []map[*redact.RemoteText]int `json:"codes"`
	}
	var nilPointer struct {
		Codes *map[*redact.RemoteText]int `json:"codes"`
	}
	set := map[*redact.RemoteText]int{}
	var behind struct {
		Codes any `json:"codes"`
	}
	behind.Codes = &set
	var embedded map[boxedKey]int
	for _, c := range []struct {
		name   string
		answer func(auth string) string
		into   any
	}{
		{"map", keyed, &top},
		{"member", member, &field},
		{"number", keyed, &number},
		{"list", listed, &list},
		{"nil-pointer", member, &nilPointer},
		{"interface", member, &behind},
		{"embedded-pointer", keyed, &embedded},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := answerOf(t, http.StatusForbidden, c.answer)
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("Decode panicked: %v", p)
					}
				}()
				return answer.Decode(c.into)
			}()
			printed := printedKeys(reflect.ValueOf(c.into))
			redacttest.RequireNone(t, "redact.answer-box-key/"+c.name, fmt.Sprintf("%s| %v", printed, err))
			switch {
			case err == nil:
				t.Errorf("Decode filled %s, whose map's key holds a box no record of the answer reaches: %s", c.name, printed)
			case strings.Contains(err.Error(), "was refused") || !strings.Contains(err.Error(), "map's key"):
				t.Errorf("Decode's refusal %q quotes the answer or does not name the map's key", err)
			case printed != "":
				t.Errorf("Decode refused %s after it filled it: %s", c.name, printed)
			}
		})
	}
	var control map[string]int
	if err := answerOf(t, http.StatusForbidden, keyed).Decode(&control); err != nil || len(control) != 1 {
		t.Fatalf("control: Decode of the same answer keyed by a plain string = %v, %v; want it filled", err, control)
	}
}
