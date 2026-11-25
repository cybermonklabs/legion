package redact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Unanswered is err, the failure of an HTTP request that got no answer it could read
// (http.Client.Do's error, or the error of reading the answer's body), as a message may print it:
// net/http's operation and the request named shown, or where shown is "" by the URL it asked as
// askedURL names it, and then the failure by its transport text, a TLS record or certificate
// failure's fixed words, or its class (unansweredFailure). It is for a request whose URL Call does
// not print, such as a GitHub API path: Cause prints a transport failure's own text only beside a
// configured URL and a request URL under /v1/ of it, Class never prints it, and neither keeps a
// timeout's or a refused connection's own words for any other request. A module outside this one,
// the daemon among them, prints such a request through it as oauth2call.Unanswered. A caller
// passes shown only for words of its own that name the call in place of its URL, never for a name
// of the URL another rule made, so a URL has one name wherever it prints.
//
// It never prints net/http's own text of any other failure, which can quote the answering
// party's words: Go parses a redirect's Location before a client's CheckRedirect is asked
// (net/http/client.go of go1.27.3, lines 655-665 before 711-717), so a Location that does not
// parse comes back as `failed to parse Location header "<the Location as it came>"` whatever the
// client's redirect policy; a status line or a header line that does not parse comes back quoted
// too (response.go 174-188, textproto's reader.go 549-621), and so does a chunked body's trailer
// line (transfer.go 956). Whoever answers in the server's place can write the request's
// credential there. For a client that follows a redirect, net/http's error names the URL the
// Location named, and its transport failure the host it named, so a failure on another host than
// request's prints as the redirect's class alone. An error that holds no *url.Error, a body
// read's, prints as its failure alone, under the caller's own words for the call.
//
// It unwraps to err, so errors.Is and errors.As still find the failure: a caller decides on it,
// and prints only this. Unanswered(nil, request, shown) is nil.
func Unanswered(err error, request *http.Request, shown string) error {
	if err == nil {
		return nil
	}
	var failed *url.Error
	if !errors.As(err, &failed) {
		return &unanswered{unansweredFailure(err), err}
	}
	name := shown
	switch {
	case name != "":
	case request != nil:
		name = askedURL(request.URL)
	default:
		name = NotPrinted
		if asked, err := url.Parse(failed.URL); err == nil {
			name = askedURL(asked)
		}
	}
	text := unansweredFailure(failed.Err)
	switch {
	case failed.Op == "parse":
		text = ClassNotAURL
	case request != nil && redirectedElsewhere(failed.URL, request.URL):
		text = ClassRedirected
	}
	return &unanswered{fmt.Sprintf("%s %q: %s", failed.Op, name, text), err}
}

// unanswered is Unanswered's answer: its text, over the failure it names.
type unanswered struct {
	text string
	err  error
}

func (u *unanswered) Error() string { return u.text }
func (u *unanswered) Unwrap() error { return u.err }

// The words Unanswered names a failure by where it prints none of net/http's, beside the classes
// class names (ClassNotAURL, ClassRequestFailed).
const (
	ClassRedirected       = "the request was redirected to another host, which did not answer (the redirect's target is not printed: it is the answering party's own words)"
	ClassLocationUnparsed = "the answer was a redirect whose Location does not parse (the Location is not printed: it is the answering party's own words)"
)

// unansweredFailure is the words Unanswered prints for err, the failure under a request that got
// no answer: for the first error down err's chain (errors.Unwrap) that is a transport failure
// (transport: a dial or another net.OpError, name resolution, a timeout, a refused, reset or
// broken connection, an end of the stream, an http answer to https, a TLS record or certificate
// failure) or the caller's own cancellation (context.Canceled), its text as transportWords prints
// it: the transport's or context's own fixed words and the addresses the request dialled, or for
// a TLS record or certificate failure the fixed words for its type, since x509's text names what
// the certificate the answering party sent holds; and otherwise fixed text naming its class. A
// Location that did not parse is told by net/http's own fixed words for it, a decision on them
// that prints none of them.
func unansweredFailure(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if transport(e) || e == context.Canceled {
			return transportWords(e)
		}
	}
	if err != nil && strings.HasPrefix(err.Error(), "failed to parse Location header ") {
		return ClassLocationUnparsed
	}
	return ClassRequestFailed
}

// unreadable is err, the failure of a read of an answer's body (ReadAnswer, StreamAnswer's
// stream), as a message may print it: err itself where it is nil, a transport failure other than
// a TLS record or certificate failure, or the caller's cancellation (io.EOF, io.ErrUnexpectedEOF,
// a timeout, a reset connection), whose text is the transport's own words, so a reader that
// compares it with io.EOF still finds it as it came; and otherwise Unanswered of it, its class or
// a TLS failure's fixed words, unwrapping to err. net/http's own text of a body it could not read
// can quote a line the answer wrote: a chunked body's trailer line that does not parse comes back
// quoted (net/http/transfer.go of go1.27.3, line 956, then textproto's reader.go 549-621), and
// whoever answers in the server's place can write the request's credential there.
func unreadable(err error) error {
	if err == nil || err == context.Canceled || transport(err) && !tlsRecordOrCertificate(err) {
		return err
	}
	return Unanswered(err, nil, "")
}

// redirectedElsewhere reports whether failed, the URL net/http's error names, is on another host
// than asked, the request's own: a redirect the client followed, whose target is the answering
// party's words. A URL that does not parse is counted as elsewhere.
func redirectedElsewhere(failed string, asked *url.URL) bool {
	at, err := url.Parse(failed)
	return err != nil || !strings.EqualFold(at.Host, asked.Host)
}

// AskedURL is u, the URL a request asked, as Unanswered names it (askedURL): the name a line about
// a request that got an answer gives the URL too, so the URL has one name wherever it prints,
// whether the request got an answer or not.
func AskedURL(u *url.URL) string { return askedURL(u) }

// askedURL is u, the URL a request asked, as Unanswered names it, daemon and envoy alike: scheme
// "://" host path, with u's user information, its query and its fragment removed, where URL prints
// that unchanged, and NotPrinted where URL refuses it (an escape, a path or a host it does not
// allow) or u is nil or opaque. The host is the one net/url parsed, the host the request dialled,
// which a failed dial's own words name too: where a password holds an '@' and no host follows it
// (URL's https://user:p@ssword), the tail net/url read as the host prints, as it does there. The
// query goes whole, its keys with its values: a client can write a credential as a key alone
// (?<token>), and neither URL's allow-list, which a token's letters, digits and '-' pass, nor the
// credentials the request carried, whose query values requestTokens reads and never its keys, can
// tell such a key from a parameter's name.
func askedURL(u *url.URL) string {
	if u == nil || u.Opaque != "" {
		return NotPrinted
	}
	return URL(u.Scheme + "://" + u.Host + u.EscapedPath())
}
