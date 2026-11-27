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

// A publish to a role no session holds keeps the message for the role's next holder. A keep that
// fails is answered 404 unclaimed, as at main, whether its publish stored nothing or went out and
// was never acknowledged (and so may have stored the copy): this change keeps what each role route
// answers. Both rows are controls of main's answer.
func TestAPublishWhoseKeepFailedIsAnsweredAsAtMain(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		err  error
	}{
		{"control: the keep's publish went out and was never acknowledged: 404, as at main",
			fmt.Errorf("%w: %w", bus.ErrUnacknowledged, errors.New("nats: connection closed"))},
		{"control: a keep that stored nothing: 404, as at main",
			errors.New("nats: no responders available for request")},
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
			if recorder.Code != http.StatusNotFound || body.Code != "" {
				t.Fatalf("answered %d with code %q (%s); want 404 with no code, as at main", recorder.Code, body.Code, recorder.Body.String())
			}
		})
	}
}
