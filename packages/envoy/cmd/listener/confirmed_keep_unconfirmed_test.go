package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// A publish to a role no session holds keeps the message for the role's next holder. A keep that
// fails on NATS kept nothing that the listener knows of, and nothing says the message cannot be
// kept, so it is answered the bus-unavailable 503 with Retry-After: 1 (keepFailureHolds,
// roleSend.noHolder), and the sender sends again. A keep whose publish went out and was never
// acknowledged may have stored the copy, so its 503 carries listenerwire.CodeMaybeStored
// (writeBusUnavailable), whatever answer ended the keep after it - the stream's refusal of the
// resend a reconnect made (bus.sendStored keeps the first send's bus.ErrUnacknowledged) among
// them, since the first send may have stored the copy; one that stored nothing carries no code. A
// keep a rule refuses is still the 404 (TestAPublishWhoseKeepFailsOnNATSIsAnsweredUnavailable
// holds that half).
func TestAPublishWhoseKeepFailedOnNATSIsAnsweredUnavailable(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		code string
	}{
		{"the keep's publish went out and was never acknowledged: 503 maybe stored",
			fmt.Errorf("%w: %w", bus.ErrUnacknowledged, errors.New("nats: connection closed")), listenerwire.CodeMaybeStored},
		{"an unanswered send, then the stream's refusal of the resend: 503 maybe stored",
			fmt.Errorf("%w: %w", bus.ErrUnacknowledged, &nats.APIError{Code: http.StatusBadRequest, ErrorCode: 10109, Description: "invalid operation on sealed stream"}), listenerwire.CodeMaybeStored},
		{"a keep that stored nothing: 503 with no code",
			errors.New("nats: no responders available for request"), ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			cfg := harness.config
			calls := 0
			cfg.keepPublish = func(context.Context, string, contracts.Envelope, string) (bus.StoredAck, error) {
				calls++
				return bus.StoredAck{}, row.err
			}
			deps := harness.publishDeps(t)
			deps.delivery = &cfg
			role := "keep-unconfirmed"
			recorder, _ := publishThrough(deps, `{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"for whoever holds the role next"}`)
			if calls == 0 {
				t.Fatalf("the publish made no keep; answered %d %s", recorder.Code, recorder.Body.String())
			}
			var body listenerwire.APIError
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			if recorder.Code != http.StatusServiceUnavailable || body.Code != row.code || recorder.Header().Get("Retry-After") != "1" {
				t.Fatalf("answered %d with code %q and Retry-After %q (%s); want 503 with code %q and Retry-After 1",
					recorder.Code, body.Code, recorder.Header().Get("Retry-After"), recorder.Body.String(), row.code)
			}
		})
	}
}
