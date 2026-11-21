// These rows start the real NATS auth service (internal/testnats/servicecallout), which links the
// broker's packages into this test binary. The server-change soak builds this package with the
// serverchange tag under a filter of its own, hand-kept to what that binary compiles
// (.github/ci-filters.yaml, listener_server_change), so this file stays out of that build: its
// rows run in the race lane, which builds cmd/listener with no tag.

//go:build !serverchange

package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testnats/servicecallout"
	"github.com/sjawhar/envoy/natscred"
)

// calloutListenerAccount is the account of the listener calloutHub admits.
const calloutListenerAccount = "system:serviceaccount:example-ns:example-listener"

// calloutHub is a server whose auth service admits one listener entry, machine "hub-a" of
// calloutListenerAccount, by its platform token, with an admin user of the server's own that holds
// every subject: the old task of a rolling deploy, which connected by its seed and created its
// durable on an _INBOX. subject.
type calloutHub struct {
	url, adminSeed, tokenFile string
}

func startCalloutHub(t *testing.T) calloutHub {
	t.Helper()
	adminSeed, adminKey := testnats.User(t)
	url := servicecallout.Start(t, "listener/hub-a/*="+calloutListenerAccount,
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	return calloutHub{url: url, adminSeed: adminSeed, tokenFile: servicecallout.TokenFile(t, calloutListenerAccount, 50*time.Minute)}
}

// connect is a client of the hub: the admin's by its seed, or the listener's by its token.
func (h calloutHub) connect(t *testing.T, admin bool) *bus.Client {
	t.Helper()
	environment := map[string]string{bus.AllowRemoteEnvVar: "1", bus.ServiceTokenFileEnvVar: h.tokenFile}
	if admin {
		environment = map[string]string{bus.AllowRemoteEnvVar: "1", bus.NkeySeedEnvVar: h.adminSeed}
	}
	client, err := bus.ConnectOwningStream([]string{h.url}, bus.WithReplicas(1), bus.WithEnvironment(func(name string) (string, bool) {
		value, set := environment[name]
		return value, set
	}))
	if err != nil {
		t.Fatalf("connect (admin %t): %v", admin, err)
	}
	t.Cleanup(client.Close)
	return client
}

// delivering binds the listener's durable through bindListenerDurable at interval and returns a
// channel of the subjects it delivers.
func delivering(t *testing.T, client *bus.Client, consumer string, interval time.Duration) (<-chan string, durableBind, error) {
	t.Helper()
	delivered := make(chan string, 16)
	bind, err := bindListenerDurable(t.Context(), client, consumer, func(msg *natsgo.Msg) {
		delivered <- msg.Subject
		_ = msg.Ack()
	}, logging.NewWithWriter("test", io.Discard), interval, 30*time.Second)
	return delivered, bind, err
}

// awaitDelivery publishes subject through admin's stream and waits for it on delivered.
func awaitDelivery(t *testing.T, admin *bus.Client, delivered <-chan string, subject string) {
	t.Helper()
	if _, err := admin.JS().Publish(subject, []byte("{}")); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	select {
	case got := <-delivered:
		if got != subject {
			t.Fatalf("delivered %s, want %s", got, subject)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was never delivered", subject)
	}
}

// A durable made before the listener connected by its service token, delivering on an _INBOX.
// subject, delivers to that listener once it starts: the auth service grants a service no inbox
// but its own reply prefix (natscred.InboxPrefix), so the start moves the deliver subject under
// it, and a restarted listener binds the moved durable as it is.
func TestAnOldDurableDeliversToAListenerOnItsServiceToken(t *testing.T) {
	hub := startCalloutHub(t)
	admin := hub.connect(t, true)
	consumer := bus.ListenerDurable("hub-a")
	old, err := startListenerSubscription(admin, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("the old task's durable: %v", err)
	}
	if err := old.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	before, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !strings.HasPrefix(before.Config.DeliverSubject, natsgo.InboxPrefix) {
		t.Fatalf("the old durable delivers on %v (%v), want an _INBOX. subject", before, err)
	}

	listener := hub.connect(t, false)
	delivered, _, err := delivering(t, listener, consumer, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("bind the old durable: %v", err)
	}
	awaitDelivery(t, admin, delivered, "notifications.dispatch.issue.EX.first")
	after, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !strings.HasPrefix(after.Config.DeliverSubject, natscred.InboxPrefix(calloutListenerAccount)+".") {
		t.Fatalf("the durable delivers on %v (%v), want under the listener's own prefix", after, err)
	}

	listener.Close()
	restarted := hub.connect(t, false)
	delivered, _, err = delivering(t, restarted, consumer, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("bind after the restart: %v", err)
	}
	awaitDelivery(t, admin, delivered, "notifications.dispatch.issue.EX.second")
	again, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || again.Config.DeliverSubject != after.Config.DeliverSubject {
		t.Fatalf("the restart moved the durable again, to %v (%v)", again, err)
	}
}

// A rolling start: the old task still holds the durable on its _INBOX. subject when the listener
// on its service token starts. nats-server refuses the move with 10013 while the old subject has a
// subscriber, so the start retries every durableBindInterval and moves and binds the durable within
// one interval of the old task's exit. The control is a start with no retry, one attempt, which
// fails with 10013.
func TestARollingStartMovesTheDurableOnceTheOldTaskLeaves(t *testing.T) {
	hub := startCalloutHub(t)
	admin := hub.connect(t, true)
	consumer := bus.ListenerDurable("hub-a")
	oldTask := hub.connect(t, true)
	if _, err := startListenerSubscription(oldTask, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() }); err != nil {
		t.Fatalf("the old task's durable: %v", err)
	}
	waitFor(t, 5*time.Second, "the old task's durable to be push-bound", func() bool {
		info, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.PushBound
	})
	listener := hub.connect(t, false)

	t.Run("control: one attempt with no retry", func(t *testing.T) {
		_, err := startListenerSubscription(listener, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
		var apiErr *natsgo.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode != natsgo.JSErrCodeConsumerNameExists {
			t.Fatalf("a single start beside the old task's subscriber: %v, want 10013", err)
		}
		if durableRefused(err) {
			t.Fatalf("10013 is classed as a refusal, which ends the start: %v", err)
		}
	})

	type outcome struct {
		delivered <-chan string
		err       error
		at        time.Time
	}
	done := make(chan outcome, 1)
	go func() {
		delivered, _, err := delivering(t, listener, consumer, durableBindInterval)
		done <- outcome{delivered, err, time.Now()}
	}()
	time.Sleep(3 * time.Second)
	select {
	case early := <-done:
		t.Fatalf("the start returned while the old task held the durable: %v", early.err)
	default:
	}
	left := time.Now()
	oldTask.Close()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("bind after the old task left: %v", got.err)
		}
		if after := got.at.Sub(left); after > durableBindInterval+time.Second {
			t.Fatalf("the start bound %s after the old task left, want within one %s interval", after, durableBindInterval)
		}
		awaitDelivery(t, admin, got.delivered, "notifications.dispatch.issue.EX.rolled")
	case <-time.After(15 * time.Second):
		t.Fatal("the start never bound the durable the old task left")
	}
}
