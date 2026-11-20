package urlsetting

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// Value is a setting that can hold a URL, and so a user name and a password
// (https://user:password@host), or a token written where the user stands. Every Go setting of
// this repository that can hold such a URL is read into a Value where it is read - an environment
// variable (New(os.Getenv(name))), a flag (fs.Var(&v, name, usage)), a field of a decoded
// configuration file (a Value field, which UnmarshalText fills) - and it stays a Value through
// every field and argument that carries it. No output can print it whole: String, GoString,
// Format, LogValue, MarshalText and MarshalJSON all give the one cleaned form (redact.URL): the
// value as written when it is provably a plain http(s) URL, and redact.NotPrinted otherwise.
//
// Raw is the one way to the value as configured, and packages/daemon/cmd/guarded-url-check holds
// every call of it to the URL argument of a consumer (a request, a dial, a parse, a cleaner that
// names it by length) or to a comparison. A message names the value through the Value itself, a
// request URL built from it through Call, and that request's error through Error, RequestError,
// Cause or Named.
//
// The text sits behind a *string so that fmt cannot print it where it asks the Value for no
// method: a Value in a struct's unexported field, or under %p, fmt prints field by field through
// reflection, and a *string it finds there prints as the memory address it holds, never as the
// text it points to. Two Values made from the same text are therefore not ==; Equal compares them.
//
// The package is outside internal/ because packages/daemon reads such settings too, and Go's
// internal-package rule refuses that module every package under github.com/sjawhar/envoy/internal/.
type Value struct {
	raw *string
}

// New is raw as a Value, kept exactly as written: a caller that trims or checks the value does so
// before, as it would with the string. Empty input is the zero Value, so an unset and an
// explicitly empty setting behave alike.
func New(raw string) Value {
	if raw == "" {
		return Value{}
	}
	return Value{raw: &raw}
}

// Raw is the value as configured, credential included, for the URL argument of a request, a dial,
// a parse or a cleaner, and for a comparison. It is never printed.
func (v Value) Raw() string {
	if v.raw == nil {
		return ""
	}
	return *v.raw
}

// Empty reports whether the setting is unset or explicitly empty.
func (v Value) Empty() bool { return v.raw == nil }

// Equal reports whether v and w hold the same text. go-cmp calls it, so a struct holding Values
// compares by their text.
func (v Value) Equal(w Value) bool { return v.Raw() == w.Raw() }

// Clean reports whether the value prints as written: it is set, and redact.Clean accepts it.
func (v Value) Clean() bool { return v.raw != nil && redact.Clean(*v.raw) }

func (v Value) cleaned() string {
	if v.raw == nil {
		return ""
	}
	return redact.URL(*v.raw)
}

// String is the value as a message may print it: redact.URL of it.
func (v Value) String() string { return v.cleaned() }

// GoString is String, so %#v quotes the printed form rather than spelling out the Value's fields.
func (v Value) GoString() string { return v.cleaned() }

// Format prints String under the verb, flags, width and precision it is given, as fmt prints that
// string, so %q quotes the printed form and %x and %X hex-encode it.
func (v Value) Format(state fmt.State, verb rune) {
	fmt.Fprintf(state, fmt.FormatString(state, verb), v.cleaned())
}

// LogValue is String, so a structured log field names the value only as a message may.
func (v Value) LogValue() slog.Value { return slog.StringValue(v.cleaned()) }

// MarshalText is String, for a text encoder that prints a diagnostic. A document that must keep
// the value (a state file, a request body) holds the text a consumer built from Raw, never a Value.
func (v Value) MarshalText() ([]byte, error) { return []byte(v.cleaned()), nil }

// MarshalJSON is String as a JSON string, for the same reason as MarshalText.
func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.cleaned()) }

// UnmarshalText reads a configuration document's field as a Value: encoding/json, yaml.v3 and the
// TOML decoders fill a Value field through it, so the value is a Value from the moment it is read.
func (v *Value) UnmarshalText(text []byte) error {
	*v = New(string(text))
	return nil
}

// Set reads a flag's value as a Value (flag.Value and pflag.Value): fs.Var(&v, name, usage)
// defines a URL flag whose value is a Value from the moment the flag set parses it.
func (v *Value) Set(raw string) error {
	*v = New(raw)
	return nil
}

// Type is the word pflag's usage names a Value flag's argument by.
func (v Value) Type() string { return "url" }

// TrimSpace is the value with its leading and trailing white space removed, as strings.TrimSpace.
func (v Value) TrimSpace() Value { return New(strings.TrimSpace(v.Raw())) }

// TrimRight is the value with every trailing character in cutset removed, as strings.TrimRight.
func (v Value) TrimRight(cutset string) Value { return New(strings.TrimRight(v.Raw(), cutset)) }

// Split is the value cut at each sep, as strings.Split, each piece a Value: a list setting
// (NATS_URLS) is a list of Values.
func (v Value) Split(sep string) []Value {
	if v.raw == nil {
		return nil
	}
	pieces := strings.Split(*v.raw, sep)
	values := make([]Value, len(pieces))
	for i, piece := range pieces {
		values[i] = New(piece)
	}
	return values
}

// Join is values joined with sep, as strings.Join, as one Value: a list setting handed to a
// consumer that takes the list as one string (nats.Connect).
func Join(values []Value, sep string) Value {
	pieces := make([]string, len(values))
	for i, value := range values {
		pieces[i] = value.Raw()
	}
	return New(strings.Join(pieces, sep))
}

// Refuse is the package's Refuse of the value: the refusal of a value whose user part net/url reads
// apart, naming setting, the value's length and the cause, or nil.
func (v Value) Refuse(setting string) error { return Refuse(setting, v.Raw()) }

// WordsNotPrinted is what a message prints in place of the text of an answer from an address that
// is not clean (Words).
const WordsNotPrinted = "(not printed: the broker's address is not clean)"

// Words is text that whoever answered a request built from Raw wrote - a refusal's words, its
// code, the answer id it names, a value it quotes - as a message may print it: beside a clean
// value the text on one line (redact.OneLine), as it came where it holds no control character and
// Go-quoted where it holds one, so a line break or an escape sequence in an answer cannot start a
// line of its own in the message that prints it; and WordsNotPrinted beside any other. Whoever
// answers at an address that is not clean, or anyone who can answer for it, can repeat the
// credential the address carries in any text of its answer, so a message made from such an answer
// prints its status and WordsNotPrinted, and no other part of it. A caller that tells answers apart
// by their text reads that text as it came, never what Words answers, and prints it nowhere.
func (v Value) Words(text string) string {
	if v.Clean() {
		return redact.OneLine(text)
	}
	return WordsNotPrinted
}

// Call is redact.Call with the value as the base: callURL, a request URL built from Raw, as a
// message may print it.
func (v Value) Call(callURL string) string { return redact.Call(v.Raw(), callURL) }

// Error is redact.Error with the value as the base: err, from a request built from Raw, as a
// message may print it.
func (v Value) Error(err error) error { return redact.Error(err, v.Raw()) }

// RequestError is redact.RequestError with the value as the base: a request's build or transport
// error, whose text net/url and net/http write with the request URL in it, with that URL as a
// message may print it.
func (v Value) RequestError(err error) error { return redact.RequestError(err, v.Raw()) }

// Cause is redact.Cause with the value as the base: Error without net/http's *url.Error wrapper,
// for a message that names the call itself.
func (v Value) Cause(err error) error { return redact.Cause(err, v.Raw()) }

// Named is redact.Named with the value as the base: err, from a request built from Raw, named by
// name, the caller's own words for the call, for a request URL that ends in what a caller typed.
// No Error, Cause or Class of the result names that URL again.
func (v Value) Named(err error, name string) error { return redact.Named(err, v.Raw(), name) }
