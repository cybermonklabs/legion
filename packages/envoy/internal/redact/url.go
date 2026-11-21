// Package redact is how a URL a person configured is printed: in an error, a log line or a
// message, with nothing in it that can carry a credential.
//
// It works by allow-list. A value prints only when it is provably a plain http(s) URL with no
// place a credential can sit; any other value prints as NotPrinted, whatever it holds. No part of
// a value that fails the check is ever printed, so no encoding, separator or control byte in it
// can make the redactor print a piece it should have hidden.
//
// An error of a request built from that URL (Error, Cause) prints text of its own only beside a
// clean URL and a request URL Call prints, and only where that text is a transport failure's, text
// the allow-list passes, or the broker's own OAuth words; beside any other URL it prints the
// failure's class alone, except words an earlier Error, Cause or Named already gave, which print
// as given (below). The broker's OAuth words are trusted, not checked, so
// whatever the broker puts in them prints: an OAuth error answer (*oauth2.RetrieveError) of the
// broker's own with a nonempty error code and an empty error_uri prints its error code and
// error_description whole, as the broker wrote them, since they are the reason for a refusal a
// person acts on. It is never one that Class or Masked holds, another server's, because printable
// checks nothing under an error this package already put in words; BrokerAnswer, for printers
// outside it, finds the same answer. No other field of an OAuth answer ever prints: an answer that
// carries an error_uri, or one with no error code (whose text is the response body x/oauth2 kept),
// prints as the failure's class alone, "the broker answered HTTP <n>", as every answer does where
// its text may not print; Error puts the *url.Error's operation and URL, through Call, before it,
// or the caller's own name for the call (Named). A class is fixed text alone: Cause and Class of
// such an error print the class, never the line that names its call, and a later Error prints
// that same line, never the URL anew. What prints is always the checked error's own text: words a
// caller wrapped around the failure or the *url.Error, with fmt.Errorf's %w or an error type of
// its own, are no part of what was checked, so they never print. The one error type whose words
// print over a trusted answer is one that vouches for them (Printable): its text is fixed words,
// words of its own that neither the answer nor a caller writes, and that answer's fields alone.
// Nothing under an error this package already put in words is checked again: what Error, Cause,
// Named, Class, Masked and File return keeps the failure they withheld reachable for errors.As, so
// a second Error or Cause of it, beside any URL and under any wrapper, prints the words already
// given, never the answer the first one withheld.
package redact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/nettimeout"
	"github.com/sjawhar/envoy/internal/tlsalert"
)

// NotPrinted stands in for a value URL will not print.
const NotPrinted = "(not printed: the value carries a credential or is not a plain http(s) URL)"

// URL is raw when raw is provably clean, the empty string as it is, and NotPrinted for anything
// else. raw is clean when all of these hold:
//   - every byte is printable ASCII other than '@', '%', '?', '#' and '\' (so no user info, no
//     escape, no query, no fragment, no whitespace and no control byte);
//   - net/url parses it, with an http or https scheme (any case), no user, no opaque part;
//   - it reads exactly as scheme "://" host path, with nothing net/url dropped or decoded;
//   - the host is a DNS name, an IPv4 address or a bracketed IPv6 address with no zone, with an
//     optional port of one to five digits;
//   - the path is empty or only letters, digits, '.', '_', '~', '/' and '-'.
//
// A URL with user info (https://user:password@host) prints as NotPrinted too, rather than as
// https://xxxxx@host: a value whose password holds an '@' and that names no host at all
// (https://user:p@ssword) would print the password's tail as the host, and no check on the text
// can tell the two apart.
//
// agent-secrets prints each URL setting through URL, beside the variable that names it, so a
// value URL withholds still says where to look: the sign-in issuer as
// "(AGENT_SECRETS_SIGNIN_ISSUER names <URL>)", the approval page beside AGENT_SECRETS_APPROVE_URL,
// and the metadata identity's audience as "<URL> (AGENT_SECRETS_MACHINE_AUDIENCE)" where it is
// clean and as "the audience AGENT_SECRETS_MACHINE_AUDIENCE names" otherwise.
func URL(raw string) string {
	if raw == "" || clean(raw) {
		return raw
	}
	return NotPrinted
}

// Clean reports whether raw is not empty and URL prints it unchanged.
func Clean(raw string) bool { return raw != "" && clean(raw) }

// clean reports whether raw passes URL's allow-list.
func clean(raw string) bool {
	for i := range len(raw) {
		if c := raw[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`@%?#\`, c) >= 0 {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return false
	}
	n := len(u.Scheme) + len("://")
	if len(raw) < n || !strings.EqualFold(raw[:n], u.Scheme+"://") || raw[n:] != u.Host+u.Path {
		return false
	}
	return hostPort(u.Host) && plainPath(u.Path)
}

// hostPort reports whether hostport is a DNS name, an IPv4 address or a bracketed IPv6 address
// without a zone, with an optional port of one to five digits.
func hostPort(hostport string) bool {
	host, port, hasPort := hostport, "", false
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return false
		}
		host = hostport[1:end]
		if rest := hostport[end+1:]; rest != "" {
			if rest[0] != ':' {
				return false
			}
			port, hasPort = rest[1:], true
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return false
		}
	} else {
		if i := strings.IndexByte(hostport, ':'); i >= 0 {
			host, port, hasPort = hostport[:i], hostport[i+1:], true
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if !addr.Is4() {
				return false
			}
		} else if !dnsName(host) {
			return false
		}
	}
	if hasPort && (port == "" || len(port) > 5 || strings.Trim(port, "0123456789") != "") {
		return false
	}
	return true
}

// dnsName reports whether name is a DNS name: dot-separated labels of 1 to 63 letters, digits
// and '-', none starting or ending with '-', 253 bytes at most.
func dnsName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// plainPath reports whether path holds only letters, digits, '.', '_', '~', '/' and '-'.
func plainPath(path string) bool {
	for i := range len(path) {
		c := path[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._~/-", c) >= 0) {
			return false
		}
	}
	return true
}

// Call is callURL, a request URL a program built as base followed by a broker route, as a message
// may print it: unchanged when base is clean and callURL is base (its scheme in any case, since
// net/url lowers it) followed by nothing or a path under /v1/ of the characters plainPath allows,
// and NotPrinted otherwise - a URL on another host or another path, such as a redirect's target,
// included. Every route the broker serves is under /v1/.
func Call(base, callURL string) string {
	if callClean(base, callURL) {
		return callURL
	}
	return NotPrinted
}

// callClean reports whether Call prints callURL unchanged.
func callClean(base, callURL string) bool {
	if !clean(base) || len(callURL) < len(base) {
		return false
	}
	n := strings.Index(base, "://") + len("://")
	if !strings.EqualFold(callURL[:n], base[:n]) || callURL[n:len(base)] != base[n:] {
		return false
	}
	rest := callURL[len(base):]
	route := rest
	if strings.HasSuffix(base, "/") {
		route = "/" + rest
	}
	return (rest == "" || strings.HasPrefix(route, "/v1/")) && plainPath(rest)
}

// Error is err as a message may print it, given raw, the configured URL err's request was built
// from. Where printable passes it, it prints the text printable checked, which is err's own text
// when no wrapper added words to it, and then it is err itself, so every message reads as before:
// raw is clean, and that text holds nothing from a URL it may not print but for the error code and
// error_description of an OAuth answer with an error code and no error_uri, which print whole as
// the broker wrote them (trustedAnswer). Words a caller wrapped around the failure or around the
// *url.Error are dropped, since nothing checked them. Otherwise no text of err is printed: the
// error's text names only the class of the failure (a URL that did not parse, failed name
// resolution, a refused connection, a timeout, a TLS failure, an HTTP status, or a request that
// failed otherwise), found on err itself, and the *url.Error's operation with its URL through Call.
// A line that names a call sits over the text printed of its failure (a called), so Cause and Class
// of the result print that text and never the line. A call already named, by Error or by a
// caller's own words for it (Named), keeps that name: its URL is not named again, so a part of it
// that Named left out does not come back. It unwraps to err, so errors.Is and errors.As still
// classify the failure.
func Error(err error, raw string) error {
	if err == nil {
		return nil
	}
	ue, checked, printable := printable(err, raw)
	text := class(err, "the broker")
	if printable {
		text = checked
	}
	var named *called
	switch {
	case errors.As(err, &named):
		// A called always holds a classified, which printable never passes, so text is the class
		// beneath its line.
		return &called{named.text, &classified{text, err}}
	case ue != nil:
		// The call's line names the URL as Call prints it; the text under it stays URL-free, so Cause
		// and Class of this error print that text alone, and a later Error prints this same line.
		line := ue.Op + " " + strconv.Quote(Call(raw, ue.URL)) + ": " + text
		if printable && line == err.Error() {
			return err
		}
		return &called{line, &classified{text, err}}
	}
	if printable && text == err.Error() {
		return err
	}
	return &classified{text, err}
}

// Named is err, from a request built from raw, named in the caller's own words rather than by its
// URL: name, ": " and Cause's text of err. It is for a request URL that ends in what a caller
// typed, such as an id the broker never issued, which Call prints as part of a plain path. Error
// over the result prints this line again, never the URL under it; Cause and Class of it print its
// class, never this line, since printable checks nothing under the classified it holds. It
// unwraps to err, so errors.Is and errors.As still classify the failure. Named(nil, raw, name) is
// nil.
func Named(err error, raw, name string) error {
	if err == nil {
		return nil
	}
	return &called{name + ": " + Cause(err, raw).Error(), &classified{class(err, "the broker"), err}}
}

// called is a line that names a failed call: its operation and URL as Call prints it (Error), or
// a caller's own words for it (Named), over the text printed of its failure: its class, or the
// text printable checked. class() reads that text beneath it, never this line, and Error over it
// prints this line rather than name the URL again.
type called struct {
	text string
	err  error
}

func (c *called) Error() string { return c.text }
func (c *called) Unwrap() error { return c.err }

// Cause is Error without the *url.Error's own wrapper, for a message that names the call itself:
// the error the *url.Error wraps when that error's text is the text printable checked, and
// otherwise that text, or the class alone where printable does not pass err, unwrapping to err.
func Cause(err error, raw string) error {
	if err == nil {
		return nil
	}
	ue, checked, printable := printable(err, raw)
	switch {
	case !printable:
		return &classified{class(err, "the broker"), err}
	case ue != nil && ue.Err != nil && ue.Err.Error() == checked:
		return ue.Err
	case ue == nil && err.Error() == checked:
		return err
	default:
		return &classified{checked, err}
	}
}

// printable answers the *url.Error err holds, if any, the text of the failure under it that may
// print, given raw (Error), and whether any may. All of these must hold:
//   - raw is clean;
//   - err holds no error this package already put in words (classified, which Error, Cause,
//     Class, Masked and File make and every called holds): what they return still unwraps to the
//     failure they withheld, beside an address that may carry a credential or for another server,
//     so checking that failure again beside a clean one would print the very text they withheld -
//     the broker's answer echoing the credential, say, or another server's answer. Such an error
//     prints the words already given (class);
//   - the URL the *url.Error names is raw followed by a plain path (callClean): net/http names a
//     redirect's target there, which can sit on another host or path and carry a query;
//   - the error the *url.Error wraps (err itself, with none) is one whose text names no URL, or
//     none but in the words the broker is trusted with (checked): a transport failure class names
//     (transport: a dial or other net.OpError, name resolution, a refused connection, a timeout,
//     TLS), an OAuth error answer with an error code and no error_uri, whose code and description
//     print whole (trustedAnswer), or text the allow-list passes. A malformed Location header,
//     say, puts the whole target in its error's text.
//
// The text it answers is the checked error's own, never a wrapper's: a caller's
// fmt.Errorf("grant %s: %w", id, cause) is that failure to errors.Is and errors.As, but its words
// are the caller's, and checking the failure says nothing about them.
func printable(err error, raw string) (ue *url.Error, text string, ok bool) {
	if !errors.As(err, &ue) {
		ue = nil
	}
	if raw == "" || !clean(raw) {
		return ue, "", false
	}
	var worded *classified
	if errors.As(err, &worded) {
		return ue, "", false
	}
	inner := err
	if ue != nil {
		// A *url.Error that wraps nothing names no failure to check.
		if !callClean(raw, ue.URL) || ue.Err == nil {
			return ue, "", false
		}
		inner = ue.Err
	}
	// A second *url.Error inside names a URL of its own, unchecked.
	var nested *url.Error
	if errors.As(inner, &nested) {
		return ue, "", false
	}
	text, ok = checked(inner)
	return ue, text, ok
}

// Printable is an error that vouches for its own words: an error type whose text is built only
// from fixed words, words of its own it held before the request, which neither the answer nor a
// caller writes (a refusal's name for the identity it traded), and the fields of the trusted OAuth
// answer it unwraps to (trustedAnswer), such as a client's sentence around the broker's refusal.
// printable passes one at its own level of the chain, as it passes that answer, and prints its
// PrintableText, never its Error. It passes one only where the error it unwraps to is itself a
// trusted answer, so the fields it is built from are ones printable would print anyway, and only
// where that text holds no control character: x/oauth2 quotes the answer's fields, and a sentence
// that does not would let a line break or an escape sequence from the answer forge a line where
// it prints, so such a text prints as the answer's own. A wrapper around it still prints nothing
// of its own. The trust is by declaration: redact cannot read a Printable's words, so each one is
// held to this promise where it is written.
type Printable interface {
	error
	// PrintableText is the error's text built from fixed words, its own words no answer or caller
	// writes, and its trusted answer's fields alone.
	PrintableText() string
}

// checked answers the text of err that may print and whether there is any: err's whole text where
// the allow-list passes it, and otherwise the text of the first error down err's chain
// (errors.Unwrap) that is itself a transport failure or the broker's trusted answer, its own
// text, or a Printable directly over a trusted answer whose PrintableText holds no control
// character, that text. That error's text is the one printed, not the text of err or of any
// error between the two, so the words of every wrapper above it drop, whoever wrote them: a
// caller's fmt.Errorf with %w or an error type of its own, and net/http's own too, so a peer that
// broke the connection mid-answer prints as `unexpected EOF` and not as `net/http: HTTP/1.x
// transport connection broken: unexpected EOF`. An error that wraps several (errors.Join) ends
// the walk, since its text is no single error's.
//
// The one way a caller's words still print is through what transport accepts by its type or its
// method rather than by its value: a *net.OpError or *net.DNSError a caller built, which prints
// its own fields and whatever error it wraps, and a caller's error type whose Timeout answers
// true, which prints its whole text. redact cannot tell those from net's own, and net/http's
// timeout types are unexported, so there is no list to hold them to. No caller builds either.
func checked(err error) (string, bool) {
	if text := err.Error(); clean(text) {
		return text, true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if p, ok := e.(Printable); ok && trustedAnswer(errors.Unwrap(e)) {
			if text := p.PrintableText(); !hasControl(text) {
				return text, true
			}
			continue
		}
		if transport(e) || trustedAnswer(e) {
			return e.Error(), true
		}
	}
	return "", false
}

// OneLine is text another party wrote - a broker's words, the code it answered - as one line of a
// message may print it: the text itself where it holds no control character and is valid UTF-8
// (hasControl), and otherwise the text Go-quoted (strconv.Quote), every such character escaped.
// That is the rule this package prints its own text under: a Printable whose text holds such a
// character prints as x/oauth2's text of its answer instead, which quotes the answer's fields the
// same way. Its answer holds no control character, so OneLine of it is that answer again.
func OneLine(text string) string {
	if hasControl(text) {
		return strconv.Quote(text)
	}
	return text
}

// hasControl reports whether text holds a control character (C0, DEL or C1) or is not valid
// UTF-8, either of which can start a line of its own or move the cursor where the text prints.
func hasControl(text string) bool {
	if !utf8.ValidString(text) {
		return true
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// transport reports whether err itself, not an error it wraps, is a failure class names from the
// transport, or a peer that broke the exchange (net/http's fixed ErrSchemeMismatch for an http
// answer to https, EOF, a reset, a broken pipe: the set the client's peerAnswered reads).
func transport(err error) bool {
	switch e := err.(type) {
	case *net.OpError, *net.DNSError:
		return true
	case interface{ Timeout() bool }:
		if e.Timeout() {
			return true
		}
	}
	switch err {
	case syscall.ECONNREFUSED, context.DeadlineExceeded, http.ErrSchemeMismatch, io.EOF, io.ErrUnexpectedEOF,
		syscall.ECONNRESET, syscall.EPIPE:
		return true
	}
	return tlsError(err)
}

// trustedAnswer reports whether err itself, not an error it wraps, is an OAuth error answer whose
// text is the broker's error code and error_description alone: an *oauth2.RetrieveError with a
// nonempty ErrorCode and an empty ErrorURI. printable asks only beside a clean base and a request
// URL Call prints, and only on a chain that holds no error this package already put in words
// (classified), so neither an answer an earlier Error or Cause withheld beside a URL that carries
// a credential nor one that Class or Masked holds, another server's, is ever one: there those two
// fields print whole, trusted rather than checked, since they are the broker's reason for a
// refusal and the only part a person can act on. Any other OAuth answer is not one, so it prints
// as its class alone ("the broker answered HTTP <n>"): an error_uri is a URL by RFC 6749, which
// may carry a credential or point anywhere, and for an answer with no error code x/oauth2's text
// is the response body it kept, up to 1 MiB with its line breaks, which can forge a log line.
func trustedAnswer(err error) bool {
	re, ok := err.(*oauth2.RetrieveError)
	return ok && re.ErrorCode != "" && re.ErrorURI == ""
}

// classified is an error whose text is its class alone: fixed text naming the kind of failure,
// which Error, Cause, Class and File make and Masked takes from its caller, or the text printable
// checked of the failure beneath a caller's wrapper (Error, Cause), and never a URL, a path, an
// id or other detail of a request. class() reads it as the failure's class, so text that named a
// call here would print again in every line made from the error; that text is a called.
type classified struct {
	text string
	err  error
}

func (c *classified) Error() string { return c.text }
func (c *classified) Unwrap() error { return c.err }

// The words class names a failure by, which Class prints and which a caller that classifies a
// failure redact cannot see into - one gRPC hands the client only as text - names the same failure
// by, so one failure has one set of words wherever it prints.
const (
	ClassNotAURL        = "not a valid URL"
	ClassNameResolution = "name resolution failed"
	ClassRefused        = "connection refused"
	ClassTimedOut       = "timed out"
	ClassTLS            = "TLS failed"
	ClassRequestFailed  = "the request failed"
)

// class names the kind of failure err is, in fixed text; server is who answered an HTTP status,
// "the broker" for Error and Cause and "the server" for Class. A failure Error, Cause, Class,
// Masked or File already put in words (a classified) keeps those words: they name the server that
// answered, which another server's answer under them would otherwise lend the broker's name. A
// call's line (a called) is not one: only the class beneath it is read.
func class(err error, server string) string {
	var ue *url.Error
	var dns *net.DNSError
	var re *oauth2.RetrieveError
	var worded *classified
	switch {
	case errors.As(err, &worded):
		return worded.text
	case errors.As(err, &ue) && ue.Op == "parse":
		return ClassNotAURL
	case errors.As(err, &dns):
		return ClassNameResolution
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassRefused
	case nettimeout.InChain(err):
		return ClassTimedOut
	case tlsFailure(err):
		return ClassTLS
	case errors.As(err, &re) && re.Response != nil:
		return fmt.Sprintf("%s answered HTTP %d", server, re.Response.StatusCode)
	default:
		return ClassRequestFailed
	}
}

// tlsFailure reports whether err, or an error it wraps, is a TLS alert, the far side's or this
// side's own (tlsalert.In, which reads the *net.OpError crypto/tls returns one as over TCP, and
// QUIC's tls.AlertError), or a TLS record or certificate failure (tlsRecordOrCertificate).
func tlsFailure(err error) bool {
	return tlsalert.In(err) || tlsRecordOrCertificate(err)
}

// tlsRecordOrCertificate reports whether err, or an error it wraps, is a TLS record or
// certificate failure (tlsError).
func tlsRecordOrCertificate(err error) bool {
	if err == nil {
		return false
	}
	if tlsError(err) {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return tlsRecordOrCertificate(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if tlsRecordOrCertificate(e) {
				return true
			}
		}
	}
	return false
}

// tlsError reports whether err itself, not an error it wraps, is a TLS record or certificate
// failure.
func tlsError(err error) bool {
	switch err.(type) {
	case tls.RecordHeaderError, *tls.CertificateVerificationError,
		x509.UnknownAuthorityError, x509.HostnameError, x509.CertificateInvalidError:
		return true
	}
	return false
}
