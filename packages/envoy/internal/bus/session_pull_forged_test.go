package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// pullDroppedLine is the line a pull writes for a message on its inbox whose reply is not its
// consumer's acknowledgement subject (SessionPull.receive).
const pullDroppedLine = "session consumer pull dropped a message its consumer did not deliver: its reply is not the consumer's acknowledgement subject"

// forger is a client of the test's account that publishes into a pull's inbox as any connection
// whose grant publishes every _INBOX. subject can.
type forger struct{ conn *nats.Conn }

func newForger(t *testing.T) *forger {
	t.Helper()
	conn, err := nats.Connect(testnats.URL(t))
	if err != nil {
		t.Fatalf("connect the forger: %v", err)
	}
	t.Cleanup(conn.Close)
	return &forger{conn: conn}
}

// publish publishes data into inbox with reply as its reply subject and returns once the server
// has read it. A publish the server refuses for its reply subject is the forger's LastError by
// then: nats.go records the server's -ERR as it reads it, before the PONG that ends the flush.
func (f *forger) publish(t *testing.T, ctx context.Context, inbox, reply, data string) {
	t.Helper()
	if err := f.conn.PublishMsg(&nats.Msg{Subject: inbox, Reply: reply, Data: []byte(data)}); err != nil {
		t.Fatalf("publish into %s: %v", inbox, err)
	}
	if err := f.conn.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush the forger: %v", err)
	}
}

// forgedWaitMargin is what forgedWait leaves of the test binary's deadline for a failed test's
// cleanups and the package's later tests to report.
const forgedWaitMargin = 30 * time.Second

// forgedWait is a context that ends forgedWaitMargin before the test binary's deadline: the
// bound of a wait for something that comes, so a loaded machine only makes it come later, and
// one that never comes fails the test by name. With no deadline (-timeout 0) it ends a day from
// now, which stands for none: nats.go's FlushWithContext refuses a context without a deadline
// ("nats: context requires a deadline").
func forgedWait(t *testing.T) context.Context {
	t.Helper()
	deadline, bounded := t.Deadline()
	if !bounded {
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		t.Cleanup(cancel)
		return ctx
	}
	if time.Until(deadline) <= forgedWaitMargin {
		t.Fatalf("the test binary's deadline leaves %s, not more than forgedWaitMargin (%s): raise -timeout",
			time.Until(deadline).Round(time.Millisecond), forgedWaitMargin)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-forgedWaitMargin))
	t.Cleanup(cancel)
	return ctx
}

// requireSubscribed returns once the server holds the pull's inbox subscription. The pull sends
// it on its own connection (Pull), and nats.go writes it to the server from a goroutine of its
// own, so a message another connection publishes into the inbox before the server has taken it
// has no subscriber, and the server drops it: the pull never sees it. A flush of the pull's
// connection ends with the server's PONG, which the server sends only once it has taken
// everything the connection sent before the PING, the subscription among it.
func requireSubscribed(t *testing.T, ctx context.Context, pull *SessionPull) {
	t.Helper()
	if err := pull.conn.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush the pull's connection, so the server holds its inbox subscription: %v", err)
	}
}

// nextWithin returns the next message pull holds, waiting for it until ctx ends.
func nextWithin(t *testing.T, ctx context.Context, pull *SessionPull) SessionMessage {
	t.Helper()
	msg, err := pull.Next(ctx)
	if err != nil {
		t.Fatalf("next message: %v", err)
	}
	return msg
}

// A message another client publishes into a pull's inbox is not one its consumer delivered: its
// reply is not the consumer's acknowledgement subject ($JS.ACK.<stream>.<consumer>.…), and the
// pull drops it there, so Next hands its caller the consumer's own message after it, and writes
// one WARN line for it, then at most one a minute, each counting the drops held back before it.
//
// Before the pull checked the reply, Next handed the forged message to sessionMessageOf, whose
// read of the delivery from the reply (nats.Msg.Metadata) failed, and Next returned that error:
// a relay restarts its run at the ack floor on it, so any connection that can publish into the
// inbox could restart a session's relay as often as it liked.
func TestAPullDropsAMessageItsConsumerDidNotDeliver(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_inboxed"
	agent := contracts.LegacyAgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	wait := forgedWait(t)
	pull := pullSession(t, consumers, session)
	requireSubscribed(t, wait, pull)
	forger := newForger(t)
	// forgeThenStore publishes a forged message into the pull's inbox and then a stored one, and
	// returns once the pull has handed the stored one over. The server has read the forged message
	// before the stored one is published, and writes both to the pull's connection in that order;
	// the pull's one subscription takes them in that order. So once Next hands the stored one
	// over, the pull has taken the forged one, and what it logged and counted for it is read at
	// once, with no wait of its own.
	forgeThenStore := func(reply, body string) {
		t.Helper()
		forger.publish(t, wait, pull.inbox, reply, "forged")
		stored := publishStored(t, owner, agent, body)
		msg := nextWithin(t, wait, pull)
		if msg.StreamSeq != stored || string(msg.Data) != body {
			t.Fatalf("the pull handed over message %d %q, want the stored message %d %q", msg.StreamSeq, msg.Data, stored, body)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack message %d: %v", stored, err)
		}
	}
	droppedHeld := func() int {
		pull.mu.Lock()
		defer pull.mu.Unlock()
		return pull.droppedHeld
	}

	forgeThenStore("_INBOX.forged", "stored")
	requireOneLine(t, logs, "WARN", pullDroppedLine, session, map[string]any{
		"consumer": "s-" + session, "reply": "_INBOX.forged", "held": uint64(0),
	})

	// A second within the minute is dropped and held back; the next one after the minute is
	// logged and counts it. drop reads the minute off the wall clock (sessionPullDroppedLogEvery),
	// so the first line's time is set to the end of the test's wait: the second drop comes before
	// that end, or nextWithin fails first, so it falls inside the minute however long the machine
	// takes to get there.
	waitEnd, _ := wait.Deadline()
	pull.mu.Lock()
	pull.droppedLoggedAt = waitEnd
	pull.mu.Unlock()
	forgeThenStore("_INBOX.forged.again", "stored again")
	if held := droppedHeld(); held != 1 {
		t.Fatalf("after a second drop within the minute the pull holds %d back from its line, want 1", held)
	}
	if lines := sessionLines(t, logs, pullDroppedLine, session); len(lines) != 1 {
		t.Fatalf("the pull logged %d drops for 2 within a minute, want the first alone: %v", len(lines), lines)
	}
	pull.mu.Lock()
	pull.droppedLoggedAt = time.Now().Add(-sessionPullDroppedLogEvery)
	pull.mu.Unlock()
	forgeThenStore("_INBOX.forged.third", "stored third")
	lines := sessionLines(t, logs, pullDroppedLine, session)
	if len(lines) != 2 || lines[1]["reply"] != "_INBOX.forged.third" || lines[1]["held"] != json.Number("1") {
		t.Fatalf("once a minute had passed the pull logged %v, want a second line for _INBOX.forged.third holding 1", lines)
	}
	requireNoMessage(t, pull, time.Second)
}

// A message in session one's pull inbox whose reply is session two's acknowledgement subject for
// a message session two holds unanswered is not delivered, and so acknowledges nothing: the
// relay of session one never sends it as a frame, and session one's done on that frame cannot
// acknowledge session two's message away.
//
// Over the wire the server refuses it before it reaches the inbox: nats-server refuses a client's
// publish whose reply subject begins $JS.ACK. (server/client.go processInboundClientMsg,
// isReservedReply, in each release read from v2.10.22 to v2.15.3, which spans every one the
// listener runs on), which the first half requires. The second half hands the same message to the
// pull's subscription callback, receive, as a server route that did not refuse it would, and is
// the half the pull's own check stands for: without it, Next returned the message and its Ack
// acknowledged session two's.
func TestAMessageInAPullsInboxNamingAnotherConsumersAckIsNotDeliveredAndAcksNothing(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const one, two = "ses_forging", "ses_forged"
	for _, session := range []string{one, two} {
		if _, err := consumers.Ensure(ctx, session, []string{contracts.LegacyAgentSubject(session)}, 0); err != nil {
			t.Fatalf("ensure %s: %v", session, err)
		}
	}
	seq := publishStored(t, owner, contracts.LegacyAgentSubject(two), "two's")
	held := next(t, pullSession(t, consumers, two))
	if held.StreamSeq != seq {
		t.Fatalf("session two's pull handed over %d, want %d", held.StreamSeq, seq)
	}
	ack := held.SessionAcker.(sessionAck).msg.Reply
	waitForAckPending(t, consumers, two, 1)
	// requireHeld requires session two's consumer to go on holding its message for an ack for half
	// a second: the server takes an ack a moment after the publish that carries it.
	requireHeld := func(when string) {
		t.Helper()
		if err := consumers.client.Connection().Flush(); err != nil {
			t.Fatalf("flush the consumers' connection: %v", err)
		}
		for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			info, err := consumers.Info(ctx, two)
			if err != nil {
				t.Fatalf("info of %s: %v", two, err)
			}
			if info.NumAckPending != 1 || info.AckFloor.Stream >= seq {
				t.Fatalf("%s: session two's consumer holds %d awaiting an ack with its ack floor %d, want message %d still awaiting one: it was acknowledged away", when, info.NumAckPending, info.AckFloor.Stream, seq)
			}
		}
	}
	pull := pullSession(t, consumers, one)
	// The server holds the pull's inbox subscription before the forger publishes, so a publish it
	// let through would reach the pull, and the over-the-wire half below reads a pull that could
	// have received it.
	wait := forgedWait(t)
	requireSubscribed(t, wait, pull)
	// deliveredWithin answers the forged message if Next hands it over within window, after
	// acknowledging it as session one's relay does on session one's done, and nil if the window
	// ends with nothing handed over.
	deliveredWithin := func(window time.Duration) *SessionMessage {
		ctx, cancel := context.WithTimeout(ctx, window)
		defer cancel()
		msg, err := pull.Next(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			t.Fatalf("next message of session one's pull: %v", err)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack the message the pull handed over: %v", err)
		}
		return &msg
	}

	forger := newForger(t)
	forger.publish(t, wait, pull.inbox, ack, "forged")
	refused := forger.conn.LastError()
	if !errors.Is(refused, nats.ErrPermissionViolation) || !strings.Contains(refused.Error(), "Permissions Violation for Publish with Reply of") {
		t.Fatalf("the server answered the forger's publish with reply %s with %v, want its refusal for the reply subject", ack, refused)
	}
	t.Logf("the server refused the forger's publish: %q", refused)
	if msg := deliveredWithin(time.Second); msg != nil {
		t.Errorf("over the wire, session one's pull handed over a message on %s with reply %s", msg.Subject, ack)
	}
	requireHeld("over the wire")

	// The subscription hands its callback each message bound to it, which is what an ack
	// publishes through.
	pull.receive(&nats.Msg{Subject: pull.inbox, Reply: ack, Data: []byte("forged"), Sub: pull.sub})
	if msg := deliveredWithin(time.Second); msg != nil {
		t.Errorf("handed to the pull's inbox, a message whose reply is session two's acknowledgement subject was handed over (%q) and its ack went to %s", msg.Data, ack)
	}
	requireHeld("handed to the pull's inbox")
	requireOneLine(t, logs, "WARN", pullDroppedLine, one, map[string]any{"consumer": "s-" + one, "reply": ack})
}
