package main

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A registration in the fresh naming mode whose user may not read the selection bucket is refused
// 503 no_grant, as a refused consumer call is: the read refusal is the grant's, not lost selection
// state, and nothing is created. The control is the same registration by a user the server lets
// read the bucket, which gets its consumer.
func TestAFreshRegistrationRefusedTheSelectionReadAnswersNoGrant(t *testing.T) {
	seed, public := testnats.User(t)
	stream := "KV_" + consumernames.SessionBucket
	uri := testnats.StartNkeyDenied(t, public, "$JS.API.STREAM.MSG.GET."+stream, "$JS.API.DIRECT.GET."+stream+".>")
	ctx := context.Background()
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	if _, err := consumernames.CreateSessionBucket(ctx, admin, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	register := func(t *testing.T, sessionID string) (*deliveryRefusal, sessionDeliveryOutcome) {
		t.Helper()
		client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConsumerNames(consumernames.ModeFresh))
		if err != nil {
			t.Fatalf("connect the listener's client: %v", err)
		}
		t.Cleanup(client.Close)
		delivery, err := connectSessionDelivery([]string{uri}, bus.SessionConsumerInactiveThreshold, client, logging.NewWithWriter("test", io.Discard))
		if err != nil {
			t.Fatalf("open the session-consumer connection: %v", err)
		}
		t.Cleanup(delivery.close)
		if !delivery.consumers.Fresh() {
			t.Fatal("the session-consumer connection did not take the listener client's fresh mode")
		}
		outcome, refusal, unlock := delivery.consumerFor(ctx, listenerwire.SubscribeRequest{
			SessionID: sessionID,
			Delivery:  &listenerwire.SubscribeDelivery{Consumer: true},
		}, []string{contracts.LegacyAgentSubject(sessionID)})
		unlock()
		return refusal, outcome
	}

	t.Run("control: a granted read", func(t *testing.T) {
		refusal, outcome := register(t, "ses_granted_read")
		if refusal != nil {
			t.Fatalf("a registration whose user may read the bucket was refused %d %s: %s", refusal.status, refusal.Reason, refusal.Error)
		}
		if outcome.answer == nil || outcome.answer.Mode != deliveryModeConsumer {
			t.Fatalf("the granted registration answered %+v, want consumer delivery", outcome.answer)
		}
	})
	t.Run("a refused read", func(t *testing.T) {
		t.Setenv("NATS_NKEY_SEED", seed)
		refusal, _ := register(t, "ses_refused_read")
		if refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.Reason != deliveryReasonNoGrant {
			t.Fatalf("a registration whose user may not read the bucket answered %+v, want 503 %s", refusal, deliveryReasonNoGrant)
		}
	})
}
