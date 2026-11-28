package dispatch

import (
	"errors"
	"net/http"
)

// AcceptanceWrite is one keyed acceptance write: the status the daemon writes on Issue, fenced on
// Sequence (expected_sequence), under Key (IdempotencyKeyHeader). A key names one request: every
// retry of that request carries the same key and the same body, and a request at another sequence
// is another request with a key of its own, since Dispatch refuses a key held under another
// request's digest (KEY_REUSED).
type AcceptanceWrite struct {
	Key      string
	Issue    string
	Status   string
	Sequence int64
}

// Verdict is how Dispatch's keyed layer settled one keyed write, read from one answer
// (KeyedVerdict).
type Verdict int

const (
	// Unsettled says the answer settles nothing about the request under its key: no answer, an
	// outage, the key's first request still running, or an answer that names no key back. The
	// request is sent again under the same key and bytes.
	Unsettled Verdict = iota
	// Landed is a 2xx naming the request's key back, fresh or replayed: the write committed.
	Landed
	// Refused is Dispatch's verdict on this exact request, naming its key back: a refusal its
	// handler stored, a stored answer replayed, or KEY_REUSED. The write did not land, and no attempt
	// under the key ever will.
	Refused
	// Committed is KEY_COMMITTED: a transaction of the key's request committed and its answer was
	// lost. The request's status write may or may not be among what committed: a close settles its
	// documents in a transaction of its own before its status's (Dispatch's issue_patch.go), so the
	// caller reads the issue's event log to tell.
	Committed
)

// KeyedVerdict reads the answer to a keyed write as the agents' client reads one
// (packages/envoy-client/src/dispatch-http.ts, answerEvent), for the answers a daemon write can
// meet: status is the answer's HTTP status, 0 where no answer came back; code its error code;
// echoed whether it named exactly the request's key back (IdempotencyKeyHeader); replayed whether it
// carried IdempotencyReplayedHeader. Every answer of Dispatch's keyed layer names the request's
// key back, so an answer that names none, or another request's, is no verdict of it, whatever its
// status says: a layer before the claim answered it, or a build without the keyed layer.
func KeyedVerdict(status int, code string, echoed, replayed bool) Verdict {
	switch {
	case status == 0:
		return Unsettled
	case status/100 == 2:
		if echoed {
			return Landed
		}
		return Unsettled
	case status/100 == 3:
		return Unsettled
	// An echoed 401 is no outcome of the key: the keyed layer releases the key on it, and it can
	// come after the handler committed.
	case status == http.StatusUnauthorized:
		return Unsettled
	case retriesUnderKey(status, code):
		return Unsettled
	case !echoed:
		return Unsettled
	case code == IdempotencyKeyCommitted:
		return Committed
	case replayed:
		return Refused
	case !storedKeyStatus(status):
		return Unsettled
	case code == IdempotencyKeyReused:
		return Refused
	// The fresh-handler clause: no code of the keyed layer's own is a handler's stored answer.
	case code == IdempotencyKeyInFlight:
		return Unsettled
	default:
		return Refused
	}
}

// retriesUnderKey is the contracts' rule of the same name (packages/contracts/src/dispatch-api-limits.ts):
// an answer Dispatch does not store, a 5xx, a 408 or a 429, or a key whose first request still runs.
func retriesUnderKey(status int, code string) bool {
	return status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusConflict && code == IdempotencyKeyInFlight
}

// storedKeyStatus is a status the keyed layer stores as the key's answer: a 4xx but a 401, a 408
// and a 429 (dispatch-http.ts, isStoredKeyStatus).
func storedKeyStatus(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusUnauthorized &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// VerdictOf is the verdict of err, the error a keyed write answered (HTTPClient.SetAcceptanceStatus):
// nil is Landed, a *Error carries its answer's verdict, and every other error, a transport
// failure among them, is Unsettled.
func VerdictOf(err error) Verdict {
	if err == nil {
		return Landed
	}
	var answer *Error
	if !errors.As(err, &answer) {
		return Unsettled
	}
	return KeyedVerdict(answer.Status, answer.Code, answer.Echoed, answer.Replayed)
}
