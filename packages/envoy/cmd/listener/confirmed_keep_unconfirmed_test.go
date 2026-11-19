package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// A publish to a role no session holds keeps the message for the role's next holder. A keep whose
// publish went out and was never acknowledged may have stored the copy, so the publish route
// answers it as a send that may have landed - a 503 carrying listenerwire.CodeMaybeStored - and not
// with the 404 that tells the sender nothing was kept and its own fallback must run. The control is
// a keep that stored nothing, which keeps today's 404.
func TestAPublishWhoseKeepWentUnacknowledgedIsAnsweredMaybeStored(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"the keep's publish went out and was never acknowledged: 503 maybe_stored",
			fmt.Errorf("%w: %w", bus.ErrUnacknowledged, errors.New("nats: connection closed")), http.StatusServiceUnavailable, listenerwire.CodeMaybeStored},
		{"control: a keep that stored nothing is the 404 it was",
			errors.New("nats: no responders available for request"), http.StatusNotFound, ""},
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
				t.Fatalf("control: the publish made no keep; answered %d %s", recorder.Code, recorder.Body.String())
			}
			var body listenerwire.APIError
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			if recorder.Code != row.want || body.Code != row.code {
				t.Fatalf("answered %d with code %q (%s); want %d with code %q", recorder.Code, body.Code, recorder.Body.String(), row.want, row.code)
			}
		})
	}
}
