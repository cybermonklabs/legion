// The listener's answers as the client reads them: the bound and the decoder every answer is read
// through (decodeAnswer), a role refusal's holder_state, and the readers that turn a refused answer
// into the client's error.

package envoy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/listenerwire"
)

// holderState is a role refusal's holder_state, read as the closed set lapsedRoleHolder tells
// apart: StateUnresponsive, and anything else. It is decided as the answer is decoded, from the
// listener's word as it came, and keeps none of it, so nothing can print it.
type holderState uint8

const (
	holderStateOther holderState = iota
	holderStateUnresponsive
)

// UnmarshalJSON decodes a JSON string into the state it names. A JSON null is holderStateOther,
// and any other JSON value is the type error encoding/json makes of it for a string, which names
// the kind of value and not the value.
func (s *holderState) UnmarshalJSON(data []byte) error {
	var word string
	if err := json.Unmarshal(data, &word); err != nil {
		return err
	}
	*s = holderStateOther
	if word == StateUnresponsive {
		*s = holderStateUnresponsive
	}
	return nil
}

// listenerAnswerLimit bounds one answer of the listener this client reads: the session list of
// a busy deployment is a few MiB.
const listenerAnswerLimit = 64 << 20

// decodeAnswer reads the listener's answer through redact.ReadAnswer and decodes it into v.
// Whatever answers in the listener's place can echo the bearer the call carried in any word of
// its answer, so every redact.RemoteText v holds then holds the call's request record, and an
// error names the member that did not decode and quotes no value of the answer.
func decodeAnswer(response *http.Response, v any) error {
	answer, err := redact.ReadAnswer(response, listenerAnswerLimit)
	if err != nil {
		return err
	}
	return answer.Decode(v)
}

// unavailableGetStatus is ErrUnavailable for a GET of route the listener answered with a status
// the caller does not take, carrying the listener's own reason when its body names one, and
// ErrUnreachable too when that status came from in front of the listener (unavailableStatus).
// The two list reads answer their refusals through listFailure instead, which tells a refilling
// listener from an unavailable one. Whatever answers in the listener's place can echo the bearer
// the read carried, so the reason is read through decodeAnswer and prints with every credential
// the read carried redacted, as every refusal this client reads does.
func unavailableGetStatus(response *http.Response, route string) error {
	var failure listenerwire.APIErrorAs[redact.RemoteText]
	if err := decodeAnswer(response, &failure); err == nil && !failure.Error.IsEmpty() {
		return unavailableStatus(response.StatusCode,
			fmt.Errorf("%w: GET %s returned %d: %s", ErrUnavailable, route, response.StatusCode, failure.Error))
	}
	return unavailableStatus(response.StatusCode,
		fmt.Errorf("%w: GET %s returned %d", ErrUnavailable, route, response.StatusCode))
}

// listenerResponseError is the error a role lookup or a send reports for a non-200 answer. A 502,
// 503 or 504 is ErrUnavailable and ErrUnreachable whatever its body says: the listener behind its
// starting gate answers 503 {"error":"service starting"} (cmd/listener/main.go startingGate), and
// a gateway in front of one that is gone answers 502 or 504, so either is "ask again" rather than
// the listener's verdict on the request. Any other status with a JSON reason is that reason alone,
// which a delivery records and a person reads verbatim ("no live session <id>", "message is
// required", "unauthorized"). The reason is kept on the unavailable error too, for the same
// reader. Any other status without one is ErrUnavailable alone: the listener answered, and asking
// again repeats it.
func listenerResponseError(response *http.Response) error {
	var body listenerwire.APIErrorAs[redact.RemoteText]
	if err := decodeAnswer(response, &body); err != nil {
		return listenerRefusal(response, redact.RemoteText{})
	}
	if response.StatusCode == http.StatusServiceUnavailable && body.Code.Equal(listenerwire.CodeMaybeStored) {
		return unreachable(fmt.Errorf("%w: %w: listener returned %d: %s", ErrUnavailable, ErrStoreUnconfirmed, response.StatusCode, body.Error))
	}
	return listenerRefusal(response, body.Error)
}

// ErrStoreUnconfirmed reports a send the listener answered 503 with the code that says the
// message may be stored (listenerwire.CodeMaybeStored): it published the envelope and the message
// bus never acknowledged the publish, so the session may already hold it, as after
// ErrReceiptTimeout. It is a failure of its own rather than ErrReceiptTimeout because the listener
// did answer; it is ErrUnavailable and ErrUnreachable too, as every 503 is.
var ErrStoreUnconfirmed = errors.New("envoy listener could not confirm the send was stored")

// listenerRefusal is a refused answer as its caller reports it: the listener's own reason when
// its body named one, and its status when the body named nothing a caller could act on. A 502,
// 503 or 504 is ErrUnavailable and ErrUnreachable whatever its body says, reason and all, since
// it comes from in front of the listener or from its starting gate rather than from a decision
// about the request. The reason is the listener's words, or its stand-in's, read through
// decodeAnswer with the credentials the request carried redacted, so it prints as it prints
// itself (redact.RemoteText).
func listenerRefusal(response *http.Response, reason redact.RemoteText) error {
	code := response.StatusCode
	if !reason.IsEmpty() {
		if gatewayStatus(code) {
			return unreachable(fmt.Errorf("%w: listener returned %d: %s", ErrUnavailable, code, reason))
		}
		return errors.New(reason.String())
	}
	return unavailableStatus(code, fmt.Errorf("%w: listener returned %d", ErrUnavailable, code))
}

// listFailure classifies a list read's non-200 answer from its body: the listener's refilling 503
// is a *RefillingError, anything else ErrUnavailable carrying the listener's own reason when its
// body names one, as it prints itself (redact.RemoteText) with every credential the read carried
// redacted (decodeAnswer), and ErrUnreachable too when the status is a 502, 503 or 504, which
// comes from in front of the listener or from its starting gate rather than from a verdict on the
// read.
func listFailure(response *http.Response, route string) error {
	var body listenerwire.APIErrorAs[redact.RemoteText]
	decoded := decodeAnswer(response, &body) == nil
	if decoded && response.StatusCode == http.StatusServiceUnavailable && body.Code.Equal(listenerRefillingCode) {
		return &RefillingError{RetryAfter: response.Header.Get("Retry-After"), route: route, reason: body.Error}
	}
	if !decoded || body.Error.IsEmpty() {
		return unavailableStatus(response.StatusCode,
			fmt.Errorf("%w: %s returned %d", ErrUnavailable, route, response.StatusCode))
	}
	return unavailableStatus(response.StatusCode,
		fmt.Errorf("%w: %s returned %d: %s", ErrUnavailable, route, response.StatusCode, body.Error))
}
