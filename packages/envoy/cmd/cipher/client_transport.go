// packages/envoy/cmd/cipher/client_transport.go
//
// The client's failures that are not the broker's answer (client.go): a call that did not
// complete (unreachableError) and an answer cut off after its headers (cutOffError), how each is
// classified, which of them a later call may cure (retryable), and how each prints its call's URL
// - through the broker's Address, with an id the person typed left out of the route
// (withoutTypedID) and a query this client built cut off (withoutQuery) - so no credential the
// address carries and no argument pasted in place of an id reaches the message.
package main

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

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/httpoutage"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/tlsalert"
)

// errNothingSent is the cause of a call this client could not even build a request for.
var errNothingSent = errors.New("nothing was sent")

// unreachableError is a broker call that did not complete. Its host might not resolve, nothing
// might answer at the address, or a peer might answer the connection but fail the exchange. It is
// not a broker refusal - the broker sent no usable response - and it reads as what it is, naming
// the address tried and the variable the address came from, because a session that inherited a
// stale AGENT_SECRETS_URL otherwise reads a bare DNS error as a broker that is down and the stale
// value goes unnoticed.
type unreachableError struct {
	// what classifies the failure: a failed name resolution, no connection made, nothing answered
	// in time, a peer that answered but failed the exchange, or a request that failed in a way
	// none of those names.
	what string
	// base is the broker's address as AGENT_SECRETS_URL gave it, method and url the call tried;
	// Error prints base as the Address prints and url through callWords, so a credential the
	// variable carries never prints, while an id the call path-escaped still does, unless route
	// names the call with the id a person typed left out (withoutTypedID).
	base        cipheraddr.Address
	method, url string
	// cause is the transport's own error through base.Cause: without net/http's *url.Error
	// wrapper, which repeats the URL, or its class alone where its text may not print.
	cause error
	// route is the call's path as a message prints it where that path holds an id the person
	// typed, with the id left out; "" for every other call (withoutTypedID).
	route string
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("%s: %s %s (%s names %s): %v", e.what, e.method, callWords(e.base, e.url, e.route), brokerURLVar, e.base, e.cause)
}

func (e *unreachableError) Unwrap() error { return e.cause }

// callWords is a call's URL as a message prints it: callURL through base.Call, or, where route is
// set because the call's path held an id the person typed, route at the broker's address with the
// id left out (cipheraddr.Address.Route). The broker never accepted that id, and an argument that
// is no id is where a credential pasted in the wrong place lands.
func callWords(base cipheraddr.Address, callURL, route string) string {
	if route == "" {
		return base.Call(callURL)
	}
	return base.Route(route)
}

// withoutTypedID is err, the failure of a call whose path holds an id the person typed, with that
// id left out of the URL it prints: route is the call's path with the id spelled as
// `(the machine id, not printed)`. Only a call that did not complete prints its URL
// (unreachableError, cutOffError, the routed failures); a broker's refusal prints none, and an
// error whose chain holds no routed failure is err. The routed failure is found wherever err's
// chain holds it (errors.As). Where err is that failure, the result is of err's own type with the
// route set, so errors.Is and errors.As still classify it, and retryable still reads it. Where a
// layer wraps it, that layer's text already holds the URL, so the result fails closed: it prints
// the failure's own words with the route set, in place of every layer's, and still unwraps to err.
func withoutTypedID(err error, route string) error {
	var failed routed
	if !errors.As(err, &failed) {
		return err
	}
	typed := failed.withRoute(route)
	if error(failed) == err {
		return typed
	}
	return redact.Masked(err, typed.Error())
}

// routed is a failure that prints its call's URL, which withRoute answers as a copy whose route,
// the call's path with the id a person typed left out, is route.
type routed interface {
	error
	withRoute(route string) error
}

func (e *unreachableError) withRoute(route string) error {
	typed := *e
	typed.route = route
	return &typed
}

func (e *cutOffError) withRoute(route string) error {
	typed := *e
	typed.route = route
	return &typed
}

// unreachable classifies a failed HTTP exchange, sending its reader to the layer that failed.
// Any resolver error is name resolution, before the generic timeout branch. A dial that made no
// connection (refused, network or host unreachable, any other dial error) is a non-answer, which
// routing or a listener fixes. Only what proves a peer answered - a TLS record, alert or certificate
// error, EOF, or a reset once the connection existed - is "answered but the exchange failed",
// which TLS or a proxy fixes. An error that fits none says no more than that the request failed,
// rather than name a layer it cannot prove.
func unreachable(base cipheraddr.Address, method, callURL string, err error) error {
	cause := base.Cause(err)
	host := brokerHost(base)
	failed := func(what string) error { return &unreachableError{what, base, method, callURL, cause, ""} }
	var dns *net.DNSError
	var timeout net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return failed("the broker's host " + host + " does not resolve")
	case errors.As(err, &dns):
		return failed("the broker's host " + host + " name resolution failed")
	case errors.Is(err, syscall.ECONNREFUSED):
		return failed("nothing answered at " + host + ": the connection was refused")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return failed("the broker at " + host + " did not answer in time")
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH),
		errors.As(err, &op) && op.Op == "dial":
		return failed("nothing answered at " + host + ": no connection could be made")
	case peerAnswered(err):
		return failed("the broker at " + host + " answered but the exchange failed")
	default:
		return failed("the request to the broker at " + host + " failed")
	}
}

// peerAnswered reports whether err shows a peer took the connection and then broke the exchange:
// a TLS record that is not TLS (an HTTP server answering an https URL, which net/http reports as
// its fixed http.ErrSchemeMismatch in place of the record error), a TLS alert, the broker's or this
// client's own (tlsalert.In, which reads the *net.OpError crypto/tls returns one as over TCP), a
// certificate it would not trust, or the stream ending or being reset once it existed.
func peerAnswered(err error) bool {
	var record tls.RecordHeaderError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &record) || errors.Is(err, http.ErrSchemeMismatch) || tlsalert.In(err) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// cutOffError is a broker answer lost after its headers came: the status line arrived and the
// body did not, because the stream broke while doStatus read it. Its text is the method and the
// call's URL as unreachableError prints it (callWords), then "read response:", then the
// transport's own cause.
type cutOffError struct {
	base        cipheraddr.Address
	method, url string
	cause       error
	// route is the call's path with the id the person typed left out, as unreachableError's is.
	route string
}

func (e *cutOffError) Error() string {
	return e.method + " " + callWords(e.base, e.url, e.route) + ": read response: " + e.cause.Error()
}

func (e *cutOffError) Unwrap() error { return e.cause }

// retryable reports whether a later call may get the answer err's call did not: the call went
// out and failed in transport (unreachable's classes), its answer was cut off after the headers
// came (*cutOffError), or the broker, or a front door before it, could not serve it just then —
// an outage (httpoutage.Is: any 5xx, a 408 or a 429), with the broker's envelope or without
// it (doStatus makes an *apiError of either). Any other refusal is the broker's answer, which the
// same call gets again, and a failure of the local signer (the helper's socket) is not the
// broker's at all.
func retryable(err error) bool {
	var unreached *unreachableError
	if errors.As(err, &unreached) {
		return !errors.Is(err, errNothingSent)
	}
	var refusal *apiError
	if errors.As(err, &refusal) {
		return httpoutage.Is(refusal.Status)
	}
	var cut *cutOffError
	return errors.As(err, &cut)
}

// withoutQuery is err with the URL of the *url.Error net/http wraps a failed call in cut at query,
// the query send's call carried (the address's own, then the route's), so the redactor reads the
// URL this client joined from the broker's address and a route. A URL that does not end in that
// query, a redirect's target say, stays as net/http named it.
func withoutQuery(err error, query string) error {
	var ue *url.Error
	if query != "" && errors.As(err, &ue) {
		ue.URL = strings.TrimSuffix(ue.URL, "?"+query)
	}
	return err
}
