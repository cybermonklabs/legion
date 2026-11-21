package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/serverwait"
	"github.com/sjawhar/envoy/internal/tlsalert"
)

// mintResponseLimit caps what a minter reads of an upstream answer. GitHub's installation pages
// and an owner-wide token answer and Google's token answer are a few KiB; 1 MiB is the limit
// Dispatch's own GitHub client reads them with (internal/dispatch/githubapp, responseLimit). A
// narrowed token's answer names every repository it reaches and is read to its own, larger cap
// (accessTokensResponseLimit).
const mintResponseLimit = 1 << 20

// mintStatusError is an upstream answer whose status is not the one the call wanted. It wraps
// ErrMintRefused for a refusal (refused) and ErrMintUpstream for anything else, a 408, a 5xx and a
// rate-limited 4xx among them, from every minter call (GitHub's installation list, token requests
// narrowed or not, and revokes, Google's refresh and Google's service-account token): a 408 is the
// upstream timing the request out and a rate limit lifts by itself, so neither is a verdict on the
// App, the login or a narrowing, nor the App owner's to fix. It names the numeric status alone: an
// upstream's reason phrase is its own text, which a malformed or hostile answer can fill with what
// the request sent.
type mintStatusError struct {
	what, who string
	code      int
	// rateLimited is an answer that says it is a rate limit (rateLimited).
	rateLimited bool
	// wait is the wait the answer asks for before the next attempt, when waitNamed (serverWait).
	wait      time.Duration
	waitNamed bool
}

func (e *mintStatusError) Error() string {
	if e.rateLimited {
		return fmt.Sprintf("%s: %s answered HTTP %d, a rate limit", e.what, e.who, e.code)
	}
	return fmt.Sprintf("%s: %s answered HTTP %d", e.what, e.who, e.code)
}

func (e *mintStatusError) Unwrap() error {
	if e.refused() {
		return ErrMintRefused
	}
	return ErrMintUpstream
}

// refused reports whether the answer is the upstream refusing the request: a status
// oauthrefusal.Refuses (any 4xx but 408 and 429, the rule the module reads a token endpoint's
// refusal by) that is no rate limit, which GitHub can also answer with a 403 (rateLimited).
func (e *mintStatusError) refused() bool {
	return oauthrefusal.Refuses(e.code) && !e.rateLimited
}

// mintTransportError is an attempt that got no answer it could read: the call failed, or the
// read of its body did. It is named by its class alone (TransportFailure) and wraps
// ErrMintUpstream.
type mintTransportError struct {
	what, who, class string
	// read is a failure reading the body of an answer that had begun.
	read bool
	// sent is whether the request may have reached the upstream, and it is true for every failure
	// but one known to come before the transport had a connection to the upstream (mintAttempt):
	// one the standard transport's trace shows came before a connection, such as a proxy's refused
	// tunnel, or one whose error says so - a name that did not resolve, a dial that failed, a
	// certificate the broker did not trust (beforeConnection). Either did nothing upstream. Any
	// other failure may come after some or all of the request went out and was served, whatever
	// the write reported. A read that failed is always sent.
	sent bool
	// status is the status that answer came with, for a read that failed; 0 where no answer
	// arrived (answeredStatus).
	status int
}

func (e *mintTransportError) Error() string {
	if e.read {
		return fmt.Sprintf("%s: read %s: %s %s", ErrMintUpstream, e.what, e.who, e.class)
	}
	return fmt.Sprintf("%s: %s: %s %s", ErrMintUpstream, e.what, e.who, e.class)
}

func (e *mintTransportError) Unwrap() error { return ErrMintUpstream }

// mintOversizeError is an answer past the call's cap, with the status it came with
// (answeredStatus). It wraps ErrMintUpstream and is never retried (classify).
type mintOversizeError struct {
	what, who string
	limit     int64
	status    int
}

func (e *mintOversizeError) Error() string {
	return fmt.Sprintf("%s: %s: %s answered more than %d bytes", ErrMintUpstream, e.what, e.who, e.limit)
}

func (e *mintOversizeError) Unwrap() error { return ErrMintUpstream }

// answeredStatus is the status of an answer whose body the call could not use whole: one whose
// read failed part of the way, or one past the call's cap. It is 0 for every other error, and for
// a call that got no answer at all. A call whose wanted status means the upstream made something,
// as GitHub's 201 to a token request means a live token, reads it to learn that the upstream made
// it although the answer did not hand it over whole (GitHubApp.installationToken).
func answeredStatus(err error) int {
	var transport *mintTransportError
	var oversize *mintOversizeError
	switch {
	case errors.As(err, &transport):
		return transport.status
	case errors.As(err, &oversize):
		return oversize.status
	}
	return 0
}

// rateLimited reports whether response, whose body is body, says it is a rate limit, without
// quoting either: a 429; GitHub's X-RateLimit-Remaining of 0, which its primary rate limit's 403
// or 429 carries; or a body naming a rate limit, as GitHub's secondary rate limit does ("rate limit")
// and Google's usage limits do (the reasons rateLimitExceeded and userRateLimitExceeded,
// RATE_LIMIT_EXCEEDED). Google sends no X-RateLimit-Remaining, so only the status and the body
// reach a Google answer.
func rateLimited(response *http.Response, body []byte) bool {
	if response.StatusCode == http.StatusTooManyRequests || response.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("rate limit")) || bytes.Contains(lower, []byte("ratelimit")) || bytes.Contains(lower, []byte("rate_limit"))
}

// serverWait is the wait an answer names before the next request, read against now through
// serverwait.Read, and whether it names one; else, for a rate limit, rateLimitedWait, the one
// minute GitHub asks for when it names none; and for any other answer no wait at all. A wait of an
// hour is far past mintWaitBudget, so it ends the call (mintRetry.again) as any wait past the
// budget does.
func serverWait(header http.Header, rateLimited bool, now time.Time) (time.Duration, bool) {
	if wait, named := serverwait.Read(header, now); named {
		return wait, true
	}
	if rateLimited {
		return rateLimitedWait, true
	}
	return 0, false
}

// mintCall sends request with client (ownClient, the minters' own, when nil) and reads at most
// limit bytes of its answer, attempting it again as mint's Retry allows: classify decides which
// failures are retried, and mintRetry.again how long the call waits first and whether it may.
// Each attempt is counted, and resends the request's body from GetBody; a request whose body
// cannot be read again gets one attempt. resendLost is whether an attempt that may have reached
// the upstream and failed (a *mintTransportError that was sent) is sent again: the upstream may
// have received it whole and served it, so a call that mints something nobody will use, as a
// check's token request does (GitHubApp.installationToken), does not have the upstream make it
// again. An attempt known to have failed before a connection (mintAttempt) did nothing upstream,
// and is sent again as any failed call is. what names the call and who the upstream in its
// errors. No error quotes the request, the answer or the transport's own text: Go's HTTP client
// quotes a malformed status line, and a status line or body can echo the credential the request
// carried, so a failed call is named by its class alone (TransportFailure). A status other than
// want is a *mintStatusError, returned with the body, so a caller can read the upstream's own
// reason from it; an answer whose body was read in part, or past limit, is returned with as much
// of it as was read; every other failure wraps ErrMintUpstream. answered, where it is not nil,
// is called with each attempt's answer that came with want but could not be read whole, cut off
// part of the way or past limit, and that attempt's error, before the call is sent again or
// answers: the upstream did what was asked, and a call that makes something, as a token request
// does, gives back what that answer made, which the next attempt's answer would otherwise leave
// behind (GitHubApp.giveBackHook). The error of a failed call is its last attempt's, and carries
// the number of attempts made (MintAttempts).
func mintCall(mint *mintRetry, client *http.Client, request *http.Request, what, who string, want int, limit int64, resendLost bool, answered func([]byte, error)) ([]byte, error) {
	if client == nil {
		client = ownClient
	}
	ctx := request.Context()
	resendable := request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
	for attempt := 1; ; attempt++ {
		body, err := mintAttempt(client, request, attempt, mint.retry.now, what, who, want, limit)
		outcome, retry := classify(ctx, err)
		mint.count(outcome, attempt)
		if err == nil {
			return body, nil
		}
		if answered != nil && answeredStatus(err) == want {
			answered(body, err)
		}
		var transport *mintTransportError
		served := errors.As(err, &transport) && transport.sent
		if !retry || !resendable || served && !resendLost || !mint.again(ctx, attempt, err) {
			return body, &attemptsError{err: err, attempts: attempt}
		}
	}
}

// mintAttempt is one attempt of a mintCall, under mintAttemptTimeout within the request's own
// context. A later attempt reads its body from the request's GetBody. A status other than want
// carries the wait the answer names, read against now (serverWait). A call that failed says
// whether its request may have reached the upstream (mintTransportError.sent): it did not where
// the error says so (beforeConnection), or, for a client whose transport this package owns
// (tracesConnections), where that transport reported to the attempt's trace that it went to get a
// connection (GetConn) and never reported one (GotConn). net/http's own path reports both, and
// GotConn before it writes a byte, on HTTP/1 and HTTP/2, so a call it failed before then - a proxy
// that refused the tunnel, a handshake that failed - sent nothing, whatever the error's type. Any
// other transport, of whatever type, owes the trace nothing, and one that forwards some hooks and
// not others would make that reading wrong, so such a call is read by its error alone. The trace's
// report of the write is not read: it can call a request the upstream received whole failed, or
// come after Do has returned.
func mintAttempt(client *http.Client, request *http.Request, attempt int, now func() time.Time, what, who string, want int, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(request.Context(), mintAttemptTimeout)
	defer cancel()
	var askedConnection, gotConnection atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { askedConnection.Store(true) },
		GotConn: func(httptrace.GotConnInfo) { gotConnection.Store(true) },
	})
	sent := request.Clone(ctx)
	if attempt > 1 && request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, fmt.Errorf("%w: %s: its request could not be sent again", ErrMintUpstream, what)
		}
		sent.Body = body
	}
	response, err := client.Do(sent)
	if err != nil {
		noConnection := tracesConnections(client) && askedConnection.Load() && !gotConnection.Load()
		return nil, &mintTransportError{what: what, who: who, class: TransportFailure(request.Context(), err), sent: !noConnection && !beforeConnection(err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return body, &mintTransportError{what: what, who: who, class: TransportFailure(request.Context(), err), read: true, sent: true, status: response.StatusCode}
	}
	if int64(len(body)) > limit {
		return body[:limit], &mintOversizeError{what: what, who: who, limit: limit, status: response.StatusCode}
	}
	if response.StatusCode != want {
		limited := rateLimited(response, body)
		wait, named := serverWait(response.Header, limited, now())
		return body, &mintStatusError{what: what, who: who, code: response.StatusCode, rateLimited: limited, wait: wait, waitNamed: named}
	}
	return body, nil
}

// ownTransport is the transport a minter sends through when it names no HTTP client, built by
// this package alone and held here (newOwnTransport), so no code outside it can register a
// protocol on it (http.Transport.RegisterProtocol), give it a callback for a negotiated protocol
// (TLSNextProto) or wrap what it does. Its trace is net/http's own: GetConn before it asks for a
// connection and GotConn before it writes a byte, on HTTP/1 and on the HTTP/2 net/http sets up
// itself, a connection it already holds included.
var ownTransport = newOwnTransport()

// ownClient sends through ownTransport.
var ownClient = &http.Client{Transport: ownTransport}

// newOwnTransport builds a transport with the settings net/http documents for
// http.DefaultTransport, rather than a clone of it: a clone carries whatever other code set on
// http.DefaultTransport before this package started, TLSNextProto callbacks among them, and
// those can send a request in a way no trace reports.
func newOwnTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// tracedTransports is every transport whose trace mintAttempt reads (tracesConnections):
// ownTransport, and a transport a test made itself and registers no protocol on
// (TraceTransportForTest in export_test.go).
var tracedTransports sync.Map

func init() { tracedTransports.Store(ownTransport, struct{}{}) }

// tracesConnections reports whether client sends through a transport whose whole path this
// package owns (tracedTransports), so the request's trace reports each connection it gets. The
// concrete type of a transport says nothing of that: an *http.Transport hands a request to any
// protocol registered on it before its own path runs, and a wrapper owes the trace none of its
// hooks.
func tracesConnections(client *http.Client) bool {
	transport, isStandard := client.Transport.(*http.Transport)
	if !isStandard {
		return false
	}
	_, traced := tracedTransports.Load(transport)
	return traced
}

// classify is the one rule for what an attempt's failure was, as broker_mint_attempts_total
// names it, and whether the call is made again. A 5xx, a 408, a rate limit and a call or body read
// that failed while the caller's context lives are retried; a refusal (mintStatusError.refused), a
// call the caller's context ended (cancelled or past its deadline), an answer past its cap and a
// 2xx or 3xx the call did not want are not.
func classify(ctx context.Context, err error) (outcome string, retry bool) {
	var status *mintStatusError
	var transport *mintTransportError
	switch {
	case err == nil:
		return attemptOK, false
	case errors.As(err, &status):
		switch {
		case status.rateLimited:
			return attemptRateLimited, true
		case status.code >= 500 || status.code == http.StatusRequestTimeout:
			return attemptUpstream, true
		case status.refused():
			return attemptRefused, false
		}
		return attemptUpstream, false
	case errors.As(err, &transport):
		if ctx.Err() != nil {
			return attemptCancelled, false
		}
		return attemptUpstream, true
	}
	return attemptUpstream, false
}

// beforeConnection reports whether err, a failed call, is one the transport met before it had a
// connection to the upstream, so no byte of the request left: a name that did not resolve, a
// dial that failed or ran out of time, or a certificate the broker did not trust, which only the
// TLS handshake checks. A TLS record error or alert can come after the handshake, once the
// request went out, so it does not count, and neither does any other failure that may have come
// after the request went out, or one this cannot place: each reads false.
func beforeConnection(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	var certificate *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dns), errors.As(err, &certificate):
		return true
	case errors.As(err, &op):
		return op.Op == "dial"
	}
	return false
}

// TransportFailure names the class of a failed call or body read, never its text: the request was
// cancelled or ran out of time, the upstream's name did not resolve, its TLS handshake failed, it
// answered with a TLS alert or met one this side sent (tlsalert.In, which reads the *net.OpError
// crypto/tls returns one as over TCP: its Op is no dial's), it could not be reached, or its answer
// ended early. Any other failure, such as a status line that is not HTTP, "gave no usable HTTP
// answer". The minters name their failed calls by it, and so does the group reader
// (groups.Reader), whose calls carry the reader account's token.
func TransportFailure(ctx context.Context, err error) string {
	var dns *net.DNSError
	var op *net.OpError
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return "was cancelled before it answered"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		return "did not answer in time"
	case errors.As(err, &dns):
		return "could not be resolved"
	case errors.As(err, &certificate) || errors.As(err, &record):
		return "failed its TLS handshake"
	case tlsalert.In(err):
		return "answered, but the TLS exchange failed"
	case errors.As(err, &op):
		return "could not be reached"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "ended its answer early"
	}
	return "gave no usable HTTP answer"
}
