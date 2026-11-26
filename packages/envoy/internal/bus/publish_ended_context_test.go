package bus

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A publish whose context has already ended sends nothing and says so: never a message on the wire
// behind an answer of failure. The core transport put it there before - its publish kept the
// caller's spent deadline, ensureConnWithContext returned at once on a healthy connection, and
// publishConfirmed published before it found the window gone - so a role lane's holder received a
// message its sender was told failed, and the sender's retry (the HTTP client retries every 5xx)
// delivered it again over a subject with no duplicate window. The route that reaches it is
// publishHandler, whose reads before the publish can spend the route's deadline before it begins.
//
// The JetStream transport is pinned beside it. It has no fix of its own: nats.go refuses an ended
// context before sending the publish request (publishJetStream says where), and this is what
// keeps an upgrade that stopped doing so from going unnoticed.
//
// Each case subscribes before it publishes and flushes the publisher's connection after, so a
// message that was sent has reached the server, and been handed on, before the subscriber is read.
// A context ends two ways, and both are refused: a deadline already past, the case above, and a
// cancel with the deadline still ahead; publishConfirmed's check of the context before it publishes
// sees both. The control is the same call with a live context, which must publish and be received.
func TestAPublishWithAnEndedContextSendsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		topic string
		core  bool
	}{
		{"a role lane", contracts.RoleTopicPrefix + "ended-context", true},
		{"an uncaptured topic", "notifications.lxspec.ended-context", true},
		{"a topic the stream captures", contracts.LegacyAgentSubject("ses_ended_context"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			client, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			if got := client.UsesCoreTransport(tc.topic); got != tc.core {
				t.Fatalf("UsesCoreTransport(%q) = %t, want %t: the case would not exercise the transport it names", tc.topic, got, tc.core)
			}
			subscriber := testnats.Connect(t, uri)
			t.Cleanup(subscriber.Close)
			received, err := subscriber.SubscribeSync(tc.topic)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if err := subscriber.Flush(); err != nil {
				t.Fatalf("flush the subscription: %v", err)
			}
			envelope := contracts.Envelope{
				EventID:        "evt-ended-context",
				Source:         "agent",
				SourceEventID:  "source-ended-context",
				Topic:          tc.topic,
				DedupeKey:      "publish.ended-context",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "published after its window",
				TraceID:        "trace-ended-context",
			}

			past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancelPast()
			cancelled, cancel := context.WithTimeout(context.Background(), time.Minute)
			cancel()
			for _, ended := range []struct {
				name string
				ctx  context.Context
				err  error
			}{
				{"a deadline already past", past, context.DeadlineExceeded},
				{"a cancelled context", cancelled, context.Canceled},
			} {
				publishErr := client.PublishWithin(ended.ctx, envelope)
				if publishErr == nil {
					t.Fatalf("publish under %s succeeded, want it refused", ended.name)
				}
				if err := client.Conn.Flush(); err != nil {
					t.Fatalf("flush the publisher's connection: %v", err)
				}
				if message, err := received.NextMsg(500 * time.Millisecond); err == nil {
					t.Fatalf("the subscriber received %s from a publish under %s, whose caller was told it failed: %v", string(message.Data), ended.name, publishErr)
				}
				if !errors.Is(publishErr, ended.err) {
					t.Fatalf("publish under %s = %v, want an error naming %v", ended.name, publishErr, ended.err)
				}
			}
			if !tc.core {
				if _, err := client.JS().GetLastMsg(Stream, tc.topic); !errors.Is(err, nats.ErrMsgNotFound) {
					t.Fatalf("the stream's last message on %s after the refused publishes: %v, want none", tc.topic, err)
				}
			}

			// The control: a live context publishes, the subscriber receives it, and the stream
			// retains it where it captures the topic.
			// No span of its own: an answer can come late on a loaded machine with nothing wrong.
			live := testwait.Context(t)
			if err := client.PublishWithin(live, envelope); err != nil {
				t.Fatalf("publish under a live context: %v", err)
			}
			if _, err := received.NextMsgWithContext(live); err != nil {
				t.Fatalf("the subscriber received nothing from the live publish: %v", err)
			}
			if !tc.core {
				if _, err := client.JS().GetLastMsg(Stream, tc.topic); err != nil {
					t.Fatalf("the stream did not retain the live publish on %s: %v", tc.topic, err)
				}
			}
		})
	}
}

// publishConfirmed refuses an ended context before it calls publish, a deadline already spent or a
// cancel with the deadline still ahead, and answers it with the spent-window text wrapping the
// context's error, which is ErrWindowEndedBeforePublish as well. It is the only check of the
// context on the core path (PublishCoreContext reads it nowhere, and ensureConnWithContext returns
// on a healthy connection without consulting it), so it is pinned on its own, both causes driven
// directly: publish must not be called at all. The text is pinned because a deployed filter may
// match it. The control is the same call with a deadline ahead, which publishes, flushes and
// confirms.
func TestAPublishConfirmedWithASpentDeadlinePublishesNothing(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Conn
	const subject = "notifications.team.spent-window"
	published := 0
	publish := func(msg *nats.Msg) error {
		published++
		return conn.PublishMsg(msg)
	}

	past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelPast()
	cancelled, cancel := context.WithTimeout(context.Background(), time.Minute)
	cancel()
	for _, ended := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"a spent deadline", past, context.DeadlineExceeded},
		{"a cancelled context", cancelled, context.Canceled},
	} {
		err = client.publishConfirmed(ended.ctx, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, publish)
		want := `bus: window elapsed before the publish to "` + subject + `" was flushed: ` + ended.err.Error()
		if err == nil || err.Error() != want || !errors.Is(err, ended.err) || !errors.Is(err, ErrWindowEndedBeforePublish) {
			t.Fatalf("publishConfirmed with %s = %v, want %q wrapping %v and ErrWindowEndedBeforePublish", ended.name, err, want, ended.err)
		}
		if published != 0 {
			t.Fatalf("publishConfirmed with %s called publish %d times, want none", ended.name, published)
		}
	}

	// A window that ends after the publish was made is not one that ended before it: the publish
	// stand-in makes its publish only once the deadline has passed (it waits on the context's end, no
	// span), so the publish happens and the flush that follows finds the window spent. The error is
	// the spent window's, and not ErrWindowEndedBeforePublish, which would tell the listener the
	// forward never left this process.
	// A window, not a wait: the window must end while the publish is being made, and 100 ms is how
	// long it lasts.
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	late := 0
	lateErr := client.publishConfirmed(short, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, func(msg *nats.Msg) error {
		<-short.Done()
		late++
		return conn.PublishMsg(msg)
	})
	if late != 1 {
		t.Fatalf("publishConfirmed with a deadline that passes during the publish called publish %d times, want 1", late)
	}
	if lateErr == nil || errors.Is(lateErr, ErrWindowEndedBeforePublish) {
		t.Fatalf("publishConfirmed whose window ended after its publish = %v, want the spent window's error, not ErrWindowEndedBeforePublish", lateErr)
	}

	// No span of its own: an answer can come late on a loaded machine with nothing wrong. forgedWait,
	// since publishConfirmed reads a context with no deadline (-timeout 0) as a spent window.
	live := forgedWait(t)
	if err := client.publishConfirmed(live, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, publish); err != nil {
		t.Fatalf("publishConfirmed with a deadline ahead: %v", err)
	}
	if published != 1 {
		t.Fatalf("publishConfirmed with a deadline ahead called publish %d times, want 1", published)
	}
}

// A core publish whose read of the connection's state has not answered when its ctx ends sends
// nothing. So its error is ErrWindowEndedBeforePublish, and it is still ErrReconnecting. The read
// waits for nats.go's connection lock, which nats.go holds through a reconnect's handshake, and
// publishConfirmed answers a read that ctx ended with reconnectWaitEnded. Its text stays
// `bus: NATS is reconnecting: context deadline exceeded`. The listener marks a role forward that
// fails so with "window_ended_before_publish":true (finishRoleForward), because the forward never
// left this process.
//
// The row holds a reconnect's handshake and makes the publish under a ctx bounded at 300 ms. The
// publish answers that text, with ErrReconnecting, context.DeadlineExceeded and
// ErrWindowEndedBeforePublish, within the bound and a margin of 700 ms, during the attempt held
// when it began (answerByName). Its publish is never called.
//
// The controls: a publish with a live ctx before the hold and once the connection is back is
// confirmed. The negative control is publishConfirmed that returns reconnectWaitEnded alone: its
// answer is not ErrWindowEndedBeforePublish.
func TestAPublishWhoseStateReadItsContextEndedIsAWindowEndedBeforeThePublish(t *testing.T) {
	// Serial (busTestParallelism): each answer has a wall-clock bound, heldBound and heldMargin.
	client, hold := heldHandshakeClient(t)
	conn := client.Connection()
	subject := fmt.Sprintf("held-handshake.ended-read.%d", time.Now().UnixNano())
	var published atomic.Int64
	publish := func(msg *nats.Msg) error {
		published.Add(1)
		return conn.PublishMsg(msg)
	}
	confirmed := func(t *testing.T, when string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, conn.PublishMsg); err != nil {
			t.Fatalf("control: a core publish %s answered %v, want nil", when, err)
		}
	}
	confirmed(t, "before the hold")

	hold.HoldReconnect(t, conn)
	answerByName(t, hold, conn, []heldRow{
		atBound("publishConfirmed", waitEnded, func(ctx context.Context) error {
			err := client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, publish)
			if !errors.Is(err, ErrWindowEndedBeforePublish) {
				return fmt.Errorf("%w, which is not ErrWindowEndedBeforePublish", err)
			}
			return err
		}),
	})
	if n := published.Load(); n != 0 {
		t.Fatalf("the core publish made during the held handshake was called %d time(s), want none", n)
	}

	releaseUntilConnected(t, hold, conn)
	confirmed(t, "once the hold was released")
	if n := published.Load(); n != 0 {
		t.Fatalf("the core publish made during the held handshake was called %d time(s) after the hold's release, want none", n)
	}
}
