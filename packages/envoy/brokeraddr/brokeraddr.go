// Package brokeraddr holds the secrets broker's configured address (AGENT_SECRETS_URL, and every
// setting that names the same broker) in a type no output can print whole. Such an address may
// carry a user name and a password (http://user:password@host:port), and a plain string holding it
// prints both wherever a log field, an error or a line on stderr names the broker. An Address
// prints only what internal/redact's URL prints - the address as written when it is provably a
// plain http(s) URL, and redact.NotPrinted otherwise - under every fmt verb, as a slog value, and
// as a field of a struct fmt prints. Its value is read through Raw, to build a request to the
// broker and, without printing it, by the few checks Raw names.
//
// It is a package of its own, outside internal/, because packages/daemon's `legion controller
// start` holds the address too, and Go's internal-package rule refuses that module every package
// under github.com/sjawhar/envoy/internal/.
package brokeraddr

import (
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/redact"
)

// Address is a configured broker address. Its zero value is no address, and prints empty.
//
// The address sits behind a *string so that fmt cannot print it where it asks the Address for no
// method: an Address in a struct's unexported field, or under %p, fmt prints field by field through
// reflection, and it prints a *string it finds there as the memory address it holds, never as the
// text it points to. A string field would print as written there, and so would a pointer to a
// struct, which fmt follows when it reports a verb a pointer does not take, such as %s. Two
// Addresses made from the same text are therefore not ==.
type Address struct {
	raw *string
}

// New is raw as an Address, kept exactly as written: a caller that trims or checks the value does
// so before, as it would with the string.
func New(raw string) Address {
	if raw == "" {
		return Address{}
	}
	return Address{&raw}
}

// Raw is the address as configured, credential included. It builds a request to the broker: its
// URL, the URL its proof signs, the audience of a request object it carries, and the OAuth
// endpoints. Five callers read it for anything else, and none of them prints it: the helper's
// check that a URL it is asked to sign is one of its operator routes (internal/broker/helper's
// operatorRoute), agent-secrets-helper's check that an address is set (its serve), the token
// client's trim of the address it is given (brokerclient.New), agent-secrets' check that a
// broker whose settings it acts on is https or loopback (requireTrustedBrokerURL), and its check
// that an id it prints by its shape is no value the address holds (addressHolds). Every caller
// keeps what it answers out of every output - a log field, an error's text, a line on stderr -
// since printing it is the leak this type exists to prevent: a message names the address through
// the Address itself, a request URL built from it through Call, and that request's error through
// Error or Cause, or through Named where the URL ends in what a caller typed.
func (a Address) Raw() string {
	if a.raw == nil {
		return ""
	}
	return *a.raw
}

// Empty reports whether no address is configured.
func (a Address) Empty() bool { return a.raw == nil }

// Clean reports whether the address prints as written: it is set, and redact.Clean accepts it.
func (a Address) Clean() bool { return redact.Clean(a.Raw()) }

// WordsNotPrinted is what a message prints in place of the text of an answer from an address that
// is not clean (Words).
const WordsNotPrinted = "(not printed: the broker's address is not clean)"

// Words is text that whoever answered a request built from Raw wrote - a refusal's words, its
// code, the answer id it names, a value it quotes - as a message may print it: beside a clean
// address the text on one line (redact.OneLine), as it came where it holds no control character
// and Go-quoted where it holds one, so a line break or an escape sequence in an answer cannot
// start a line of its own in the message that prints it; and WordsNotPrinted beside any other.
// Whoever answers at an address that is not clean, the broker or anyone who can answer for it,
// can repeat the credential the address carries in any text of its answer, so a message made
// from such an answer prints its status and WordsNotPrinted, and no other part of it. A caller
// that tells answers apart by their text reads that text as it came, never what Words answers,
// and prints it nowhere. A decoder's error for an answer the helper or `agent-secrets enroll --pod`
// cannot read prints through Words as well, since the decoder quotes the answer's text.
func (a Address) Words(text string) string {
	if a.Clean() {
		return redact.OneLine(text)
	}
	return WordsNotPrinted
}

// String is the address as a message may print it: redact.URL of it.
func (a Address) String() string { return redact.URL(a.Raw()) }

// Format prints String under the verb, flags, width and precision it is given, as fmt prints that
// string, so %q quotes the printed form, %x and %X hex-encode it, and %#v quotes it rather than
// spelling out the Address's fields as Go syntax would.
func (a Address) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), a.String())
}

// LogValue is String, so a structured log field names the address only as a message may.
func (a Address) LogValue() slog.Value { return slog.StringValue(a.String()) }

// Call is redact.Call with the address as the base: callURL, a request URL built from Raw, as a
// message may print it.
func (a Address) Call(callURL string) string { return redact.Call(a.Raw(), callURL) }

// Error is redact.Error with the address as the base: err, from a request built from Raw, as a
// message may print it.
func (a Address) Error(err error) error { return redact.Error(err, a.Raw()) }

// Cause is redact.Cause with the address as the base: Error without net/http's *url.Error wrapper,
// for a message that names the call itself.
func (a Address) Cause(err error) error { return redact.Cause(err, a.Raw()) }

// Named is redact.Named with the address as the base: err, from a request built from Raw, named
// by name, the caller's own words for the call, for a request URL that ends in what a caller typed.
// No Error, Cause or Class of the result names that URL again. The helper's revoke is the case:
// its route ends in the enrollment id it was handed, which `unenroll --enrollment` takes as typed
// and the broker never accepted, so its failure names the route without the id, and neither
// UNENROLL_FAILED nor UNENROLL_OUTCOME_UNKNOWN prints the request URL.
func (a Address) Named(err error, name string) error { return redact.Named(err, a.Raw(), name) }
