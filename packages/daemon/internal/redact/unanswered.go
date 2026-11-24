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
	"net/url"
	"strings"
	"syscall"
)

// Unanswered is err, the failure of an HTTP request that got no answer (http.Client.Do's error,
// or http.NewRequest's, with request nil), as the daemon prints it: net/http's operation, the
// request named shown, or where shown is "" by the URL it asked through the URL rule
// (URLCredentials), and the failure by its transport class (failure). It never prints net/http's
// own text of the failure, which can quote the answering party's words: Go parses a redirect's
// Location before a client's CheckRedirect is asked (net/http/client.go of go1.27.3, lines 655-665
// before 711-717), so a Location that does not parse comes back as `failed to parse Location
// header "<the Location as it came>"` whatever the client's redirect policy, and a malformed status
// line comes back quoted too. Whoever answers in the server's place can write the request's
// credential there. For a client that follows a redirect (one redirects.Refused did not make),
// net/http's error names the host the Location named, its URL and its transport failure alike, so
// a failure on another host than request's prints as the redirect's class alone.
//
// It unwraps to err, so errors.Is and errors.As still find the failure: a caller decides on it,
// and prints only this.
func Unanswered(err error, request *http.Request, shown string) error {
	if err == nil {
		return nil
	}
	var failed *url.Error
	if !errors.As(err, &failed) {
		return &unanswered{failure(err), err}
	}
	name := shown
	switch {
	case name != "":
	case request != nil:
		name = URLCredentials(request.URL.String())
	default:
		name = URLCredentials(failed.URL)
	}
	text := failure(failed.Err)
	switch {
	case failed.Op == "parse":
		text = classNotAURL
	case request != nil && redirectedElsewhere(failed.URL, request.URL):
		text = classRedirected
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

// The words Unanswered names a failure by where it prints none of net/http's.
const (
	classNotAURL          = "not a valid URL"
	classRedirected       = "the request was redirected to another host, which did not answer (the redirect's target is not printed: it is the answering party's own words)"
	classLocationUnparsed = "the answer was a redirect whose Location does not parse (the Location is not printed: it is the answering party's own words)"
	classRequestFailed    = "the request failed (net/http's words for the failure are not printed: they can quote what the answering party sent)"
)

// failure is the words Unanswered prints for err, the failure under a request that got no answer:
// the text of the first error down err's chain (errors.Unwrap) that is a transport failure whose
// text is the transport's own fixed words and the addresses the request dialled (transport), and
// otherwise fixed text naming its class. A Location that did not parse is told by net/http's own
// fixed words for it, a decision on them that prints none of them.
func failure(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if transport(e) {
			return e.Error()
		}
	}
	if err != nil && strings.HasPrefix(err.Error(), "failed to parse Location header ") {
		return classLocationUnparsed
	}
	return classRequestFailed
}

// transport reports whether err itself, not an error it wraps, is a failure of the transport whose
// text names no word of the answer: a dial or another net.OpError, name resolution, a timeout, a
// refused, reset or broken connection, an end of the stream, the request's own context ending, an
// http answer to https, or a TLS record or certificate failure. These are the classes envoy's
// internal/redact prints a transport failure's own text for (transport, tlsError there), which
// this module cannot import, and context.Canceled beside them: the caller's own cancellation.
func transport(err error) bool {
	switch e := err.(type) {
	case *net.OpError, *net.DNSError, tls.RecordHeaderError, *tls.CertificateVerificationError,
		x509.UnknownAuthorityError, x509.HostnameError, x509.CertificateInvalidError:
		return true
	case interface{ Timeout() bool }:
		if e.Timeout() {
			return true
		}
	}
	switch err {
	case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, context.DeadlineExceeded, context.Canceled,
		http.ErrSchemeMismatch, io.EOF, io.ErrUnexpectedEOF:
		return true
	}
	return false
}

// redirectedElsewhere reports whether failed, the URL net/http's error names, is on another host
// than asked, the request's own: a redirect the client followed, whose target is the answering
// party's words. A URL that does not parse is counted as elsewhere.
func redirectedElsewhere(failed string, asked *url.URL) bool {
	at, err := url.Parse(failed)
	return err != nil || !strings.EqualFold(at.Host, asked.Host)
}
