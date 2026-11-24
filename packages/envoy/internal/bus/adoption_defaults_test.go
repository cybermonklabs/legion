package bus

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A candidate the server stored from its family's own create needs no correction: the correction
// compares each setting after the server's own defaulting of the create (serverDefaulted), so a
// setting the create leaves to the server (max deliver, ack wait, a pull consumer's max waiting)
// reads as the server stored it. The creates are the session family's own (freshConfig, which sets
// each of them) and a durable's that leaves every one unset, as Dispatch's leaves max deliver. A
// candidate whose setting really differs is still corrected: one stored with max deliver 5, which
// the create leaves unset, is corrected to the server's default, -1. Before, the correction
// compared the create's unset max deliver (0) and ack wait (0) with the server's stored -1 and 30 s,
// and sent an update on every bind of such a durable.
func TestACandidateStoredFromItsFamilysCreateNeedsNoCorrection(t *testing.T) {
	owner, consumers, _ := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	js, err := jetstream.New(owner.Connection())
	if err != nil {
		t.Fatal(err)
	}
	session := "ses_defaults"
	filters := SessionConsumerFilters([]string{contracts.LegacyAgentSubject(session)})
	unset := func(name string) jetstream.ConsumerConfig {
		return jetstream.ConsumerConfig{
			Durable:       name,
			DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:   1,
			AckPolicy:     jetstream.AckExplicitPolicy,
			MaxAckPending: 64,
			FilterSubject: "notifications.delivery.defaults",
			Metadata:      consumernames.LogicalNaming().Metadata("delivery-defaults"),
		}
	}
	for _, c := range []struct {
		name    string
		create  jetstream.ConsumerConfig
		stored  func(*jetstream.ConsumerConfig)
		changed bool
	}{
		{name: "the session family's create", create: consumers.freshConfig("session~DEFAULTS", session, 1, filters)},
		{name: "a durable's create that leaves the defaulted settings unset", create: unset("delivery-defaults~A")},
		{name: "a candidate stored with max deliver 5, which the create leaves unset", create: unset("delivery-defaults~B"),
			stored: func(c *jetstream.ConsumerConfig) { c.MaxDeliver = 5 }, changed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			made := c.create
			if c.stored != nil {
				c.stored(&made)
			}
			if _, err := js.CreateConsumer(ctx, Stream, made); err != nil {
				t.Fatal(err)
			}
			info, err := consumers.client.ConsumerInfo(ctx, made.Durable)
			if err != nil {
				t.Fatal(err)
			}
			next, changed := CorrectedConfig(info.Config, c.create)
			if changed != c.changed {
				t.Fatalf("CorrectedConfig of %s as the server stored it = changed %t, want %t:\nstored %+v\nnext   %+v", made.Durable, changed, c.changed, info.Config, next)
			}
			if c.changed && next.MaxDeliver != -1 {
				t.Fatalf("the correction writes max deliver %d, want the server's default -1", next.MaxDeliver)
			}
		})
	}
}
