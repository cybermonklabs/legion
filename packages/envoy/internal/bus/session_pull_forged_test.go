package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
// whose grant publishes every _INBOX. subject can, and records each publish the server refuses
// it for its reply subject, which reaches the client only as an asynchronous error.
type forger struct {
	conn    *nats.Conn
	mu      sync.Mutex
	refused []string
}

func newForger(t *testing.T) *forger {
	t.Helper()
	f := &forger{}
	conn, err := nats.Connect(testnats.URL(t), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if strings.Contains(err.Error(), "Permissions Violation for Publish with Reply of") {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.refused = append(f.refused, err.Error())
		}
	}))
	if err != nil {
		t.Fatalf("connect the forger: %v", err)
	}
	t.Cleanup(conn.Close)
	f.conn = conn
	return f
}

// publish publishes data into inbox with reply as its reply subject and returns once the server
// has read it.
func (f *forger) publish(t *testing.T, inbox, reply, data string) {
	t.Helper()
	if err := f.conn.PublishMsg(&nats.Msg{Subject: inbox, Reply: reply, Data: []byte(data)}); err != nil {
		t.Fatalf("publish into %s: %v", inbox, err)
	}
	if err := f.conn.Flush(); err != nil {
		t.Fatalf("flush the forger: %v", err)
	}
}

func (f *forger) refusals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refused)
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
	pull := pullSession(t, consumers, session)
	forger := newForger(t)
	// The forged message is read by the server before the stored one is published, and the
	// server writes both to the pull's connection in that order.
	forger.publish(t, pull.inbox, "_INBOX.forged", "forged")
	stored := publishStored(t, owner, agent, "stored")

	msg := next(t, pull)
	if msg.StreamSeq != stored || string(msg.Data) != "stored" {
		t.Fatalf("the pull handed over message %d %q, want the stored message %d", msg.StreamSeq, msg.Data, stored)
	}
	requireOneLine(t, logs, "WARN", pullDroppedLine, session, map[string]any{
		"consumer": "s-" + session, "reply": "_INBOX.forged", "held": uint64(0),
	})

	// A second within the minute is dropped and held back; the next one after the minute is
	// logged and counts it.
	forger.publish(t, pull.inbox, "_INBOX.forged.again", "forged")
	waitUntil(t, "the pull to hold one drop back from its line", func() (bool, string) {
		pull.mu.Lock()
		defer pull.mu.Unlock()
		return pull.droppedHeld == 1, fmt.Sprintf("it holds %d back", pull.droppedHeld)
	})
	if lines := sessionLines(t, logs, pullDroppedLine, session); len(lines) != 1 {
		t.Fatalf("the pull logged %d drops for 2 within a minute, want the first alone: %v", len(lines), lines)
	}
	pull.mu.Lock()
	pull.droppedLoggedAt = pull.droppedLoggedAt.Add(-sessionPullDroppedLogEvery)
	pull.mu.Unlock()
	forger.publish(t, pull.inbox, "_INBOX.forged.third", "forged")
	var lines []map[string]any
	waitUntil(t, "the pull's second line", func() (bool, string) {
		lines = sessionLines(t, logs, pullDroppedLine, session)
		return len(lines) == 2, fmt.Sprintf("it wrote %d", len(lines))
	})
	if lines[1]["reply"] != "_INBOX.forged.third" || lines[1]["held"] != json.Number("1") {
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
	forger.publish(t, pull.inbox, ack, "forged")
	waitUntil(t, "the server to refuse the forger's publish for its reply subject", func() (bool, string) {
		refused := forger.refusals()
		return len(refused) == 1, fmt.Sprintf("it has refused %q", refused)
	})
	t.Logf("the server refused the forger's publish: %q", forger.refusals())
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
