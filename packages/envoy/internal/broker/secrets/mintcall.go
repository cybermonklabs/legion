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
	"strconv"
	"time"
)

// mintResponseLimit caps what a minter reads of an upstream answer. GitHub's installation pages
// and an owner-wide token answer and Google's token answer are a few KiB; 1 MiB is the limit
// Dispatch's own GitHub client reads them with (internal/dispatch/githubapp, responseLimit). A
// narrowed token's answer names every repository it reaches and is read to its own, larger cap
// (accessTokensResponseLimit).
const mintResponseLimit = 1 << 20

// mintStatusError is an upstream answer whose status is not the one the call wanted. It wraps
// ErrMintRefused for a 4xx and ErrMintUpstream for anything else, a rate-limited 4xx among them,
// from every minter call (GitHub's installation list, token requests narrowed or not, and revokes,
// and Google's refresh): a rate limit is no verdict on the App, the login or a narrowing, and
// lifts by itself, so it is not the App owner's to fix. It names the numeric status alone: an
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
	if e.code >= 400 && e.code < 500 && !e.rateLimited {
		return ErrMintRefused
	}
	return ErrMintUpstream
}

// mintTransportError is an attempt that got no answer it could read: the call failed, or the
// read of its body did. It is named by its class alone (transportFailure) and wraps
// ErrMintUpstream.
type mintTransportError struct {
	what, who, class string
	// read is a failure reading the body of an answer that had begun.
	read bool
}

func (e *mintTransportError) Error() string {
	if e.read {
		return fmt.Sprintf("%s: read %s: %s %s", ErrMintUpstream, e.what, e.who, e.class)
	}
	return fmt.Sprintf("%s: %s: %s %s", ErrMintUpstream, e.what, e.who, e.class)
}

func (e *mintTransportError) Unwrap() error { return ErrMintUpstream }

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

// serverWait is the wait an answer names before the next request, read against now, and whether
// it names one: Retry-After's seconds or HTTP date, else the time until X-RateLimit-Reset's epoch
// seconds when X-RateLimit-Remaining is 0, else, for a rate limit, rateLimitedWait, the one minute
// GitHub asks for when it gives neither, and for any other answer no wait at all. A wait already
// past is no wait rather than a negative one. It reads the two headers as
// internal/dispatch/githubapp's rateLimitWait does, on a clock the caller sets, and adds
// Retry-After's HTTP date, a floor of zero on its seconds, and no wait for an answer that is no
// rate limit.
func serverWait(header http.Header, rateLimited bool, now time.Time) (time.Duration, bool) {
	retryAfter := header.Get("Retry-After")
	if seconds, err := strconv.Atoi(retryAfter); err == nil {
		return max(time.Duration(seconds)*time.Second, 0), true
	}
	if at, err := http.ParseTime(retryAfter); err == nil {
		return max(at.Sub(now), 0), true
	}
	if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && header.Get("X-RateLimit-Remaining") == "0" {
		return max(time.Unix(reset, 0).Sub(now), 0), true
	}
	if rateLimited {
		return rateLimitedWait, true
	}
	return 0, false
}

// mintCall sends request with client (http.DefaultClient when nil) and reads at most limit bytes of
// its answer, attempting it again as mint's Retry allows: classify decides which failures are
// retried, and mintRetry.again how long the call waits first and whether it may. Each attempt is
// counted, and resends the request's body from GetBody; a request whose body cannot be read again
// gets one attempt. what names the call and who the upstream in its errors. No error quotes the
// request, the answer or the transport's own text: Go's HTTP client quotes a malformed status
// line, and a status line or body can echo the credential the request carried, so a failed call
// is named by its class alone (transportFailure). A status other than want is a *mintStatusError,
// returned with the body, so a caller can read the upstream's own reason from it; every other
// failure wraps ErrMintUpstream. The error of a failed call is its last attempt's, and carries
// the number of attempts made (MintAttempts).
func mintCall(mint *mintRetry, client *http.Client, request *http.Request, what, who string, want int, limit int64) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
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
		if !retry || !resendable || !mint.again(ctx, attempt, err) {
			return body, &attemptsError{err: err, attempts: attempt}
		}
	}
}

// mintAttempt is one attempt of a mintCall, under mintAttemptTimeout within the request's own
// context. A later attempt reads its body from the request's GetBody. A status other than want
// carries the wait the answer names, read against now (serverWait).
func mintAttempt(client *http.Client, request *http.Request, attempt int, now func() time.Time, what, who string, want int, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(request.Context(), mintAttemptTimeout)
	defer cancel()
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
		return nil, &mintTransportError{what: what, who: who, class: transportFailure(request.Context(), err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, &mintTransportError{what: what, who: who, class: transportFailure(request.Context(), err), read: true}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: %s: %s answered more than %d bytes", ErrMintUpstream, what, who, limit)
	}
	if response.StatusCode != want {
		limited := rateLimited(response, body)
		wait, named := serverWait(response.Header, limited, now())
		return body, &mintStatusError{what: what, who: who, code: response.StatusCode, rateLimited: limited, wait: wait, waitNamed: named}
	}
	return body, nil
}

// classify is the one rule for what an attempt's failure was, as broker_mint_attempts_total
// names it, and whether the call is made again. A 5xx, a rate limit and a call or body read that
// failed while the caller's context lives are retried; a 4xx that is no rate limit, a call the
// caller's context ended (cancelled or past its deadline), an answer past its cap and a 2xx or
// 3xx the call did not want are not.
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
		case status.code >= 500:
			return attemptUpstream, true
		case status.code >= 400:
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

// transportFailure names the class of a failed call or body read, never its text: the request was
// cancelled or ran out of time, the upstream's name did not resolve, it could not be reached, its
// TLS handshake failed, or its answer ended early. Any other failure, such as a status line that is
// not HTTP, "gave no usable HTTP answer".
func transportFailure(ctx context.Context, err error) string {
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
	case errors.As(err, &op):
		return "could not be reached"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "ended its answer early"
	}
	return "gave no usable HTTP answer"
}
