package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Every path that decides whether NATS refused a publish for good reads one list
// (busRefusesForGood), so the paths agree on every refusal class: the role lane ends a forward on
// it (roleForwardEnded), ends a keep on it rather than holding the message (keepFailureHolds), and
// the publish route answers it with a 4xx rather than the 5xx it gives a failure that another
// attempt can get past (writeBusUnavailable). Each sender of ours ends on that 4xx, except two that
// meet it again at their own cadence: the phase reminder at its next delivered reminder, and a
// session's registration, which its own route refuses the same way, at its next heartbeat. Each
// class is wrapped as the bus returns it. A stream's refusal of a resend after a send nothing
// answered (bus.ErrUnacknowledged, which bus.sendStored keeps) is no refusal for good: the first
// send may have stored the message, so the keep holds it and the route answers the maybe-stored
// 503. A role token no claim can name is the keep's own rule: it ends a keep and is no refusal of
// NATS's. The rows that need a read of the deployed stream (no responders, no acknowledgement) are
// the forward's alone and are in its own table
// (TestListenerDeliveryHandler_RoleForwardFailureEndsOnlyUnderARule).
func TestTheRoleLanesRefusalRulesAgree(t *testing.T) {
	answered := func(status, code int, description string, names ...error) error {
		var err error = &natsgo.APIError{Code: status, ErrorCode: natsgo.ErrorCode(code), Description: description}
		for _, name := range names {
			err = fmt.Errorf("%w: %w", name, err)
		}
		return fmt.Errorf("%w: %w", bus.ErrForwardNotStored, err)
	}
	subject := contracts.LegacyAgentSubject("ses_refusal_rules")
	cases := []struct {
		name    string
		err     error
		refused bool
	}{
		{"a message past the server's max payload", fmt.Errorf("forward: %w", bus.ErrTooLarge), true},
		{"a subject NATS does not accept", fmt.Errorf("forward: %w", bus.ErrInvalidSubject), true},
		{"a forward with no message id", fmt.Errorf("forward: %w", bus.ErrNoForwardMsgID), true},
		{"a subject the listener's grant denies", fmt.Errorf("forward: %w", bus.ErrPublishDenied), true},
		{"a message past the stream's maximum size", answered(400, 10054, "message size exceeds maximum allowed"), true},
		{"headers past 64 KiB", answered(400, 10097, "header size exceeds maximum allowed of 64k"), true},
		{"a message too large for the store", answered(503, 10077, "message too large"), true},
		{"a sealed stream", answered(400, 10109, "invalid operation on sealed stream"), true},
		{"an unanswered send, then a sealed stream's refusal of the resend", fmt.Errorf("%w: %w", bus.ErrUnacknowledged,
			&natsgo.APIError{Code: 400, ErrorCode: 10109, Description: "invalid operation on sealed stream"}), false},
		{"a full stream", answered(503, 10077, "maximum bytes exceeded", bus.ErrStreamFull), false},
		{"an id still in process", answered(409, 10158, "duplicate message id is in process", bus.ErrDuplicateInProcess), false},
		{"a closed store", answered(503, 10077, "store is closed"), false},
		{"insufficient resources", answered(503, 10023, "insufficient resources"), false},
		{"an account past its limits", answered(400, 10002, "resource limits exceeded for account"), false},
		{"a publish not known to have been accepted", fmt.Errorf("%w: %w", bus.ErrForwardNotStored, bus.ErrPublishUnconfirmed), false},
		{"a flush that ran out", fmt.Errorf("bus: flush publish: %w", natsgo.ErrTimeout), false},
		{"a connection that closed", errors.New("nats: connection closed"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := busRefusesForGood(tc.err); got != tc.refused {
				t.Errorf("busRefusesForGood = %v, want %v", got, tc.refused)
			}
			if got := roleForwardEnded(nil, subject, tc.err, roleForwardTimeout); got != tc.refused {
				t.Errorf("roleForwardEnded = %v, want %v: the forward and the list disagree", got, tc.refused)
			}
			if got := keepFailureHolds(tc.err); got == tc.refused {
				t.Errorf("keepFailureHolds = %v, want %v: the keep and the list disagree", got, !tc.refused)
			}
			recorder := httptest.NewRecorder()
			status := writeBusUnavailable(recorder, tc.err)
			if refusedByRoute := status < http.StatusInternalServerError; refusedByRoute != tc.refused {
				t.Errorf("the route answers %d, a refusal %v, want a refusal %v: the route and the list disagree", status, refusedByRoute, tc.refused)
			}
		})
	}
	t.Run("a role token no claim can name", func(t *testing.T) {
		err := fmt.Errorf("keep: %w", errRoleNotKeepable)
		if keepFailureHolds(err) {
			t.Error("keepFailureHolds holds a keep under a role token no claim can name, which no attempt can keep")
		}
		if busRefusesForGood(err) {
			t.Error("busRefusesForGood counts the keep's own rule as a refusal of NATS's")
		}
	})
}
