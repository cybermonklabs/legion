package bus_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// requestClient starts a NATS server of the test's own and connects a client that owns the
// notification stream, which captures every agent subject, under options of the test's own.
func requestClient(t *testing.T, options ...bus.ConnectOption) (*testnats.Server, string, *bus.Client) {
	t.Helper()
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url}, options...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	return server, url, client
}

// forwardEnvelope is a role forward as Dispatch sends one to a holder: its topic is the role's,
// whatever subject it is sent to.
func forwardEnvelope(name string) contracts.Envelope {
	return contracts.Envelope{
		EventID:       "evt-" + name,
		Source:        "dispatch",
		SourceEventID: "src-" + name,
		Topic:         "notifications.role.request-test",
		DedupeKey:     "envoy.role.forward.dispatch-" + name,
		IssuedAt:      contracts.NowMillis(),
	}
}

// responder subscribes subject on a connection of its own and answers each message with an empty
// receipt, which answers no Request: nothing but the stream does.
func responder(t *testing.T, url, subject string) {
	t.Helper()
	conn, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the responder: %v", err)
	}
	t.Cleanup(conn.Close)
	if _, err := conn.Subscribe(subject, func(message *natsgo.Msg) { _ = message.Respond(nil) }); err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the responder's subscription: %v", err)
	}
}

// answerOf is outcome's answer.
func answerOf(t *testing.T, outcome <-chan error) error {
	t.Helper()
	return testwait.Receive(t, "the answer to the window's send", outcome)
}

// requestClock is the clock a row below hands its client (bus.WithClock): a Request's timeout is a
// timer on it, so a request that waits out its timeout does so only when the row moves this clock,
// while its flush, which nats.go times itself, keeps the wall clock.
func requestClock() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
}

// A Request is answered by the stream's acknowledgement of the copy it stored, and by nothing
// else. An empty receipt is passed over, so a Request to a subject the stream does not capture
// runs out of time though a subscriber of that subject answered it at once: its timeout is a timer
// on the client's clock, which the row moves to it once the request has armed it, and the
// acknowledged request before it is answered with that clock standing still.
func TestARequestIsAnsweredByTheStreamsAcknowledgementAlone(t *testing.T) {
	clock := requestClock()
	_, url, client := requestClient(t, bus.WithClock(clock))
	window := client.WindowUntil(nil)

	stored := contracts.LegacyAgentSubject("ses_request_stored")
	if err := answerOf(t, window.Request(stored, forwardEnvelope("stored"), bus.RequestOptions{MsgID: bus.ForwardMsgID("stored", stored)})); err != nil {
		t.Fatalf("a Request the stream acknowledged, with nobody subscribed to its subject, returned %v, want nil", err)
	}

	// No stream subject captures a role lane (bus.StreamSubjects), so the receipt below is the
	// only reply this Request gets.
	const uncaptured = "notifications.role.request-receipt"
	responder(t, url, uncaptured)
	outcome := window.Request(uncaptured, forwardEnvelope("receipt"), bus.RequestOptions{MsgID: bus.ForwardMsgID("receipt", uncaptured)})
	timeout := clock.Now().Add(bus.PublishAcknowledgementTimeout)
	awaitArmedAt(t, clock, "the timeout of the Request its receiver answers with a receipt", timeout.Equal)
	clock.Set(timeout)
	if err := answerOf(t, outcome); !errors.Is(err, bus.ErrNotAcknowledged) {
		t.Fatalf("a Request its receiver answered with an empty receipt returned %v, want ErrNotAcknowledged: the stream stored nothing of it", err)
	}
}

// A Request nobody answers ends when its timeout runs out, as not acknowledged, never as a lost
// connection: the server's no-responders answer is no acknowledgement and does not end the wait.
// The timeout is a timer on the client's clock, armed at the request's own window from its send,
// which the row waits to see armed, among the client's other timers, before it moves the clock to
// it.
func TestARequestNobodyAnswersIsNotAcknowledgedWithinItsTimeout(t *testing.T) {
	clock := requestClock()
	_, _, client := requestClient(t, bus.WithClock(clock))

	started := clock.Now()
	outcome := client.WindowUntil(nil).Request("notifications.role.request-nobody", forwardEnvelope("nobody"),
		bus.RequestOptions{MsgID: bus.ForwardMsgID("nobody", "notifications.role.request-nobody")})
	timeout := started.Add(bus.PublishAcknowledgementTimeout)
	awaitArmedAt(t, clock, fmt.Sprintf("the unanswered Request's timeout, its %s window from the send", bus.PublishAcknowledgementTimeout), timeout.Equal)
	if len(outcome) != 0 {
		t.Fatalf("an unanswered Request answered %v before its timeout", <-outcome)
	}
	clock.Set(timeout)
	if err := answerOf(t, outcome); !errors.Is(err, bus.ErrNotAcknowledged) || errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("an unanswered Request returned %v, want ErrNotAcknowledged, not ErrUnavailable", err)
	}
}

// A Request that ran out of time says whether its publish was confirmed: one whose flush confirmed
// the publish is not acknowledged alone, a request the server took and the stream stored nothing
// of, and one whose flush could not confirm it wraps ErrPublishUnconfirmed with the flush's own
// reason. A caller counts the two apart - Dispatch's outbox counts a role forward the stream did
// not acknowledge under `unanswered` and a forward that may never have left this process under
// `failed` - and both wrap ErrNotAcknowledged, so a caller that only backs the send off is
// unaffected.
func TestAnUnansweredRequestSaysWhetherItsPublishWasConfirmed(t *testing.T) {
	clock := requestClock()
	server, _, client := requestClient(t, bus.WithClock(clock))
	window := client.WindowUntil(nil)

	confirmed := window.Request("notifications.role.request-confirmed", forwardEnvelope("confirmed"),
		bus.RequestOptions{MsgID: bus.ForwardMsgID("confirmed", "notifications.role.request-confirmed")})
	confirmedTimeout := clock.Now().Add(bus.PublishAcknowledgementTimeout)
	awaitArmedAt(t, clock, "the timeout of the Request whose publish was confirmed", confirmedTimeout.Equal)
	clock.Set(confirmedTimeout)
	if err := answerOf(t, confirmed); !errors.Is(err, bus.ErrNotAcknowledged) || errors.Is(err, bus.ErrPublishUnconfirmed) {
		t.Fatalf("an unanswered Request whose publish was confirmed returned %v, want ErrNotAcknowledged and not ErrPublishUnconfirmed", err)
	}

	// A frozen server answers the flush's PING with nothing, so the flush runs out of the
	// request's own window - which nats.go times itself, on the wall clock - while the connection
	// stays connected: the publish is not known to have been accepted. The request's own timeout
	// is the clock's, and its answer waits for that flush.
	thaw := server.Freeze(t)
	unconfirmed := window.Request("notifications.role.request-unconfirmed", forwardEnvelope("unconfirmed"),
		bus.RequestOptions{MsgID: bus.ForwardMsgID("unconfirmed", "notifications.role.request-unconfirmed")})
	unconfirmedTimeout := clock.Now().Add(bus.PublishAcknowledgementTimeout)
	awaitArmedAt(t, clock, "the timeout of the Request whose flush the frozen server never answers", unconfirmedTimeout.Equal)
	clock.Set(unconfirmedTimeout)
	err := answerOf(t, unconfirmed)
	connected := client.Connected()
	thaw()
	if !connected {
		t.Fatal("the client lost its connection while the server was frozen; the Request says nothing about an unconfirmed flush")
	}
	if !errors.Is(err, bus.ErrPublishUnconfirmed) || !errors.Is(err, bus.ErrNotAcknowledged) {
		t.Fatalf("an unanswered Request whose flush ran out of time returned %v, want ErrNotAcknowledged wrapping ErrPublishUnconfirmed", err)
	}
}

// Requests of one envelope to one subject under its ForwardMsgID leave the stream one copy however
// often they are sent, the repeat's duplicate acknowledgement still answering it; the same envelope
// to another subject is a copy of its own there.
func TestRequestsUnderOneMsgIDLeaveTheStreamOneCopy(t *testing.T) {
	_, _, client := requestClient(t)
	window := client.WindowUntil(nil)
	item := forwardEnvelope("once")
	holder := contracts.LegacyAgentSubject("ses_request_once")
	other := contracts.LegacyAgentSubject("ses_request_other")

	for attempt := 1; attempt <= 2; attempt++ {
		opts := bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, holder)}
		if err := answerOf(t, window.Request(holder, item, opts)); err != nil {
			t.Fatalf("attempt %d returned %v, want nil", attempt, err)
		}
	}
	opts := bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, other)}
	if err := answerOf(t, window.Request(other, item, opts)); err != nil {
		t.Fatalf("the Request to the second subject returned %v, want nil", err)
	}

	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: "notifications.agent.>"})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[holder]; got != 1 {
		t.Errorf("the stream holds %d copies on %s after two Requests, want 1", got, holder)
	}
	if got := info.State.Subjects[other]; got != 1 {
		t.Errorf("the stream holds %d copies on %s, want 1", got, other)
	}
}

// A Request whose connection is lost before an answer answers ErrUnavailable once the loss is
// seen, without waiting out its timeout, and a Request after it answers ErrUnavailable before
// sending anything.
//
// The request's flush is held until the client has lost the connection (WithBeforeRequestFlush),
// which is the interleaving a server that goes away right behind the publish has: nats.go clears
// its pending flushes as the connection turns reconnecting, so a flush begun after that waits out
// its whole timeout for a PONG no server sends. The loss is the answer either way, and it is given
// the moment the loss is seen. The request's own timeout is a timer on the client's clock, which
// stands still here, so an answer that comes is the loss and never a timeout.
func TestARequestWhoseConnectionIsLostAnswersUnavailable(t *testing.T) {
	lost := make(chan struct{})
	clock := requestClock()
	server, _, client := requestClient(t, bus.WithClock(clock), bus.WithBeforeRequestFlush(func() {
		<-lost
	}))
	window := client.WindowUntil(nil)

	pending := window.Request("notifications.role.request-lost", forwardEnvelope("lost"), bus.RequestOptions{MsgID: bus.ForwardMsgID("lost", "notifications.role.request-lost")})
	server.Stop(t)
	testwait.Eventually(t, "the client to see the stopped server's connection lost", func() bool {
		return !client.Connected()
	})
	close(lost)
	if err := answerOf(t, pending); !errors.Is(err, bus.ErrUnavailable) || errors.Is(err, bus.ErrNotAcknowledged) {
		t.Fatalf("a Request whose connection was lost returned %v, want ErrUnavailable", err)
	}

	select {
	case err := <-window.Request("notifications.role.request-lost", forwardEnvelope("after"), bus.RequestOptions{MsgID: bus.ForwardMsgID("after", "notifications.role.request-lost")}):
		if !errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("a Request on a lost window answered %v, want ErrUnavailable", err)
		}
	default:
		t.Fatal("a Request on a lost window returned with no answer, as if it had sent something")
	}
}

// A Request to a subject NATS does not accept is refused before anything is sent.
func TestARequestToASubjectNATSRefusesIsRefusedAtOnce(t *testing.T) {
	_, _, client := requestClient(t)
	select {
	case err := <-client.WindowUntil(nil).Request("notifications.agent..bad", forwardEnvelope("bad"), bus.RequestOptions{MsgID: bus.ForwardMsgID("bad", "notifications.agent..bad")}):
		if !errors.Is(err, bus.ErrRefused) {
			t.Fatalf("a Request to an invalid subject answered %v, want ErrRefused", err)
		}
	default:
		t.Fatal("a Request to an invalid subject returned with no answer")
	}
}

// A Request the stream refuses is answered by that refusal the moment it comes back, not after
// the request's timeout: the stream answered, so a full stream's refusal reaches the caller as
// bus.ErrStreamFull, as it does through a window's PublishAsync, and never as
// bus.ErrNotAcknowledged, which says the stream gave no answer at all. Dispatch's outbox waits
// roleForwardAckTimeout for a role forward's answer, so a refusal left to that timeout holds the
// forward's whole chain for five seconds and is counted as a copy that might yet exist.
//
// The timeout is a timer on the client's clock, which this row never moves: an answer that comes
// is the refusal, and a request that waits its timeout out - the defect this row is for - is never
// answered at all.
func TestARequestAFullStreamRefusesIsAnsweredByThatRefusalAtOnce(t *testing.T) {
	const limit = 16 << 10
	clock := requestClock()
	_, _, client := requestClient(t, bus.WithStreamMaxBytes(limit), bus.WithClock(clock))
	bustest.FillStream(t, client, 1024)

	holder := contracts.LegacyAgentSubject("ses_request_full")
	item := forwardEnvelope("full")
	err := answerOf(t, client.WindowUntil(nil).Request(holder, item,
		bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, holder)}))
	if !errors.Is(err, bus.ErrStreamFull) || errors.Is(err, bus.ErrNotAcknowledged) || errors.Is(err, bus.ErrRefused) {
		t.Fatalf("a Request the full stream refused = %v, want bus.ErrStreamFull and neither bus.ErrNotAcknowledged nor bus.ErrRefused", err)
	}
}

// Many Requests in flight on one window, sent from several goroutines while others publish on it,
// are each answered by the stream's acknowledgement of their own copy.
func TestManyRequestsInFlightOnOneWindowEachTakeTheirOwnAnswer(t *testing.T) {
	const senders = 20
	_, _, client := requestClient(t)
	subject := contracts.LegacyAgentSubject("ses_request_many")
	window := client.WindowUntil(nil)

	var wg sync.WaitGroup
	failures := make(chan error, 2*senders)
	for index := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("many-%d", index)
			request := window.Request(subject, forwardEnvelope(name), bus.RequestOptions{MsgID: bus.ForwardMsgID(name, subject)})
			publish := window.PublishAsync(numberedEnvelope(orderedStreamTopic, index))
			if err := <-request; err != nil {
				failures <- fmt.Errorf("request %s: %w", name, err)
			}
			if err := <-publish; err != nil {
				failures <- fmt.Errorf("publish %d: %w", index, err)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

// Probe answers whether the window's connection still answers: nil while its server answers a
// flush, ErrUnavailable when the flush runs out of time on a connection that stays connected (a
// silent server), and ErrUnavailable once the connection is lost.
//
// Probe's bound is its caller's, and the flush it bounds is nats.go's own wait on the wire, which
// no clock of this package reaches: the durations below are the ones under test, passed in as a
// caller passes them.
func TestProbeAnswersWhetherTheWindowsConnectionStillAnswers(t *testing.T) {
	server, _, client := requestClient(t)
	window := client.WindowUntil(nil)

	if err := window.Probe(time.Second); err != nil {
		t.Fatalf("a Probe of a live connection returned %v, want nil", err)
	}

	thaw := server.Freeze(t)
	err := window.Probe(200 * time.Millisecond)
	thaw()
	if !errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a Probe of a silent server returned %v, want ErrUnavailable", err)
	}
	if !client.Connected() {
		t.Fatal("the client lost its connection while frozen; the Probe proves nothing about a silent server")
	}
	if err := window.Probe(time.Second); err != nil {
		t.Fatalf("a Probe once the server answers again returned %v, want nil", err)
	}

	server.Stop(t)
	testwait.Eventually(t, "the client to see the stopped server's connection lost", func() bool {
		return !client.Connected()
	})
	if err := window.Probe(200 * time.Millisecond); !errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a Probe of a lost connection returned %v, want ErrUnavailable", err)
	}
}

// A window with a deadline (WindowUntil) writes nothing at or past it. Its first send, made while
// the client has no connection, waits for one only until the deadline - a moment on the client's
// clock, which this row moves to once the send has armed its wait - and answers ErrWindowDeadline
// then, though the connection comes back afterwards: the stream holds nothing it sent once the
// client is connected again. A window pinned to a live connection answers a send past its deadline
// the same way, at once.
func TestAWindowSendsNothingPastItsDeadline(t *testing.T) {
	clock := requestClock()
	server, _, client := requestClient(t, bus.WithClock(clock))
	subject := contracts.LegacyAgentSubject("ses_window_deadline")
	server.Stop(t)
	testwait.Eventually(t, "the client to see the stopped server's connection lost", func() bool {
		return !client.Connected()
	})

	deadline := clock.Now().Add(200 * time.Millisecond)
	window := client.WindowUntil(func() time.Time { return deadline })
	item := forwardEnvelope("deadline")
	item.Topic = subject
	// PublishAsync pins the window to a connection before it returns, and that pin is the wait
	// here: the client has no connection, so the first send runs on a goroutine of its own, and the
	// test moves the clock to the deadline the pin's wait is armed at.
	sent := make(chan error, 1)
	go func() { sent <- <-window.PublishAsync(item) }()
	awaitArmedAt(t, clock, "the send's wait for a connection, at the window's deadline", deadline.Equal)
	clock.Set(deadline)
	if err := testwait.Receive(t, "the send whose wait for a connection met the window's deadline", sent); !errors.Is(err, bus.ErrWindowDeadline) || errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a send whose wait for a connection met the window's deadline answered %v, want ErrWindowDeadline and not ErrUnavailable", err)
	}
	err := answerOf(t, window.Request(subject, forwardEnvelope("deadline-request"), bus.RequestOptions{MsgID: bus.ForwardMsgID("deadline-request", subject)}))
	if !errors.Is(err, bus.ErrWindowDeadline) {
		t.Fatalf("a Request past the window's deadline answered %v, want ErrWindowDeadline", err)
	}

	server.Start(t)
	testwait.Eventually(t, "the client to reconnect to its restarted server", client.Connected)
	// A send the window had held until the connection came back would be written when it came
	// back, and so before the marker this window writes now, which the stream acknowledges.
	pinned := clock.Now().Add(time.Hour)
	live := client.WindowUntil(func() time.Time { return pinned })
	marker := forwardEnvelope("deadline-marker")
	marker.Topic = contracts.LegacyAgentSubject("ses_window_marker")
	if err := answerOf(t, live.PublishAsync(marker)); err != nil {
		t.Fatalf("the marker after the reconnect answered %v, want nil", err)
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[subject]; got != 0 {
		t.Fatalf("the stream holds %d messages the window sent past its deadline, want none", got)
	}

	if err := answerOf(t, live.PublishAsync(item)); err != nil {
		t.Fatalf("a send before the window's deadline answered %v, want nil", err)
	}
	pinned = clock.Now()
	if err := answerOf(t, live.PublishAsync(item)); !errors.Is(err, bus.ErrWindowDeadline) {
		t.Fatalf("a send of a pinned window past its deadline answered %v, want ErrWindowDeadline", err)
	}
}
