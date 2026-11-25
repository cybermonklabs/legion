// Package dispatchread is the broker's one GET of a Dispatch access route, the read both of the
// broker's clients of the Dispatch at BROKER_PROJECT_ACCESS_URL make: internal/cipher/projectaccess
// (whether a person may see a project, and their member kind) and internal/cipher/projectrepos
// (the repositories a project's issues resolve to). It holds what the two must not do two ways:
// the read of the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), the bound on one
// call, the headers, the bounded read of the body, which failures are that token's and not
// Dispatch's (ErrBrokerRefused), and which are Dispatch giving no answer at all (ErrNoAnswer). It
// decodes no answer: each client reads its own route's body, so no field of one answer reaches a
// reader of the other. The body is read through redact.ReadAnswer, so whatever answers in
// Dispatch's place, which can echo the broker's account token, is read with the record of the call
// it answers: a code, a key or a value an error names prints as the rule reads it.
package dispatchread

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// ErrBrokerRefused is the broker's own account token not working: Dispatch answering 401 or 403 to
// it (Dispatch does not list the broker's account as one that may ask, or does not accept the
// token, one minted for another audience, say), or the broker unable to read it from
// BROKER_PROJECT_ACCESS_TOKEN_FILE at all (missing, empty or unreadable). Either is a
// misconfiguration and not an outage, so no caller mints, enrolls or answers through it as it would
// through a failure to reach Dispatch: every read made with that credential would be wrong.
var ErrBrokerRefused = errors.New("dispatchread: the broker's own account token was refused or could not be read")

// ErrNoAnswer is a call Dispatch gave no whole answer to: the transport failed before its status
// arrived or while its body was read (a refused or reset connection, a name server that did not
// answer in time, an answer cut short), or the call's timeout, or its caller's deadline, passed
// first. It is Dispatch, or the way to it, not answering, which clears by itself once it answers
// again, unlike ErrBrokerRefused and unlike an answer that is wrong; so a caller that lets
// something through only while Dispatch does not answer goes ahead on it
// (projectaccess.Unanswered). A certificate that does not verify and a name that does not resolve
// are no such failure (transportAnswers): each lasts until somebody fixes the deployment.
var ErrNoAnswer = errors.New("dispatchread: dispatch gave no whole answer")

// Reader reads Dispatch's routes at one base URL, presenting the broker's own account token, each
// call bounded by its timeout and each body by its limit.
type Reader struct {
	base    string
	account ciphertoken.Account
	http    *http.Client
	timeout time.Duration
	limit   int
}

// New is a reader of the Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL), presenting account
// (BROKER_PROJECT_ACCESS_TOKEN_FILE's reader) on every call. A nil httpClient is
// http.DefaultClient. Either is used as a copy that follows no redirect (redirects.Refused): every
// call carries the broker's account token, and Dispatch's access routes answer no redirect, so a
// 3xx is Dispatch's answer, which Get hands back by its status as it does any other. Each call,
// the dial and the body's read included, is bounded by timeout whatever the client, and a body
// longer than limit bytes is an error.
func New(baseURL string, account ciphertoken.Account, httpClient *http.Client, timeout time.Duration, limit int) Reader {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return Reader{base: baseURL, account: account, http: redirects.Refused(httpClient), timeout: timeout, limit: limit}
}

// Answer is what Dispatch answered one call: its status, and its whole body, held with the record
// of the call (redact.Answer): a caller decodes it (Answer.Decode), or reads its bytes to decide
// (StreamTo) and names what it read only through the body's Said.
type Answer struct {
	Status int
	Body   redact.Answer
}

// Get asks path, with query when it holds any, and answers Dispatch's status and body for the
// caller to read, whatever the status, but for these errors, each naming what failed and never the
// token. An account token it cannot read is ErrBrokerRefused naming
// BROKER_PROJECT_ACCESS_TOKEN_FILE, and Dispatch is asked nothing. A 401 or a 403 is
// ErrBrokerRefused too, Dispatch refusing that token, decided by the status alone before the body
// is read: a body cut short or past the limit only leaves the error without the code it names, and
// never turns the refusal into a failure to ask. A transport failure, the timeout passing, and a
// body it could not read are ErrNoAnswer, but for a certificate that does not verify and a name
// that does not resolve (transportAnswers), each an error of its own; a body past the limit is an
// error of its own too, an answer too long rather than none.
//
// path, with query after whatever BROKER_PROJECT_ACCESS_URL carries, is joined onto that URL's
// path (urljoin.Join), its query kept and its fragment, which no request sends, dropped: path
// appended to the URL's text would sit inside a query or a fragment and Dispatch would be asked
// another path. A BROKER_PROJECT_ACCESS_URL that does not parse or names no host, and a joined URL
// that does not parse again (url.URL.String can write text url.Parse refuses, such as an IPv6 zone
// escape), are refused in this package's own words: url.Parse's error, which http.NewRequest's
// carries, quotes the URL whole, user information and query included.
func (r Reader) Get(ctx context.Context, path string, query url.Values) (Answer, error) {
	bearer, err := r.account.Read()
	if err != nil {
		return Answer{}, fmt.Errorf("%w: BROKER_PROJECT_ACCESS_TOKEN_FILE could not be read: %w", ErrBrokerRefused, err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ref := path
	if len(query) > 0 {
		ref += "?" + query.Encode()
	}
	target, err := urljoin.Join(r.base, ref)
	if err != nil {
		return Answer{}, errors.New("dispatchread: BROKER_PROJECT_ACCESS_URL is not an absolute URL with a host")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Answer{}, errors.New("dispatchread: the URL joined from BROKER_PROJECT_ACCESS_URL does not parse")
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	response, err := r.http.Do(request)
	if err != nil {
		if transportAnswers(err) {
			return Answer{}, fmt.Errorf("dispatchread: ask dispatch %s: %w", path, redact.Class(err))
		}
		return Answer{}, fmt.Errorf("%w to %s: %w", ErrNoAnswer, path, redact.Class(err))
	}
	defer response.Body.Close()
	answer, err := redact.ReadAnswer(response, int64(r.limit))
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return Answer{}, fmt.Errorf("%w: dispatch answered %s %d with the code %q", ErrBrokerRefused, path, response.StatusCode, Quoted(RefusalCode(answer)))
	}
	if errors.Is(err, redact.ErrAnswerTooLarge) {
		return Answer{}, fmt.Errorf("dispatchread: dispatch's answer to %s is over %d bytes", path, r.limit)
	}
	if err != nil {
		return Answer{}, fmt.Errorf("%w to %s: read its body: %w", ErrNoAnswer, path, err)
	}
	return Answer{Status: response.StatusCode, Body: answer}, nil
}

// transportAnswers reports whether err, a call that failed before Dispatch's status arrived, is
// the way to Dispatch answering rather than not answering: a certificate that does not verify
// (tls.CertificateVerificationError, or an x509 error the handshake returned as it is), or a name
// the resolver answered does not resolve (a net.DNSError that is not found, or neither a timeout
// nor temporary). Each lasts until somebody fixes the deployment's address or certificate, unlike
// a name server that did not answer in time, which clears by itself.
func transportAnswers(err error) bool {
	var verify *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verify) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return true
	}
	var name *net.DNSError
	return errors.As(err, &name) && (name.IsNotFound || !name.IsTimeout && !name.IsTemporary)
}

// RefusalCode is the code a refusal's body names, as Dispatch wrote it (redact.RemoteText, decoded
// with the record of the call, so it prints as the rule reads it and compares as it came), and
// empty for a body that is cut short, holds more than one value or names none: the status has
// already decided what the answer is, and the code only adds to its error.
func RefusalCode(answer redact.Answer) redact.RemoteText {
	var refusal struct {
		Code redact.RemoteText `json:"code"`
	}
	if answer.Decode(&refusal) != nil {
		return redact.NewRemoteText("")
	}
	return refusal.Code
}

// MaxQuoted bounds how much of a string from Dispatch's answer an error quotes, so that a code, a
// key or a value as long as the answer itself is never logged whole.
const MaxQuoted = 64

// Quoted is what an error of the broker's quotes of a string Dispatch's answer named — a refusal's
// code (RefusalCode), or a key or a value of a body the caller reads itself (redact.Answer.Said):
// the rule's reading of it, cut to MaxQuoted bytes. Every caller of this package names such a
// string through this one function, so that no site of its own decides the bound or the reading.
func Quoted(text redact.RemoteText) string {
	shown := text.String()
	if len(shown) > MaxQuoted {
		return shown[:MaxQuoted]
	}
	return shown
}
