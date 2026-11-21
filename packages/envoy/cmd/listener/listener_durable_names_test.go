package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// durableRig is a lone server with the stream, a listener client in the given naming mode, the
// selection bucket with its sentinel saying the cutover is done, and a plain connection.
type durableRig struct {
	uri     string
	client  *bus.Client
	conn    *natsgo.Conn
	durable string
}

func newDurableRig(t *testing.T, mode consumernames.Mode) *durableRig {
	t.Helper()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConsumerNames(mode))
	if err != nil {
		t.Fatalf("connect the listener's client: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	return &durableRig{uri: uri, client: client, conn: conn, durable: bus.ListenerDurable("durable-names-" + id.New())}
}

// bind binds the rig's durable as the listener's start and rebuild do, and waits for the server to
// report it bound.
func (r *durableRig) bind(t *testing.T) *natsgo.Subscription {
	t.Helper()
	sub, err := startListenerSubscription(r.client, r.durable, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("bind %s: %v", r.durable, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := awaitDurableBound(context.Background(), r.client, r.durable, 25*time.Millisecond, 10*time.Second); err != nil {
		t.Fatalf("wait for %s bound: %v", r.durable, err)
	}
	return sub
}

// current is the durable's current as its record names it.
func (r *durableRig) current(t *testing.T) string {
	t.Helper()
	record, _, err := r.client.SelectionStore().Read(context.Background(), consumernames.DurableKey(r.durable))
	if err != nil {
		t.Fatalf("read the record of %s: %v", r.durable, err)
	}
	return record.Current
}

// removeFromOutside deletes consumer name as the server's inactivity removal would.
func (r *durableRig) removeFromOutside(t *testing.T, name string) {
	t.Helper()
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteConsumer(bus.Stream, name); err != nil {
		t.Fatalf("remove %s: %v", name, err)
	}
}

// rebuild runs the monitor's path for a durable its probe found gone: the probe, the create again
// and the bound wait (rebuildListenerDependencies).
func (r *durableRig) rebuild(t *testing.T) {
	t.Helper()
	probe := func() error { return durableProbe(context.Background(), r.client, r.durable) }
	if err := rebuildListenerDependencies(context.Background(), r.client, nil, probe, r.durable, func(msg *natsgo.Msg) { _ = msg.Ack() }, nil); err != nil {
		t.Fatalf("rebuild %s: %v", r.durable, err)
	}
}

// createSpy records the consumer names the server is asked to create on the stream.
type createSpy struct {
	mu      sync.Mutex
	creates []string
}

func spyOnCreates(t *testing.T, uri string) (*createSpy, *natsgo.Conn) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	spy := &createSpy{}
	sub, err := conn.Subscribe("$JS.API.CONSUMER.>", func(msg *natsgo.Msg) {
		tokens := strings.Split(msg.Subject, ".")
		spy.mu.Lock()
		defer spy.mu.Unlock()
		switch {
		case len(tokens) >= 6 && tokens[3] == "CREATE" && tokens[4] == bus.Stream:
			spy.creates = append(spy.creates, tokens[5])
		case len(tokens) >= 7 && tokens[3] == "DURABLE" && tokens[4] == "CREATE" && tokens[5] == bus.Stream:
			spy.creates = append(spy.creates, tokens[6])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	return spy, conn
}

// settled is every create name the spy heard, once the server answered a request sent after them.
func (s *createSpy) settled(t *testing.T, conn *natsgo.Conn) []string {
	t.Helper()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.creates...)
}

// createdTwice is the first name sent to create a second time with a consumer of it gone between,
// which only a create after a delete or removal can be: nats.go sends the drift correction's update
// as a create too, of a consumer that exists.
func createdTwice(names []string, removed map[string]bool) string {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] && removed[name] {
			return name
		}
		seen[name] = true
	}
	return ""
}

// TestNoConsumerNameIsCreatedTwice is the fixed-name spy for the listener's own durable: in the fresh
// mode the start's bind and the monitor's rebuild after the server removed the durable create a fresh
// <durable>~<nuid> each, and never the fixed name. Its control is the fixed mode's own rebuild, which
// the spy sees create the fixed name again after its removal.
func TestNoConsumerNameIsCreatedTwice(t *testing.T) {
	t.Run("control: the fixed mode's rebuild", func(t *testing.T) {
		r := newDurableRig(t, consumernames.ModeFixed)
		spy, spyConn := spyOnCreates(t, r.uri)
		r.bind(t)
		r.removeFromOutside(t, r.durable)
		r.rebuild(t)
		if twice := createdTwice(spy.settled(t, spyConn), map[string]bool{r.durable: true}); twice != r.durable {
			t.Fatalf("the spy saw %q created again after its removal; it cannot see the fixed mode's re-creation of %s", twice, r.durable)
		}
	})
	t.Run("the fresh mode", func(t *testing.T) {
		r := newDurableRig(t, consumernames.ModeFresh)
		spy, spyConn := spyOnCreates(t, r.uri)
		r.bind(t)
		removed := map[string]bool{}
		for range 2 {
			name := r.current(t)
			removed[name] = true
			r.removeFromOutside(t, name)
			r.rebuild(t)
		}
		creates := spy.settled(t, spyConn)
		if twice := createdTwice(creates, removed); twice != "" {
			t.Fatalf("the fresh mode created %s again after its removal: %v", twice, creates)
		}
		for _, name := range creates {
			if name == r.durable || !strings.HasPrefix(name, r.durable+"~") {
				t.Fatalf("the fresh mode created %s, want only %s~<nuid> names: %v", name, r.durable, creates)
			}
		}
		if len(removed) != 2 || len(creates) < 3 {
			t.Fatalf("the spy heard %v over %d removals: it heard too little to judge", creates, len(removed))
		}
	})
}

// TestEveryReaderFindsTheCurrentName: after the durable was created again under a fresh name, every
// reader of it (the start's check, the probe, the bound wait, /healthz's read through durableInfo)
// reads the current its record names, and the session listing never takes it for a session. The
// control is a reader of the fixed name, which finds nothing: a reader left on it would report the
// durable gone.
func TestEveryReaderFindsTheCurrentName(t *testing.T) {
	r := newDurableRig(t, consumernames.ModeFresh)
	r.bind(t)
	first := r.current(t)
	r.removeFromOutside(t, first)
	r.rebuild(t)
	current := r.current(t)
	if current == first || !strings.HasPrefix(current, r.durable+"~") {
		t.Fatalf("the rebuild selected %q after %q, want a fresh %s~<nuid>", current, first, r.durable)
	}
	ctx := context.Background()
	// Control: the fixed name reads as no consumer.
	if _, err := r.client.JS().ConsumerInfo(bus.Stream, r.durable); !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("the fixed name %s read %v, want no consumer", r.durable, err)
	}
	if info, err := listenerDurable(r.client, r.durable); err != nil || info == nil || info.Name != current {
		t.Fatalf("the start's check read %v, %v; want %s", info, err, current)
	}
	if info, err := durableInfo(ctx, r.client, r.durable); err != nil || info.Name != current {
		t.Fatalf("durableInfo (/healthz's read) read %v, %v; want %s", info, err, current)
	}
	if err := durableProbe(ctx, r.client, r.durable); err != nil {
		t.Fatalf("the probe of the bound current = %v, want nil", err)
	}
	if err := awaitDurableBound(ctx, r.client, r.durable, 25*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("the bound wait = %v, want the current bound", err)
	}
	consumers, err := bus.ConnectSessionConsumers([]string{r.uri}, 0, nil, bus.WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(consumers.Close)
	sets, err := consumers.SessionConsumerSets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for sessionID, set := range sets {
		for _, info := range set {
			if info.Name == current {
				t.Fatalf("the session listing took the listener's durable %s for session %s", current, sessionID)
			}
		}
	}
	// Control: a listing by envoy.logical alone holds the durable, so a sweep reading it would take
	// the durable for a session and delete it.
	var byLogical []string
	for name := range r.client.JS().ConsumerNames(bus.Stream) {
		info, err := r.client.JS().ConsumerInfo(bus.Stream, name)
		if err == nil && info.Config.Metadata[consumernames.LogicalMetadataKey] != "" {
			byLogical = append(byLogical, name)
		}
	}
	if !strings.Contains(" "+strings.Join(byLogical, " ")+" ", " "+current+" ") {
		t.Fatalf("a listing by envoy.logical held %v, without the durable %s: the control cannot fail", byLogical, current)
	}
}
