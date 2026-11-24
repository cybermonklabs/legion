package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A push candidate's deliver subject is a reply inbox of the adopting connection's own inbox prefix
// (WithInboxPrefix): the listener's durable delivers to its connection's own inbox, and an edge
// listener's connections name theirs under _INBOX_envoy_edge_<machine id>
// (natsgrant.EdgeInboxPrefix). So a client of a prefix adopts a candidate delivering to a reply
// inbox under that prefix, and refuses one under another, naming its deliver subject: nats.go's
// default _INBOX., under which a shared listener's durable delivers, another edge machine's prefix,
// and two tokens under its own. Before, the check took nats.InboxPrefix alone, so an edge listener
// in the fresh mode refused its own candidate and exited at every start. The control is a client
// with nats.go's default prefix, which adopts a candidate under _INBOX. and refuses an edge one.
func TestAPushCandidateIsAdoptedOnlyUnderItsConnectionsOwnInboxPrefix(t *testing.T) {
	const edge, otherEdge = "_INBOX_envoy_edge_m", "_INBOX_envoy_edge_n"
	_, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	js, err := jetstream.New(owner.Connection())
	if err != nil {
		t.Fatal(err)
	}
	push := func(name, deliver string) jetstream.ConsumerConfig {
		return jetstream.ConsumerConfig{Durable: name, DeliverSubject: deliver, AckPolicy: jetstream.AckExplicitPolicy, FilterSubjects: StreamSubjects()}
	}
	for i, c := range []struct {
		name    string
		prefix  string
		held    string
		adopted bool
	}{
		{name: "an edge client's own candidate", prefix: edge, held: edge + ".PRECREATED", adopted: true},
		{name: "control: a default client's own candidate", held: "_INBOX.PRECREATED", adopted: true},
		{name: "a candidate under the default prefix, at an edge client", prefix: edge, held: "_INBOX.PRECREATED"},
		{name: "a candidate under another edge machine's prefix", prefix: edge, held: otherEdge + ".PRECREATED"},
		{name: "two tokens under the edge client's own prefix", prefix: edge, held: edge + ".PRECREATED.TWO"},
		{name: "control: an edge candidate at a default client", held: edge + ".PRECREATED"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var options []ConnectOption
			if c.prefix != "" {
				options = append(options, WithInboxPrefix(c.prefix))
			}
			client, err := Connect([]string{uri}, options...)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			name := fmt.Sprintf("adoption-inbox-%d", i)
			if _, err := js.CreatePushConsumer(context.Background(), Stream, push(name, c.held)); err != nil {
				t.Fatalf("create %s delivering to %s: %v", name, c.held, err)
			}
			err = client.ConsumerAdoptable(context.Background(), name, push(name, client.Connection().NewInbox()))
			if c.adopted {
				if err != nil {
					t.Fatalf("the adoption of %s delivering to %s, by a client of prefix %q = %v; want it adopted", name, c.held, c.prefix, err)
				}
				return
			}
			if !errors.Is(err, consumernames.ErrUnexpectedConsumer) || !strings.Contains(err.Error(), `deliver_subject "`+c.held+`"`) {
				t.Fatalf("the adoption of %s delivering to %s, by a client of prefix %q = %v; want ErrUnexpectedConsumer naming its deliver subject", name, c.held, c.prefix, err)
			}
		})
	}
}
