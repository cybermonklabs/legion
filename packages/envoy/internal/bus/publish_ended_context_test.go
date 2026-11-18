package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
			// A marker the client publishes on a subject of its own, after a refused publish, is
			// routed to the subscriber behind anything that connection sent before it, so a
			// marker that arrives with the topic's subscription still empty shows the refused
			// publish sent nothing.
			const markerSubject = "notifications.lxspec.ended-context-marker"
			marker, err := subscriber.SubscribeSync(markerSubject)
			if err != nil {
				t.Fatalf("subscribe to the marker: %v", err)
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
			cancelled, cancel := context.WithCancel(testwait.Context(t))
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
				if err := client.Conn.Publish(markerSubject, nil); err != nil {
					t.Fatalf("publish the marker: %v", err)
				}
				if _, err := marker.NextMsgWithContext(testwait.Context(t)); err != nil {
					t.Fatalf("the marker published after %s: %v", ended.name, err)
				}
				if pending, _, _ := received.Pending(); pending != 0 {
					t.Fatalf("the subscriber received %d message(s) from a publish under %s, whose caller was told it failed: %v", pending, ended.name, publishErr)
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
			if err := client.PublishWithin(testwait.Context(t), envelope); err != nil {
				t.Fatalf("publish under a live context: %v", err)
			}
			if _, err := received.NextMsgWithContext(testwait.Context(t)); err != nil {
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
	cancelled, cancel := context.WithCancel(testwait.Context(t))
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

	live := testwait.Context(t)
	if err := client.publishConfirmed(live, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, publish); err != nil {
		t.Fatalf("publishConfirmed with a deadline ahead: %v", err)
	}
	if published != 1 {
		t.Fatalf("publishConfirmed with a deadline ahead called publish %d times, want 1", published)
	}
}

// A window that ends once the publish is made is no window that ended before it: the publish may
// have reached its subscribers, so its error carries the spent window's text and is not
// ErrWindowEndedBeforePublish, which a caller reads as a publish that never left this process. The
// window is armed on the client's fake clock (armBound), and publish moves the fake past it after
// the check before the publish has passed and before it sends, so the window ends between the two.
func TestAWindowThatEndsAfterThePublishIsNotOneThatEndedBeforeIt(t *testing.T) {
	fake := clocktest.New(time.Now())
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithClock(fake))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Conn
	const subject = "notifications.team.window-after-publish"
	const window = time.Second
	published := 0
	publish := func(msg *nats.Msg) error {
		published++
		fake.Advance(window)
		return conn.PublishMsg(msg)
	}
	ctx, cancel := client.armBound(context.Background(), window)
	defer cancel()
	err = client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, publish)
	want := `bus: window elapsed before the publish to "` + subject + `" was flushed`
	if err == nil || err.Error() != want || errors.Is(err, ErrWindowEndedBeforePublish) {
		t.Fatalf("publishConfirmed whose window ended after its publish = %v, want %q and not ErrWindowEndedBeforePublish", err, want)
	}
	if published != 1 {
		t.Fatalf("publishConfirmed whose window ended after its publish called publish %d times, want 1", published)
	}
}
