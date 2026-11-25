// Package cipheraddr holds the secrets broker's configured address (AGENT_SECRETS_URL, and every
// setting that names the same broker) in a type no output can print whole. Such an address may
// carry a user name and a password (http://user:password@host:port), and a plain string holding it
// prints both wherever a log field, an error or a line on stderr names the broker. An Address
// prints only what internal/redact's URL prints - the address as written when it is provably a
// plain http(s) URL, and redact.NotPrinted otherwise - under every fmt verb, as a slog value, and
// as a field of a struct fmt prints. Join builds a request URL from it; Raw reads its value, for
// the rest of a request to the broker and, without printing it, for the few checks Raw names.
//
// It is a package of its own, outside internal/, because packages/daemon's `legion controller
// start` holds the address too, and Go's internal-package rule refuses that module every package
// under github.com/sjawhar/envoy/internal/.
package cipheraddr

import (
	"fmt"
	"log/slog"
	"net/url"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// Address is a configured broker address. Its zero value is no address, and prints empty. A request
// URL at the address is built by Join, which joins a path onto the address's path and keeps its
// query, never by appending a path to Raw's text, which would sit inside that query or a fragment.
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
// URL, which Join builds from it (no caller joins a path onto it by hand), the URL its proof signs,
// the audience of a request object it carries, and the OAuth endpoints. Five callers read it for
// anything else, and none of them prints it: agent-secrets-helper's check that an address is set
// (its serve), the token client's trim of the address it is given (cipherclient.New),
// agent-secrets' check that a broker whose settings it acts on is https or loopback
// (requireTrustedBrokerURL), and its two checks of an id it prints by its shape: that the address
// could carry no credential (plainAddress) and does not hold the id (addressHolds). Every caller
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

// Join is the request URL of ref at the address: urljoin.Join of Raw and ref, ref's path joined
// onto the address's path, the address's query kept ahead of ref's own, its user information, host
// and port kept and its fragment dropped. An address that does not parse, has no scheme or has no
// host is urljoin.ErrBase, which quotes no part of it.
func (a Address) Join(ref string) (*url.URL, error) { return urljoin.Join(a.Raw(), ref) }

// Empty reports whether no address is configured.
func (a Address) Empty() bool { return a.raw == nil }

// Clean reports whether the address prints as written: it is set, and redact.Clean accepts it.
func (a Address) Clean() bool { return redact.Clean(a.Raw()) }

// WordsNotPrinted is what a message prints in place of the text of an answer from an address that
// is not clean (Words). It is a different case from redact.WordsNotReadable, which Words prints
// beside a clean address in place of a field the rule for remote text cannot read.
const WordsNotPrinted = "(not printed: the broker's address is not clean)"

// Words is text that whoever answered a request built from Raw wrote - a refusal's words, its
// code, the answer id it names, a value it quotes - as a message may print it. Beside a clean
// address it is the text as the rule for remote text reads it (redact.Field): ordinary words as
// they came, a URL with no user information and no query or fragment value, and
// redact.WordsNotReadable in place of a field the rule cannot read as clean; on one line
// (redact.OneLine), Go-quoted where it holds a control character, so a line break or an escape
// sequence in an answer cannot start a line of its own in the message that prints it. Beside any
// other address it is WordsNotPrinted: whoever answers at an address that is not clean, the
// broker or anyone who can answer for it, can repeat the credential the address carries in any
// text of its answer, so a message made from such an answer prints its status and
// WordsNotPrinted, and no other part of it. A caller that tells answers apart by their text reads
// that text as it came, never what Words answers, and prints it nowhere. A decoder's error for an
// answer the helper or `agent-secrets enroll --pod` cannot read prints through Words as well,
// since the decoder quotes the answer's text.
func (a Address) Words(text string) string {
	if a.Clean() {
		return redact.Field(text)
	}
	return WordsNotPrinted
}

// WordsOf is Words of a field held as a redact.RemoteText: the field as it prints itself beside a
// clean address, and WordsNotPrinted beside any other.
func (a Address) WordsOf(field redact.RemoteText) string {
	if a.Clean() {
		return field.String()
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

// Route is route, a broker route whose id a caller typed is spelled as words in its place (as
// "/v1/enrollments/(the enrollment id, not printed)"), at the address as a message may print it:
// route joined onto the address as a request's path is (Join), its words as written, and
// redact.NotPrinted beside an address that is not clean. It names a call whose URL ends in what a
// caller typed (Named), since the broker never accepted that id and an argument that is no id is
// where a credential pasted in the wrong place lands.
func (a Address) Route(route string) string {
	joined, err := a.Join(route)
	if err != nil || !a.Clean() {
		return redact.NotPrinted
	}
	return joined.Scheme + "://" + joined.Host + joined.Path
}

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
