package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// Row 1, in one process: a message to S is stored, the owning replica relays it, the socket's
// replica forwards it to the session as a msg frame under the sid it subscribed with, carrying
// the frame's headers and its receipt subject, which is under the owning machine's own receipt
// family (natsgrant.EdgeRelayReplyPrefix) and not a reply inbox, since the edge's grant subscribes
// that family alone; the session answers done over the socket, the socket's replica publishes it
// on that subject, and the owner's consumer acks the message. Its control is the live row's
// (relay_socket_live_test.go): the receipt not published, and the message is still pending.
func TestARelaySocketForwardsAFrameAndPublishesItsReceipt(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_forward"
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)

	stored, seq := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	frame := client.frame("the stored event's frame")
	if frame.Op != "msg" || frame.sid(t) != 1 || frame.Subject != bus.DeliverSubject(session) {
		t.Fatalf("the socket sent %s, want a msg of sid 1 on %s", frame.raw, bus.DeliverSubject(session))
	}
	if owned := natsgrant.EdgeRelayReplyPrefix(rig.owner.relays.machineID) + "."; !strings.HasPrefix(frame.Reply, owned) {
		t.Fatalf("the frame's receipt subject is %q, want one under the owning machine's %s: the edge's grant subscribes no other receipt family and no reply inbox", frame.Reply, owned)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(frame.data(t), &item); err != nil || item.EventID != stored.EventID {
		t.Fatalf("the frame carries %q (%v), want the stored event %s", frame.data(t), err, stored.EventID)
	}
	if got := frame.Headers[relayHeaderSubject]; !slices.Equal(got, []string{contracts.LegacyAgentSubject(session)}) {
		t.Fatalf("the frame's %s header is %q, want the stored subject", relayHeaderSubject, got)
	}

	client.pub(frame.Reply, relayReplyReceived)
	client.pub(frame.Reply, relayReplyDone)
	awaitAcked(t, rig.owner.consumers, session, seq)
}

// A pub on a forwarded frame's receipt subject whose body is no reply word the relay reads
// (received, progress, done, nak) is refused publish_not_granted and published nothing, so a
// client publishes nothing on the receipt family but receipts a word long. That nothing was
// published is read by a marker: the client's done on the same subject, which it pubs next, is the
// first message the subject carries. The socket's reader handles each frame whole before it reads
// the next (relaySocket.read), and both pubs publish on the sockets' one NATS connection, which
// the server delivers in order, so whatever the refused pub published would reach the watcher
// ahead of the done. Control, in the same row: the same subject with done is published, and the
// owner acks the message.
func TestARelaySocketPublishesOnlyAReceiptsWord(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_receipt_body"
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	_, seq := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	frame := client.frame("the stored event's frame")
	if frame.Op != "msg" {
		t.Fatalf("the socket sent %s, want a msg", frame.raw)
	}
	watcher := testnats.Connect(t, rig.owner.url)
	heard := make(chan *natsgo.Msg, 2)
	if _, err := watcher.ChanSubscribe(frame.Reply, heard); err != nil {
		t.Fatalf("watch %s: %v", frame.Reply, err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	client.pub(frame.Reply, strings.Repeat("not a receipt ", 4096))
	if got := client.frame("the answer to a pub whose body is no reply word"); got.Op != "err" || got.Code != relayErrPublishNotGranted {
		t.Fatalf("a pub of a body that is no reply word was answered %s, want err %s", got.raw, relayErrPublishNotGranted)
	}
	client.pub(frame.Reply, relayReplyDone)
	if msg := testwait.Receive(t, "the client's done on "+frame.Reply+", after the refused pub", heard); string(msg.Data) != relayReplyDone {
		t.Fatalf("the replica published %d bytes on %s ahead of the client's done, which is no receipt", len(msg.Data), frame.Reply)
	}
	awaitAcked(t, rig.owner.consumers, session, seq)
}

// A forwarded frame's receipt subject may be published on for relaySocketReplyWindow from the
// moment its frame was forwarded, read on the sockets' clock, and not after: with the rig's clock
// at exactly the window's end the socket publishes the client's receipt, and 1 ms later it refuses
// the same receipt publish_not_granted. The socket checks its token once a day, so no check runs
// while the clock moves. Control: the window's test reading < for <=; the receipt at the window's
// end is refused, and the row fails naming the err frame.
func TestARelaySocketTakesAReceiptUntilItsReplyWindowEnds(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(relaySocketPastTheBinary))
	const session = "ses_edge_reply_window"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	sent := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	frame := client.frame("the frame whose receipt the socket remembers")
	if frame.Op != "msg" || frame.Reply != sent.Reply {
		t.Fatalf("the socket sent %s, want the msg %s", frame.raw, sent.Reply)
	}
	// The socket remembered the receipt where the rig's clock stands, which nothing has moved.
	forwarded := rig.clock.Now()
	watcher := testnats.Connect(t, rig.owner.url)
	heard := make(chan *natsgo.Msg, 2)
	if _, err := watcher.ChanSubscribe(frame.Reply, heard); err != nil {
		t.Fatalf("watch %s: %v", frame.Reply, err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}

	rig.clock.Set(forwarded.Add(relaySocketReplyWindow))
	client.pub(frame.Reply, relayReplyReceived)
	testwait.Until(t, "the receipt published at the window's end", func() (string, bool) {
		select {
		case msg := <-heard:
			if string(msg.Data) != relayReplyReceived {
				t.Fatalf("the replica published %q on %s, want the client's %q", msg.Data, frame.Reply, relayReplyReceived)
			}
			return string(msg.Data), true
		default:
		}
		client.requireNothingYet("a receipt at the window's end")
		return "nothing published yet", false
	})

	rig.clock.Set(forwarded.Add(relaySocketReplyWindow + time.Millisecond))
	client.pub(frame.Reply, relayReplyReceived)
	if got := client.frame("the answer to a receipt 1 ms past the window"); got.Op != "err" || got.Code != relayErrPublishNotGranted || got.SID != nil {
		t.Fatalf("a receipt 1 ms past the window was answered %s, want err %s naming no sid", got.raw, relayErrPublishNotGranted)
	}
}

// Row 2, in one process: a sub to another session's relay subject and to the session's own
// agent-stream control subject is answered subject_not_granted under its sid, and the replica
// holds no subscription to either, so a frame published to the other session never reaches this
// socket; a pub to a reply subject the socket never forwarded is publish_not_granted, and nothing
// is published on it. Each absence is read by a marker that comes after it: the socket's reader
// handles each frame whole before it reads the next (relaySocket.read), so a sub's answer is on the
// wire ahead of the pong of a ping sent after the sub, and what a pub publishes goes out on the
// sockets' one NATS connection ahead of what the next pub publishes; and a frame of the other
// session's, published ahead of one of the session's own on one connection, would reach the client
// first, since the socket holds one subscription (relaySocketMaxSubscriptions), which hands its
// frames on in the order the server routed them. Control: the subject check removed; the replica
// takes the first sub, which is answered nothing ahead of the pong, and the row fails naming it.
func TestARelaySocketRefusesASubjectAndAReplyItWasNotGranted(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session, other = "ses_edge_refused", "ses_someone_else"
	client, _ := rig.open(t, session)
	held := rig.client.Connection().NumSubscriptions()

	// refused sends a sub of subject as sid and a ping after it, and requires sid's err ahead of
	// the ping's pong: a sub answered nothing by then is one the replica took.
	refused := func(sid int, subject string) {
		t.Helper()
		client.sub(sid, subject)
		client.ping()
		testwait.Receive(t, "the pong of a ping sent after the sub of "+subject, client.pongs)
		select {
		case frame := <-client.frames:
			if frame.Op != "err" || frame.Code != relayErrSubjectNotGranted || frame.sid(t) != int64(sid) {
				t.Fatalf("a sub to %s was answered %s, want err %s for sid %d", subject, frame.raw, relayErrSubjectNotGranted, sid)
			}
		default:
			t.Errorf("a sub to %s was answered nothing ahead of the pong of a ping sent after it, want err %s for sid %d: the replica took it", subject, relayErrSubjectNotGranted, sid)
		}
	}
	refused(1, bus.DeliverSubject(other))
	refused(2, "agentstream."+session+".control")
	if now := rig.client.Connection().NumSubscriptions(); now != held {
		t.Errorf("the replica holds %d subscriptions after two refused subs, want the %d it held before", now, held)
	}

	client.subscribeOwn(session)
	publisher := testnats.Connect(t, rig.owner.url)
	// Both frames name an event of the token's project, which the edge's rule passes to this
	// socket (edgeFrameVerdict), and a receipt of this session's, which its receipt check passes
	// (relayReceiptOf), so only the socket's subscriptions keep the other session's frame away.
	header := natsgo.Header{relayHeaderSubject: {"notifications.dispatch.issue." + relaySocketProject + "." + relaySocketProject + "-1.comment.created"}}
	stray := &natsgo.Msg{Subject: bus.DeliverSubject(other), Reply: relayFrameReceipt(session), Data: []byte(`{"stray":true}`), Header: header}
	own := &natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: relayFrameReceipt(session), Data: []byte(`{"own":true}`), Header: header}
	for _, msg := range []*natsgo.Msg{stray, own} {
		if err := publisher.PublishMsg(msg); err != nil {
			t.Fatalf("publish a frame on %s: %v", msg.Subject, err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the frames: %v", err)
	}
	if frame := client.frame("the first frame after the other session's"); frame.Reply != own.Reply {
		if frame.Reply == stray.Reply {
			t.Fatalf("the replica subscribed %s for this socket: the other session's frame reached it as %s", bus.DeliverSubject(other), frame.raw)
		}
		t.Fatalf("the socket sent %s, want the msg %s of its own session", frame.raw, own.Reply)
	}
	if t.Failed() {
		return
	}

	// The marker of the pub on never is the client's done on its own frame's receipt, which it
	// pubs next: the watcher's subscriptions of the two subjects share one channel, which its
	// connection fills in the order the server delivers to it.
	watcher := testnats.Connect(t, rig.owner.url)
	never := bus.RelayReplyPrefix + "never.forwarded"
	heard := make(chan *natsgo.Msg, 4)
	for _, subject := range []string{never, own.Reply} {
		if _, err := watcher.ChanSubscribe(subject, heard); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	client.pub(never, relayReplyDone)
	if frame := client.frame("the answer to a pub to a reply never forwarded"); frame.Op != "err" || frame.Code != relayErrPublishNotGranted || frame.SID != nil {
		t.Fatalf("a pub to a reply never forwarded was answered %s, want err %s naming no sid", frame.raw, relayErrPublishNotGranted)
	}
	client.pub(own.Reply, relayReplyDone)
	if msg := testwait.Receive(t, "the client's done on its own frame's receipt, after the pub on "+never, heard); msg.Subject != own.Reply {
		t.Fatalf("the replica published %q on %s, which it never forwarded", msg.Data, msg.Subject)
	}
}

// Row 3, in one process: a send to S probes S's relay subject (probeSessionConnection) while the
// socket's replica subscribes it; the probe reads connected, since a subscriber that answers
// nothing reads connected, and the session's client receives no probe frame. The probe's reply is
// the prober's own inbox, which is no relay receipt, so two filters of forward each keep it from
// the client: the probe's header, and the stray reply. A message carrying the probe's header with
// a relay receipt's reply, which only the header's filter stops, is not forwarded either.
// Controls: the header's filter removed, the second message is forwarded and the row fails naming
// it; the header's and the stray reply's filters removed together, the probe is forwarded too.
// (Either filter alone removed passes the probe's half: the other stops it.) The row's second half
// is its own control: the socket closed first, the replica holds no subscription, and the same
// probe reads stopped.
func TestAProbeOfASocketIsNeitherForwardedNorAnswered(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_probed"
	client, _ := rig.open(t, session)
	held := rig.client.Connection().NumSubscriptions()
	client.subscribeOwn(session)

	prober := rig.client
	logger := logging.NewWithWriter("prober", &bytes.Buffer{})
	if !probeSessionConnection(t.Context(), prober, session, logger) {
		t.Fatal("the probe of a session whose socket subscribes its relay subject read stopped, want connected")
	}
	client.none(500*time.Millisecond, "the connection probe")
	publishRelayFrame(t, rig.owner.url, session, natsgo.Header{bus.ConnectionProbeHeader: []string{"1"}})
	client.none(500*time.Millisecond, "a message carrying the probe's header under a relay receipt")

	_ = client.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	waitFor(t, "the replica to drop the socket's subscription", func() bool {
		return rig.client.Connection().NumSubscriptions() == held
	})
	if probeSessionConnection(t.Context(), prober, session, logger) {
		t.Fatal("the probe of a session whose socket closed read connected, want stopped")
	}
}

// Rows 4, 5, 7, 8, 9 and 10, in one process, each on its own socket: the broker's answer for the
// socket's token changes, and the socket closes with the code the wire gives that case at the first
// check that reads it, on the rig's clock: the first check past the five seconds the answer its
// upgrade cached is reused for, where the row leaves the clock, or, for a token whose exp passed, the
// check at that exp. Before the change five checks keep the socket, each one witnessed by the ping
// its verdict sends; a check before the cached answer's end reads that answer, and keeps it too. The
// last two cases are the negative controls of all of them: an answer that has not changed, and one
// whose not-held deadline is still ahead, keep the socket open at the first check that asks the
// broker afresh (the broker's count of answers grows, and that check's ping comes) and at the ten
// checks after it. Each row's own control is the check of its case removed, and the socket stays
// open. "its token's exp passed" has the broker answer the token not live before the clock reaches
// its exp, so the close rests on the verdict reading that answer at the exp as the expiry; "its
// token was revoked" is its control, the same answer read before the exp, a refusal.
func TestAnOpenSocketClosesByTheBrokersAnswer(t *testing.T) {
	t.Parallel()
	type outcome struct {
		code int
		text string
	}
	expired := outcome{relaySocketCloseExpired, relaySocketReasonExpired}
	refused := outcome{relaySocketCloseRefused, relaySocketReasonRefused}
	for _, row := range []struct {
		name  string
		life  time.Duration
		later func(rig *relaySocketRig, session, token string)
		// atExpiry is whether the check that reads the change is the one at the token's exp,
		// rather than the first past the cached answer.
		atExpiry bool
		want     *outcome
	}{
		{name: "its token's exp passed", life: 2 * time.Second, atExpiry: true, want: &expired,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, ciphertoken.Introspection{})
			}},
		{name: "its token was revoked", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, ciphertoken.Introspection{})
			}},
		{name: "its answer binds no key", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
				answer.Thumbprint = ""
				rig.broker.Says(token, answer)
			}},
		{name: "its projects became another", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketOtherProject))
			}},
		{name: "its projects grew", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketOtherProject, relaySocketProject))
			}},
		{name: "its session changed", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, relaySocketAnswer("ses_another", rig.caller.JKT(), time.Hour, relaySocketProject))
			}},
		{name: "its person became held", life: time.Hour, want: &outcome{relaySocketCloseRefused, relaySocketReasonMembershipHeld},
			later: func(rig *relaySocketRig, session, token string) {
				answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
				answer.Held = true
				rig.broker.Says(token, answer)
			}},
		{name: "its not-held answer's deadline passed", life: time.Hour, want: &outcome{websocket.CloseTryAgainLater, relaySocketReasonDeadlinePassed},
			later: func(rig *relaySocketRig, session, token string) {
				answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
				answer.NotHeldUntil = oauthwire.NotHeldUntilPassed
				rig.broker.Says(token, answer)
			}},
		{name: "the broker cannot be asked", life: time.Hour, want: &outcome{websocket.CloseTryAgainLater, relaySocketReasonBrokerUnavailable},
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Server.Close()
			}},
		{name: "its answer stands", life: time.Hour,
			later: func(rig *relaySocketRig, session, token string) {}},
		{name: "its not-held answer's deadline is ahead", life: time.Hour,
			later: func(rig *relaySocketRig, session, token string) {
				answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
				answer.NotHeldUntil = rig.clock.Now().Add(time.Hour).Unix()
				rig.broker.Says(token, answer)
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rig := newRelaySocketRig(t, withPongFloor(relaySocketPastTheBinary))
			const session = "ses_edge_checked"
			token := "access-token-of-a-checked-socket"
			answer := relaySocketAnswer(session, rig.caller.JKT(), row.life, relaySocketProject)
			// The exp is dated on the rig's clock, which the answer's cache and the socket read, so a
			// short-lived token's exp falls inside the five seconds its upgrade's answer is reused
			// for, however long the rig took to build.
			answer.ExpiresAt = rig.clock.Now().Add(row.life).Unix()
			rig.broker.Says(token, answer)
			client := rig.openWith(t, session, token)
			checks := rig.checksOf(t, client, session)
			for range 5 {
				checks.keep(t, "a check before the answer changed")
			}

			row.later(rig, session, token)
			at := checks.atOrAfter(checks.origin.Add(ciphertoken.AffirmativeCacheTTL))
			if row.atExpiry {
				at = time.Unix(answer.ExpiresAt, 0)
			}
			checks.skipTo(t, at, "the checks the cached answer serves")
			if row.want == nil {
				checks.keepFresh(t, "the first check past the cached answer")
				for range 10 {
					checks.keep(t, "a check after the answer was read afresh")
				}
				return
			}
			code, text := checks.closeAt(t, at, "the close at the first check that reads the changed answer")
			if code != row.want.code || text != row.want.text {
				t.Fatalf("the socket closed %d %q, want %d %q\n%s", code, text, row.want.code, row.want.text, rig.logs.String())
			}
		})
	}
}

// The upgrade's refusals, each with its control: a request of another method is refused 405 and
// one presenting no broker token 401, each by serve itself, since the rig's apiAuth lets a request
// with no token through: the second offers no subprotocol either, so it is refused 401 before the
// subprotocol's 400, which only serve's own check of the caller does; a token naming another
// session is refused 403 before the upgrade, with
// sessionNotBound's line (control: the token's own session opens); a client that offers no
// envoy-relay.v1 is refused 400; an upgrade whose Origin is another host's is refused 403, as a
// page of another origin would send (control: an Origin of the listener's own host opens); a fifth
// socket of one session is refused 429 (control: the fourth opens); and a socket the listener
// closes at its stop is closed 1012, and an upgrade after the stop is refused 503 by serve.
func TestARelaySocketUpgradeIsRefusedAsTheWireSays(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session, other = "ses_edge_upgrade", "ses_not_the_tokens"
	token := "access-token-of-the-upgrading-session"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))

	posted, err := http.Post(rig.server.URL+relaySocketPath(session), "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", relaySocketPath(session), err)
	}
	_ = posted.Body.Close()
	if posted.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST %s = %d, want 405", relaySocketPath(session), posted.StatusCode)
	}
	if _, response, err := rig.dialAs(t, session, http.Header{}); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an upgrade presenting no broker token = %v (%s), want 401", err, describeUpgrade(response))
	}

	_, response, err := rig.dialAs(t, other, rig.upgradeHeader(t, other, token), relaySubprotocol)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("an upgrade of %s with %s's token = %v (%s), want 403", other, session, err, describeUpgrade(response))
	}
	if logged := rig.logs.String(); !strings.Contains(logged, "listener: broker token acts as another session") || !strings.Contains(logged, other) {
		t.Fatalf("the refusal wrote no sessionNotBound line naming %s:\n%s", other, logged)
	}

	if _, response, err := rig.dialAs(t, session, rig.upgradeHeader(t, session, token)); err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an upgrade offering no subprotocol = %v (%s), want 400", err, describeUpgrade(response))
	}

	foreign := rig.upgradeHeader(t, session, token)
	foreign.Set("Origin", "https://elsewhere.example")
	if _, response, err := rig.dialAs(t, session, foreign, relaySubprotocol); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("an upgrade whose Origin is another host's = %v (%s), want 403", err, describeUpgrade(response))
	}
	own := rig.upgradeHeader(t, session, token)
	own.Set("Origin", rig.server.URL)
	first, response, err := rig.dialAs(t, session, own, relaySubprotocol)
	if err != nil {
		t.Fatalf("an upgrade whose Origin is the listener's own host = %v (%s), want it opened", err, describeUpgrade(response))
	}
	open := []*relaySocketClient{first}
	for range relaySocketMaxPerSession - 1 {
		open = append(open, rig.openWith(t, session, token))
	}
	if _, response, err := rig.dialAs(t, session, rig.upgradeHeader(t, session, token), relaySubprotocol); err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a fifth socket of %s = %v (%s), want 429", session, err, describeUpgrade(response))
	}

	rig.sockets.stop()
	for _, client := range open {
		if code, text := client.closeNext("the close of a socket the listener's stop closed"); code != websocket.CloseServiceRestart || text != relaySocketReasonStopping {
			t.Fatalf("a socket the listener's stop closed got %d %q, want %d %q", code, text, websocket.CloseServiceRestart, relaySocketReasonStopping)
		}
	}
	if _, response, err := rig.dialAs(t, session, rig.upgradeHeader(t, session, token), relaySubprotocol); err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an upgrade after the listener's stop = %v (%s), want 503", err, describeUpgrade(response))
	}
}

// A session that is not one NATS subject token is refused 400 before the upgrade, whatever its
// token names: its relay subject would be a wildcard or reach under another session's, so a socket
// of the session * would receive every session's relay frames that this replica's connection may
// subscribe. The token of each row names that same session, so only the path's own rule refuses
// it. Control: the rule removed; the socket of * opens, subscribes notifications.envoy.deliver.*,
// and another session's frame reaches it, which the row names.
func TestARelaySocketRefusesASessionThatIsNotOneSubjectToken(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const victim = "ses_edge_victim"
	for _, session := range []string{"*", ">", "ses_edge.dotted"} {
		t.Run(session, func(t *testing.T) {
			token := "access-token-of-the-session-" + session
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
			client, response, err := rig.dialAs(t, session, rig.upgradeHeader(t, session, token), relaySubprotocol)
			if err == nil {
				client.sub(1, bus.DeliverSubject(session))
				client.ping()
				select {
				case <-client.pongs:
				case frame := <-client.frames:
					t.Fatalf("the socket of the session %q was opened, and its sub of %s was answered %s; want 400 before the upgrade", session, bus.DeliverSubject(session), frame.raw)
				case <-time.After(listenerWaitBound(t)):
					t.Fatalf("the socket of the session %q was opened and answered no ping; want 400 before the upgrade", session)
				}
				publishRelayFrame(t, rig.owner.url, victim, natsgo.Header{})
				select {
				case frame := <-client.frames:
					t.Fatalf("the socket of the session %q was opened, and %s's frame reached it: %s", session, victim, frame.raw)
				case <-time.After(listenerWaitBound(t)):
					t.Fatalf("the socket of the session %q was opened; want 400 before the upgrade", session)
				}
			}
			if response == nil || response.StatusCode != http.StatusBadRequest {
				t.Fatalf("an upgrade of the session %q = %v (%s), want 400", session, err, describeUpgrade(response))
			}
		})
	}
	// Control of the rule's reach: a session that is one subject token, on the same rig, opens.
	rig.open(t, "ses_edge_one_token")
}

// A socket whose client reads nothing, with a data frame's write blocked on it, is closed without
// waiting for that write: after a revoke, its connection is dropped once the check that reads the
// revoke has run, and the listener's stop returns. A write blocked on such a client holds the
// connection's write lock until its own deadline, so neither the close nor the watch's ping may
// wait for it. The row holds that write blocked for its whole run: its deadline (withWriteWait) and
// the reader's (pongWait, three check intervals, with the check a day away) are both past the test
// binary's, so no deadline but the close frame's own ends anything. With the token still live, the
// row moves the rig's clock to the socket's first check, whose ping waits behind the blocked write,
// and waits for that ping to be pending; only then does it close the socket. The close frame,
// behind the same write, is not written by its deadline, which the row reads through the sockets'
// writeControl and requires to be the rig's grace past the wall-clock moment the close read,
// exactly: the grace is short (withCloseGrace), so the write fails soon, and a close frame that
// could not be written drops the connection at once, with no grace's callback armed. No span is
// asserted: the row waits for the stop to return and for the replica to release the socket, each
// bounded only by the test binary's deadline. Controls: end taking the data writers' lock, or the
// watch writing its ping in line; the close's write, the stop's return and the release never come,
// and the row fails naming the one it waited for. The close passing its write a deadline ten
// seconds later; the row fails naming the deadline.
func TestARelaySocketWhoseClientReadsNothingClosesWithinTheGrace(t *testing.T) {
	t.Parallel()
	// closeGrace is the rig's grace: the close frame's write deadline past the moment the close
	// read, short, since that write waits behind the blocked data write and fails at it. No verdict
	// reads how soon.
	const closeGrace = 100 * time.Millisecond
	for _, row := range []struct {
		name string
		// close closes the socket, whose first check ran at first, and returns the code and the
		// reason of the close.
		close func(t *testing.T, rig *relaySocketRig, token string, first time.Time) (int, string)
	}{
		{name: "a revoke", close: func(t *testing.T, rig *relaySocketRig, token string, first time.Time) (int, string) {
			rig.revoke(token)
			second := first.Add(relaySocketPastTheBinary)
			rig.clock.AwaitArmedAt(t, "the socket's second check, which reads the revoke", second)
			rig.clock.Set(second)
			return relaySocketCloseRefused, relaySocketReasonRefused
		}},
		{name: "the listener's stop", close: func(t *testing.T, rig *relaySocketRig, _ string, _ time.Time) (int, string) {
			testwait.Returns(t, "the listener's stop, with a data write blocked on a client that reads nothing", rig.sockets.stop)
			return websocket.CloseServiceRestart, relaySocketReasonStopping
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			closes := newCloseWrites(nil)
			// Limits that hold every frame, so no slow consumer closes the socket first.
			rig := newRelaySocketRig(t, withPendingLimits(1<<16, 1<<30), withWriteWait(relaySocketPastTheBinary),
				withCheckEvery(relaySocketPastTheBinary), withCloseGrace(closeGrace), withCloseWrites(closes))
			const session = "ses_edge_reads_nothing"
			token := "access-token-of-a-client-that-reads-nothing"
			// The token outlives the second check, so no check reads it expired.
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 3*relaySocketPastTheBinary, relaySocketProject))
			// The socket's checks fall one interval apart from where the rig's clock stands as its
			// watch begins, and the clock stands still until the row moves it.
			first := rig.clock.Now().Add(relaySocketPastTheBinary)
			// The client sends its sub and reads nothing from here on, and far more than the
			// connection's buffers hold is published: the socket's write blocks, and the rest wait
			// in its queue.
			_, socket := rig.dialStalled(t, session, token)
			flood(t, rig.owner.url, session, 60)
			awaitBlockedWrite(t, socket)

			rig.clock.AwaitArmedAt(t, "the socket's first check", first)
			rig.clock.Set(first)
			testwait.Until(t, "the first check's ping, pending behind the blocked data write", func() (bool, bool) {
				pinging := socket.pinging.Load()
				return pinging, pinging
			})

			code, reason := row.close(t, rig, token, first)
			write := closes.next(t, "the close frame's write, behind the blocked data write", code, reason)
			if got := write.deadline.Sub(write.origin); got != closeGrace {
				t.Fatalf("the close frame's write was given a deadline %s past the moment the close read, want the rig's grace, %s, exactly", got, closeGrace)
			}
			var timedOut net.Error
			if !errors.As(write.err, &timedOut) || !timedOut.Timeout() {
				t.Fatalf("the close frame's write behind the blocked data write returned %v, want it to fail at its deadline", write.err)
			}
			testwait.Until(t, "the replica to release the socket whose close frame could not be written", func() (int, bool) {
				held := rig.holds(session)
				return held, held == 0
			})
		})
	}
}

// A socket's close runs its grace from the close's start, not from the end of its close frame's
// write: end reads its deadline before the write and, once the frame is written, arms the grace's
// callback on the sockets' clock for only what the write left of it. The client reads the close
// and withholds its answer, and the socket checks its token once a day, so the reader's own
// deadline (pongWait, on the real clock) is past the test binary's: only the grace's callback can
// end the connection. Each row closes the socket on a revoke and reads the close frame's write
// through the sockets' writeControl: the write is given the rig's grace past the wall-clock moment
// the close read, exactly, and the callback is armed one grace past the close's start on the rig's
// clock; one nanosecond short of it the callback is still armed and the connection open, and at it
// the listener drops the connection, which the client reads as EOF. In the first row the close
// frame's write takes half the grace on the rig's clock; the second holds the deployed grace
// (relaySocketCloseGrace, 1 s), with no withCloseGrace. No grace passes on the real clock.
// Controls: the callback left on the wall clock's time.AfterFunc, which arms nothing on the rig's
// clock; each row fails waiting for the callback armed at the close's start plus one grace. The
// callback armed for a full grace once the write is done, at the close's start plus one and a half
// graces in the first row; that row fails at the same wait. newRelaySockets setting another grace;
// the second row fails naming the deadline.
func TestARelaySocketsCloseGraceRunsFromTheClosesStart(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		options []relaySocketRigOption
		// during runs as the close frame's write begins, on the rig's clock.
		during func(clock *clocktest.Fake)
		grace  time.Duration
	}{
		// An hour: the close frame's own write deadline on the real clock, which a write to a
		// client that reads stays well inside.
		{name: "a grace its close frame's write takes half of", options: []relaySocketRigOption{withCloseGrace(time.Hour)},
			during: func(clock *clocktest.Fake) { clock.Advance(time.Hour / 2) }, grace: time.Hour},
		{name: "the deployed grace", grace: relaySocketCloseGrace},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			closes := newCloseWrites(row.during)
			rig := newRelaySocketRig(t, append([]relaySocketRigOption{withCheckEvery(relaySocketPastTheBinary), withCloseWrites(closes)}, row.options...)...)
			const session = "ses_edge_close_grace"
			token := "access-token-of-a-socket-whose-close-is-not-answered"
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 2*relaySocketPastTheBinary, relaySocketProject))
			client := rig.openWithholdingItsAnswer(t, session, token)
			checks := rig.checksOf(t, client, session)
			rig.revoke(token)
			if code, text := checks.closeAt(t, checks.next, "the close at the first check after the revoke"); code != relaySocketCloseRefused || text != relaySocketReasonRefused {
				t.Fatalf("the socket closed %d %q, want %d %q", code, text, relaySocketCloseRefused, relaySocketReasonRefused)
			}
			write := closes.next(t, "the close frame's write", relaySocketCloseRefused, relaySocketReasonRefused)
			if write.err != nil {
				t.Fatalf("the close frame's write to a client that reads failed: %v", write.err)
			}
			if got := write.deadline.Sub(write.origin); got != row.grace {
				t.Fatalf("the close frame's write was given a deadline %s past the moment the close read, want the grace, %s, exactly", got, row.grace)
			}
			graceEnds := write.began.Add(row.grace)
			rig.clock.AwaitArmedAt(t, "the close's grace, one grace past the close's start", graceEnds)
			raw := client.conn.NetConn()
			ended := make(chan error, 1)
			go func() {
				_, err := raw.Read(make([]byte, 1))
				ended <- err
			}()
			rig.clock.Set(graceEnds.Add(-time.Nanosecond))
			if armed := rig.clock.Armed(); !slices.ContainsFunc(armed, graceEnds.Equal) {
				t.Fatalf("one nanosecond short of the grace's end %v the clock holds the timers %v: the grace's callback is gone", graceEnds, armed)
			}
			if held := rig.holds(session); held != 1 {
				t.Fatalf("one nanosecond short of the grace's end the replica holds %d sockets of %s, want the one whose close is not answered", held, session)
			}
			select {
			case err := <-ended:
				t.Fatalf("one nanosecond short of the grace's end the connection ended (%v), want it open", err)
			default:
			}
			rig.clock.Set(graceEnds)
			if err := testwait.Receive(t, "the listener to drop the connection at the grace's end", ended); !errors.Is(err, io.EOF) {
				t.Fatalf("the connection at the grace's end read %v, want the listener's end of it (EOF)", err)
			}
		})
	}
}

// A data write that fails - one that ran out its write deadline on a client whose window stayed
// shut - ends the socket and drops its connection at once, though the client goes on sending
// frames that would keep its read deadline from running out. The write's deadline is the rig's
// write wait past the moment the write began, exactly, which the row reads through the sockets'
// writeDeadline; the wait is short (withWriteWait), so the write fails soon, and no verdict reads
// how soon. The row waits for the line the pump logs on the failure, naming the session and the
// deadline's timeout, and then for the replica to release the socket, each bounded only by the test
// binary's deadline. "At once" is the order of the events: the rig's clock is never moved, so no
// check, no token expiry and no close's grace ran, and no close frame was written; the failure
// alone ended the socket. Its limits hold every frame, so no slow consumer closes it either.
// Controls: end of code 0 closing nothing; the connection stays as long as the client sends, and
// the row fails naming the release it waited for. write setting twice its wait as its deadline; the
// row fails naming the deadline.
func TestARelaySocketWhoseWriteFailsIsDroppedAtOnce(t *testing.T) {
	t.Parallel()
	// writeWait is the rig's data write deadline past the moment a write begins: short, so the
	// write blocked on the client fails soon.
	const writeWait = 100 * time.Millisecond
	deadlines := &dataWriteDeadlines{}
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withWriteWait(writeWait), withWriteDeadlines(deadlines))
	const session = "ses_edge_write_fails"
	token := "access-token-of-a-socket-whose-write-fails"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 2*time.Hour, relaySocketProject))
	conn, _ := rig.dialStalled(t, session, token)
	// The client reads nothing and keeps its read deadline alive: an unsub of a sid it never
	// subscribed is nothing to the socket.
	sending := make(chan struct{})
	t.Cleanup(func() { close(sending) })
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sending:
				return
			case <-ticker.C:
				if conn.WriteJSON(map[string]any{"op": "unsub", "sid": 99}) != nil {
					return
				}
			}
		}
	}()
	flood(t, rig.owner.url, session, 60)
	const failed = "listener: the relay socket could not write a frame; it is closed"
	testwait.Until(t, "the pump's line that the socket's write failed on its deadline, naming "+session, func() (string, bool) {
		lines := edgeSessionLogLines(rig.logs.String(), "WARN", failed, session)
		if len(lines) == 0 {
			return "no such line", false
		}
		text, _ := lines[0]["error"].(string)
		if !strings.Contains(text, os.ErrDeadlineExceeded.Error()) {
			t.Fatalf("the socket's write failed with %q, want its deadline's timeout (%q)", text, os.ErrDeadlineExceeded)
		}
		return text, true
	})
	testwait.Until(t, "the replica to release the socket whose write failed", func() (int, bool) {
		held := rig.holds(session)
		return held, held == 0
	})
	deadlines.requireEach(t, writeWait, "the socket's data writes")
	if closed := edgeSessionLogLines(rig.logs.String(), "INFO", "listener: relay socket closed", session); len(closed) != 0 {
		t.Fatalf("the socket whose write failed was closed with a close frame (%v), want it dropped by the failure alone", closed)
	}
}

// A socket's data write is bound by relaySocketWriteWait, the deployed 10 s, past the moment the
// write began, exactly: a rig with no withWriteWait, whose sockets' writeDeadline reads the
// deadline each data write sets, forwards one frame to a client that reads it. Nothing waits ten
// seconds. Control: newRelaySockets setting another write wait; the deadline is that wait past its
// origin, and the row names it.
func TestARelaySocketsDataWriteTakesTheDeployedWriteWait(t *testing.T) {
	t.Parallel()
	deadlines := &dataWriteDeadlines{}
	rig := newRelaySocketRig(t, withWriteDeadlines(deadlines))
	const session = "ses_edge_write_wait"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	sent := publishRelayFrame(t, rig.owner.url, session, nil)
	if frame := client.frame("the published frame"); frame.Op != "msg" || frame.Reply != sent.Reply {
		t.Fatalf("the socket sent %s, want the msg %s", frame.raw, sent.Reply)
	}
	if set := deadlines.requireEach(t, relaySocketWriteWait, "the socket's data write"); set != 1 {
		t.Fatalf("the socket's one data frame set %d write deadlines, want 1", set)
	}
}

// A socket whose client reads its frames slower than they come falls past what one socket may
// hold for its client (relaySocketPendingMsgs, relaySocketPendingBytes): the frame that would pass
// them closes the socket 1013 slow_consumer, on which its client opens it again and the owning
// relay sends the frames it held again, since it heard no receipt for them. The row holds the
// socket's pump from writing (its write lock), so every frame the socket takes stays held, and
// raises nats.go's own pending limits on the socket's subscription above the flood, so nats.go
// drops none ahead of the queue: of 80 frames of 512 KiB, the one past the socket's 32 frames or
// 16 MiB closes it, however fast they come. The socket checks its token once an hour, so no ping
// and no read deadline ends it first, and the grace its close is written in is raised to an hour
// (withCloseGrace), so no clock decides the row; it waits for the client's read to end, bounded
// only by the test binary's deadline. Control: the socket's own check of what it holds removed
// (enqueue); the replica's budget is far past the flood, so nothing closes the socket, and the row
// fails at the deadline naming the wait.
func TestARelaySocketWhoseSubscriptionFallsBehindClosesSlowConsumer(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withCloseGrace(time.Hour))
	const session = "ses_edge_slow_consumer"
	token := "access-token-of-a-slow-consumer"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
	conn, socket := rig.dialStalled(t, session, token)
	if err := socket.awaitSubscription(t).SetPendingLimits(1<<16, 1<<30); err != nil {
		t.Fatalf("raise the subscription's pending limits: %v", err)
	}
	socket.writeMu.Lock()
	t.Cleanup(sync.OnceFunc(socket.writeMu.Unlock))
	client := startRelaySocketClient(t, conn)
	flood(t, rig.owner.url, session, 80)
	err := testwait.Receive(t, "the socket whose pump is held to end, after 80 frames of 512 KiB came at once", client.closed)
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.CloseTryAgainLater || closed.Text != relaySocketReasonSlowConsumer {
		heldMsgs, heldBytes := socket.held()
		t.Fatalf("the socket that fell behind ended with %v, want a close %d %q (it holds %d frames, %d bytes)", err, websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer, heldMsgs, heldBytes)
	}
}

// relaySocketFrameWire is about what one flood frame (512 KiB of data, base64 inside its JSON)
// takes on the wire; the order rows below call a control frame behind half of it, or more, one
// that waited for the frame's data.
const relaySocketFrameWire = 700 << 10

// A socket that closes 1013 slow_consumer while a data frame is being written to its client
// writes the close in the middle of that frame, behind one fragment of its data rather than all
// of it: write hands gorilla/websocket each frame in pieces of relaySocketWriteFragment, which it
// sends as fragments of that size, and a control frame may go between two fragments (RFC 6455,
// section 5.4). A close behind all of a frame's data waits as long as the client takes to read it,
// and past relaySocketCloseGrace the connection is dropped with no close frame: a client that
// reads slowly read 1006. The row holds the socket's writer inside its write of the first fragment
// of a frame of 512 KiB (heldWrites), as a client that reads nothing holds it, so that write holds
// the connection's write lock; 2 frames more come, and the second of them closes the socket, which
// holds at most 2. The row waits until the close waits for the connection's write lock
// (awaitControlWriteWaiting), so it is handed the lock the moment the write holding it lets go, and
// only then lets the write through. The writer is held at the gate rather than by a client that
// reads nothing through the least buffers the kernel allows: there a write holds the lock only
// while the kernel blocks it, and a close that came while the writer was between two fragments
// took the free lock at once and waited for it never, so the row waited for it until the test
// binary's deadline. The grace and the write deadline are raised to an hour and the socket checks
// its token once an hour, so no clock decides the row; it reads the order on the wire: the client
// has read less than half the frame (relaySocketFrameWire) when the close comes, and it waits on
// the client's own reads, bounded only by the test binary's deadline. Control: write handing
// gorilla each frame whole (WriteJSON), which writes a frame past twice its buffer as one fragment
// under its connection's write lock, and the frame's empty last fragment after it; the close waits
// for all of the frame's data, which the client reads first, and the row names it.
func TestARelaySocketsCloseGoesBetweenTheFragmentsOfAFrame(t *testing.T) {
	// Not parallel: it reads every goroutine's stack (runtime.Stack in awaitControlWriteWaiting)
	// for a close waiting in WriteControl, which another test's socket would also match.
	held := &heldWrites{}
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(2, 1<<30), withCloseGrace(time.Hour), withWriteWait(time.Hour), withHeldWrites(held))
	const session = "ses_edge_held_writer"
	token := "access-token-of-a-held-writer"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
	conn, counted, socket := rig.dialCounted(t, session, token)
	// nats.go's own pending limits on the subscription are the socket's 2 frames; a frame counts
	// there until enqueue returns, so they are raised above the 3 frames the row sends, leaving the
	// socket's own limit of 2 the one that closes it.
	if err := socket.awaitSubscription(t).SetPendingLimits(1<<16, 1<<30); err != nil {
		t.Fatalf("raise the subscription's pending limits: %v", err)
	}
	client := startRelaySocketClient(t, conn)
	before := counted.read.Load()
	held.hold(t)
	flood(t, rig.owner.url, session, 1)
	held.waitHeld(t, "the socket's write of the first frame to wait at the gate")
	flood(t, rig.owner.url, session, 2)
	awaitControlWriteWaiting(t, "the socket's close to wait for its connection's write lock")
	held.release()
	err := testwait.Receive(t, "the socket to end, after 2 frames more of 512 KiB came", client.closed)
	if read := counted.read.Load() - before; read >= relaySocketFrameWire/2 {
		t.Fatalf("the client read %d bytes of the first frame before the socket's close, half the frame's %d or more: the close waited for the frame's data being written", read, relaySocketFrameWire)
	}
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.CloseTryAgainLater || closed.Text != relaySocketReasonSlowConsumer {
		heldMsgs, heldBytes := socket.held()
		t.Fatalf("the client read %v after %d bytes, want the close %d %q in the middle of the first frame (the socket ended %v, holding %d frames, %d bytes)", err, counted.read.Load()-before, websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer, socket.ended.Load(), heldMsgs, heldBytes)
	}
}

// A socket's ping goes between two fragments of the data frame being written to its client,
// behind one fragment of its data rather than all of it, as the close does: the ping is not held
// behind a frame for up to relaySocketWriteWait, past which it would take a client that reads
// slowly for gone. The row holds the socket's writer inside its write of the first fragment of a
// frame of 512 KiB (heldWrites), as a client that reads nothing holds it, so that write holds the
// connection's write lock; it starts a ping (socket.ping), waits until the ping waits for that lock
// (awaitControlWriteWaiting), and only then lets the write through. The writer is held at the gate
// for the reason the close's row gives. The client's ping handler notes how many bytes it had read:
// under half the frame (relaySocketFrameWire). The frame then comes whole, its data intact across
// the fragments and the ping between them. The write deadline is raised to an hour and the socket
// checks its token once an hour, so no clock decides the row; it reads the order on the wire and
// waits on the client's own reads, bounded only by the test binary's deadline. Control: write
// handing gorilla each frame whole (WriteJSON); the ping waits for all of the frame's data, which
// the client reads first, and the row names it.
func TestARelaySocketsPingGoesBetweenTheFragmentsOfAFrame(t *testing.T) {
	// Not parallel: it reads every goroutine's stack (runtime.Stack in awaitControlWriteWaiting)
	// for a ping waiting in WriteControl, which another test's socket would also match.
	held := &heldWrites{}
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withWriteWait(time.Hour), withHeldWrites(held))
	const session = "ses_edge_held_ping"
	token := "access-token-of-a-held-pinged-writer"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
	conn, counted, socket := rig.dialCounted(t, session, token)
	// pinged carries how many bytes the client had read in all when the first ping came, or -1
	// when its read ended first (ended then holds why).
	pinged := make(chan int64, 1)
	conn.SetPingHandler(func(data string) error {
		select {
		case pinged <- counted.read.Load():
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Time{})
	})
	messages := make(chan []byte, 1)
	ended := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				ended <- err
				select {
				case pinged <- -1:
				default:
				}
				return
			}
			select {
			case messages <- data:
			default:
			}
		}
	}()
	before := counted.read.Load()
	held.hold(t)
	flood(t, rig.owner.url, session, 1)
	held.waitHeld(t, "the socket's write of the frame to wait at the gate")
	socket.ping()
	awaitControlWriteWaiting(t, "the socket's ping to wait for its connection's write lock")
	held.release()
	atPing := testwait.Receive(t, "the client's ping handler to run", pinged)
	if atPing < 0 {
		t.Fatalf("the client's read ended before any ping came: %v", <-ended)
	}
	if read := atPing - before; read >= relaySocketFrameWire/2 {
		t.Fatalf("the ping reached the client after %d bytes of the frame, half the frame's %d or more: the ping waited for the frame's data being written", read, relaySocketFrameWire)
	}
	var frame relayFrameRead
	if err := json.Unmarshal(testwait.Receive(t, "the frame the ping went into to come whole", messages), &frame); err != nil {
		t.Fatalf("the frame the ping went into is no JSON frame: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(frame.Data)
	if frame.Op != "msg" || err != nil || !bytes.Equal(data, bytes.Repeat([]byte("x"), 512<<10)) {
		t.Fatalf("the frame the ping went into came as op %q with %d bytes of data (%v), want the msg frame of 512 KiB of x the row sent", frame.Op, len(data), err)
	}
}

// A frame the NATS client drops ahead of the socket's queue, past nats.go's own pending limits on
// the subscription, closes nothing: the queue is the bound on what a socket holds, and the owning
// relay sends that frame again, since it hears no receipt for it. The row holds the queue's lock,
// so the subscription's goroutine waits in enqueue with the first frame, lowers nats.go's limit on
// the socket's subscription to 2 frames and sends 6: nats.go keeps 2, that one among them, and
// drops 4. Once the lock is let go the client reads the 2 kept, and then a frame published after
// them, so the socket stayed open: a close over the frames dropped would be made by a forward of
// the socket's one subscription, which hands its frames on in order, at a kept frame or at the
// frame after them, and a socket that has ended writes no frame after its close. Control: the
// socket closing 1013 slow_consumer once its subscription dropped a frame (forward reading
// Subscription.Dropped); the client reads the first frame and then that close, and the row fails
// naming it.
func TestARelaySocketStaysOpenWhenTheNATSClientDropsAFrameAheadOfItsQueue(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour))
	const session, sent = "ses_edge_nats_drops", 6
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	socket := rig.openSocket(t, session)
	sub := socket.awaitSubscription(t)
	if err := sub.SetPendingLimits(2, 1<<30); err != nil {
		t.Fatalf("lower the subscription's pending limits: %v", err)
	}
	unlock := sync.OnceFunc(socket.queueMu.Unlock)
	socket.queueMu.Lock()
	defer unlock()
	flood(t, rig.owner.url, session, sent)
	var kept, dropped int
	testwait.Until(t, fmt.Sprintf("nats.go to keep or drop each of the %d frames sent", sent), func() (string, bool) {
		kept, _, _ = sub.Pending()
		dropped, _ = sub.Dropped()
		return fmt.Sprintf("kept %d, dropped %d", kept, dropped), kept+dropped == sent
	})
	unlock()
	if dropped == 0 {
		t.Fatalf("nats.go kept %d and dropped %d of the %d frames sent, want some dropped", kept, dropped, sent)
	}
	t.Logf("nats.go kept %d and dropped %d of the %d frames sent", kept, dropped, sent)
	for i := range kept {
		if frame := client.frame("a frame nats.go kept"); frame.Op != "msg" {
			t.Fatalf("frame %d of the %d nats.go kept reached the client as %s, want a msg", i+1, kept, frame.raw)
		}
	}
	after := publishRelayFrame(t, rig.owner.url, session, nil)
	if frame := client.frame("the frame published after the ones nats.go kept"); frame.Op != "msg" || frame.sid(t) != 1 || frame.Reply != after.Reply {
		t.Fatalf("the socket sent %s after the frames nats.go kept, want the msg %s published after them under sid 1", frame.raw, after.Reply)
	}
}

// What a replica's sockets hold for their clients together stays within its budget
// (relaySocketReplicaPendingBytes; here 8 MiB), whatever each socket may hold alone: four clients
// that read nothing are each sent 20 frames of 512 KiB at once, 10 MiB each and 40 MiB in all, and
// the frame that would take the replica past the budget closes its socket 1013 slow_consumer. Each
// socket's pump is held from writing (its write lock, which the row holds), so every frame a
// socket takes stays counted and none is written ahead of the close; each socket alone is sent
// more than the budget, so each closes, and its client reads the close first. The replica's count
// of the bytes held never passes the budget, and once the pumps are let go it is 0 again. The four
// sockets share the replica's one NATS connection, so the 40 MiB stay under the 64 MiB nats-server
// queues for one connection (max_pending): past that it cuts the connection as a slow consumer, and
// the frames it held for the sockets not yet closed are lost, so those sockets never close. Control:
// the budget's check removed; the count passes the budget, and the row fails naming it.
func TestARelaySocketsReplicaHoldsNoMoreThanItsBudget(t *testing.T) {
	t.Parallel()
	const budget, frames = 8 << 20, 20
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withReplicaBudget(budget))
	sessions := []string{"ses_edge_budget_a", "ses_edge_budget_b", "ses_edge_budget_c", "ses_edge_budget_d"}
	conns := make([]*websocket.Conn, len(sessions))
	sockets := make([]*relaySocket, len(sessions))
	unlocks := make([]func(), len(sessions))
	for i, session := range sessions {
		token := "access-token-of-" + session
		rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
		conns[i], sockets[i] = rig.dialStalled(t, session, token)
		sockets[i].writeMu.Lock()
		unlocks[i] = sync.OnceFunc(sockets[i].writeMu.Unlock)
		t.Cleanup(unlocks[i])
	}
	var most atomic.Int64
	watching := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if held := rig.sockets.buffered.Load(); held > most.Load() {
				most.Store(held)
			}
			select {
			case <-watching:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	stopWatching := sync.OnceFunc(func() {
		close(watching)
		<-watched
	})
	t.Cleanup(stopWatching)
	for _, session := range sessions {
		flood(t, rig.owner.url, session, frames)
	}
	// Each socket has either taken every frame sent to it or closed.
	testwait.Eventually(t, "each socket to take every frame sent to it or close", func() bool {
		for _, socket := range sockets {
			if held, _ := socket.held(); !socket.ended.Load() && held != frames {
				return false
			}
		}
		return true
	})
	stopWatching()
	if most.Load() > budget {
		t.Fatalf("the replica's sockets held %d bytes at once, past its budget of %d", most.Load(), budget)
	}
	for i, socket := range sockets {
		if !socket.ended.Load() {
			held, bytes := socket.held()
			t.Fatalf("the socket of %s holds %d frames, %d bytes, and is open, though it alone was sent %d frames of 512 KiB past the replica's budget of %d", sessions[i], held, bytes, frames, budget)
		}
		_ = conns[i].SetReadDeadline(time.Now().Add(listenerWaitBound(t)))
		_, _, err := conns[i].ReadMessage()
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.CloseTryAgainLater || closed.Text != relaySocketReasonSlowConsumer {
			t.Fatalf("the client of %s read %v first, want the close %d %q", sessions[i], err, websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer)
		}
	}
	for _, unlock := range unlocks {
		unlock()
	}
	testwait.Eventually(t, "the replica's count of the bytes its sockets hold to be 0 once their pumps are let go", func() bool {
		return rig.sockets.buffered.Load() == 0
	})
	t.Logf("the replica held at most %d of its %d bytes; %d of %d sockets closed %s", most.Load(), budget, len(sockets), len(sessions), relaySocketReasonSlowConsumer)
}

// A replica holds at most its cap of sockets, of every session: the next upgrade is refused 503,
// on which its client opens it on another replica. Control: once a socket closes, the same upgrade
// opens.
func TestARelaySocketPastTheListenersCapIsRefused503(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withMaxSockets(2))
	first, _ := rig.open(t, "ses_edge_cap_a")
	rig.open(t, "ses_edge_cap_b")
	const third = "ses_edge_cap_c"
	token := "access-token-of-the-socket-past-the-cap"
	rig.broker.Says(token, relaySocketAnswer(third, rig.caller.JKT(), time.Hour, relaySocketProject))
	if _, response, err := rig.dialAs(t, third, rig.upgradeHeader(t, third, token), relaySubprotocol); err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a socket past the replica's cap of 2 = %v (%s), want 503", err, describeUpgrade(response))
	}
	if err := first.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("close the first socket: %v", err)
	}
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool { return rig.holds("ses_edge_cap_a") == 0 }) {
		t.Fatal("the replica still holds the first socket the time this package gives a wait after its client closed it")
	}
	rig.openWith(t, third, token)
}

// A binary frame closes the socket 1003 text_frames_only: the wire is text frames of JSON.
// Control: the binary frame's branch removed; the socket stays open, and the row fails naming it.
func TestARelaySocketClosesOnABinaryFrame(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_binary"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	client.sendRaw(websocket.BinaryMessage, []byte(`{"op":"unsub","sid":1}`))
	if code, text := client.closeNext("the close a binary frame gets"); code != websocket.CloseUnsupportedData || text != relaySocketReasonTextOnly {
		t.Fatalf("a binary frame closed the socket %d %q, want %d %q", code, text, websocket.CloseUnsupportedData, relaySocketReasonTextOnly)
	}
}

// A socket whose subscription its connection lost closes 1013 nats_unavailable at its next check,
// on which its client opens it again after 5 s, on whichever replica answers. The row closes the
// connection, waits until the socket's subscription reads as lost (the state the check reads), and
// moves the rig's clock to the socket's first check and leaves it there: the close must come from
// that one check, with nothing before it. The reader's own deadline, on the real clock, is raised
// past the test binary's (withPongFloor), so nothing but a check closes the socket. Control: the
// subscription's check removed; the socket stays open, and the row fails naming the close it waited
// for.
func TestARelaySocketClosesWhenItsSubscriptionIsLost(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withPongFloor(relaySocketPastTheBinary))
	const session = "ses_edge_lost_subscription"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	checks := rig.checksOf(t, client, session)
	rig.client.Close()
	testwait.Until(t, "the socket's subscription to read as lost once its connection closed", func() (bool, bool) {
		subscribed := checks.socket.subscribed()
		return subscribed, !subscribed
	})
	if code, text := checks.closeAt(t, checks.next, "the close at the first check after the subscription was lost"); code != websocket.CloseTryAgainLater || text != relaySocketReasonNATSUnavailable {
		t.Fatalf("a socket whose subscription was lost closed %d %q, want %d %q", code, text, websocket.CloseTryAgainLater, relaySocketReasonNATSUnavailable)
	}
}

// An open socket closes 4401 at its token's expiry, not at the next check, which here is a day
// away: its client trades for a fresh token at once. The stand-in answers the token live all along
// with exp the whole second at or before its expiry, as the real broker answers it for up to a
// second past that exp, so the close rests on the socket reading the exp and not on the broker's
// refusal. The token lives an hour, and the socket's expiry timer runs on the rig's clock, armed
// for the exp itself: one nanosecond short of it the timer is still armed, the socket's watch not
// woken, and at it the socket closes 4401 credential_expired, with no check between. Controls: the
// verdict reading only the broker's refusal as expired, and the socket stays open past the exp,
// which the row names; the expiry left on the real clock's timer, and the row fails waiting for the
// timer armed at the exp.
func TestAnOpenSocketClosesAtItsTokensExpiry(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(relaySocketPastTheBinary))
	const session = "ses_edge_expiring"
	token := "access-token-that-expires-in-an-hour"
	answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
	rig.broker.Says(token, answer)
	client := rig.openWith(t, session, token)
	expires := time.Unix(answer.ExpiresAt, 0)
	rig.clock.AwaitArmedAt(t, "the socket's expiry timer", expires)
	rig.clock.Set(expires.Add(-time.Nanosecond))
	if armed := rig.clock.Armed(); !slices.ContainsFunc(armed, expires.Equal) {
		t.Fatalf("one nanosecond short of the token's exp %v the clock holds the timers %v: the socket's expiry timer is gone", expires, armed)
	}
	rig.clock.Set(expires)
	if code, text := client.closeNext("the close at the token's exp"); code != relaySocketCloseExpired || text != relaySocketReasonExpired {
		t.Fatalf("the socket closed %d %q at its token's expiry, want %d %q\n%s", code, text, relaySocketCloseExpired, relaySocketReasonExpired, rig.logs.String())
	}
}

// A socket the listener closes drops its connection as soon as the client answers the close
// frame, not when relaySocketCloseGrace runs out: a client that reopens once its connection ends
// (Bun's WebSocket fires close only then) reopens at once, a 4401's "at once" included. Two closes
// the listener begins, each on its own socket: a 4403 the check writes (the token revoked) and a
// 1003 the reader writes (a binary frame). The client answers the close as gorilla/websocket does,
// and the row proves by order that the answer ended the connection: the grace's callback runs on
// the rig's clock, which the row does not move once the close has begun, so the connection ends
// with the clock short of the grace's end and the callback still armed there. The socket checks its
// token once a day, so the reader's own deadline (pongWait, on the real clock) is past the test
// binary's, and the token outlives that first check, which reads the revoke. Controls: the reader
// leaving the connection to end's grace once end has run (read's conn.Close removed), or the
// client withholding its answer; the connection does not end, and the row fails naming the wait.
func TestARelaySocketDropsItsConnectionWhenTheClientAnswersTheClose(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		close func(t *testing.T, rig *relaySocketRig, checks *socketChecks, token string) (int, string)
		code  int
		text  string
	}{
		{name: "a 4403 the check writes", code: relaySocketCloseRefused, text: relaySocketReasonRefused,
			close: func(t *testing.T, rig *relaySocketRig, checks *socketChecks, token string) (int, string) {
				rig.revoke(token)
				return checks.closeAt(t, checks.next, "the close at the first check after the revoke")
			}},
		{name: "a 1003 the reader writes", code: websocket.CloseUnsupportedData, text: relaySocketReasonTextOnly,
			close: func(_ *testing.T, _ *relaySocketRig, checks *socketChecks, _ string) (int, string) {
				checks.client.sendRaw(websocket.BinaryMessage, []byte(`{"op":"unsub","sid":1}`))
				return checks.client.closeNext("the close a binary frame gets")
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rig := newRelaySocketRig(t, withCheckEvery(relaySocketPastTheBinary))
			const session = "ses_edge_close_answered"
			token := "access-token-of-a-socket-whose-close-is-answered"
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 2*relaySocketPastTheBinary, relaySocketProject))
			client := rig.openWith(t, session, token)
			checks := rig.checksOf(t, client, session)
			if code, text := row.close(t, rig, checks, token); code != row.code || text != row.text {
				t.Fatalf("the socket closed %d %q, want %d %q", code, text, row.code, row.text)
			}
			graceEnds := rig.clock.Now().Add(relaySocketCloseGrace)
			raw := client.conn.NetConn()
			ended := make(chan error, 1)
			go func() {
				_, err := raw.Read(make([]byte, 1))
				ended <- err
			}()
			if err := testwait.Receive(t, "the listener to drop the connection once the client answered its close", ended); !errors.Is(err, io.EOF) {
				t.Fatalf("the connection after the answered close read %v, want the listener's end of it (EOF)", err)
			}
			rig.clock.AwaitArmedAt(t, "the close's grace, which the clock has not reached", graceEnds)
		})
	}
}

// A client that reads promptly keeps its socket however large the session's backlog: an edge
// relay holds at most relayEdgeInFlight of a session's frames in flight (its consumers'
// MaxAckPending), within the socket's pending limits, so a restart at the ack floor with 300
// frames waiting sends them as fast as the client answers them. The backlog is stored while no
// socket subscribes, and the session's next registration restarts the relay at the floor. Once
// the client has answered the whole backlog, the frame of an event stored after it reaches the
// client, so the socket stayed open: the relay hands the session's frames to the socket's one
// subscription in the stream's order, that event's last, and the socket writes them in the order
// they came (relay_socket_queue.go), so a close over the backlog would come ahead of that frame,
// and a socket that has ended writes no frame after its close. Control: the edge's consumers at
// bus.SessionConsumerMaxAckPending; the relay holds the whole backlog in flight before the
// restart, and the row fails naming its wait for relayEdgeInFlight of it in flight.
func TestARelaySocketOfAPromptClientTakesABacklogPastItsPendingLimits(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour))
	const session, backlog = "ses_edge_backlog", 300
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	stored := map[string]bool{}
	for range backlog {
		item, _ := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
		stored[item.EventID] = true
	}
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	// The relay sent the first relayEdgeInFlight frames to no socket, and the registration below
	// restarts it at the floor, which deletes the consumer holding them and creates it again, so
	// the row waits for that many in flight and then for the consumer's stored state
	// (awaitSessionConsumerStored).
	awaitAckState(t, rig.owner.consumers, session, relayEdgeInFlight, 0)
	awaitSessionConsumerStored(t, rig.owner.harness.client.Conn, session)
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-2"))
	// take waits for the next frame, requires it to be a msg of an event the row stored, answers
	// it done, as a prompt client does, and returns the event's id.
	take := func(what string) string {
		t.Helper()
		frame := client.frame(what)
		if frame.Op != "msg" {
			t.Fatalf("%s: the socket sent %s, want only msg frames", what, frame.raw)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(frame.data(t), &item); err != nil || !stored[item.EventID] {
			t.Fatalf("%s: the socket sent %q (%v), which is none of the stored events", what, frame.data(t), err)
		}
		client.pub(frame.Reply, relayReplyDone)
		return item.EventID
	}
	seen := map[string]bool{}
	for len(seen) < backlog {
		seen[take(fmt.Sprintf("frame %d of the backlog of %d", len(seen)+1, backlog))] = true
	}
	after, _ := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	stored[after.EventID] = true
	// A frame of the backlog the relay sends again comes ahead of it, and is passed over.
	for take("the frame of the event stored after the backlog") != after.EventID {
	}
}

// A frame over 1 MiB closes the socket 1009 (control: a frame just under the bound is read and
// answered).
func TestARelaySocketClosesOnAFrameOverItsReadLimit(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_oversized"
	client, _ := rig.open(t, session)
	// A pub of a reply never forwarded, padded to just under the bound, is read and refused.
	padding := strings.Repeat("a", relaySocketReadLimit-200)
	client.send(map[string]any{"op": "pub", "subject": "notifications.envoy.relayreply.padded", "data": "", "padding": padding})
	if frame := client.frame("the answer to a frame under the bound"); frame.Code != "publish_not_granted" {
		t.Fatalf("a frame under the bound was answered %s, want it read and refused publish_not_granted", frame.raw)
	}
	client.send(map[string]any{"op": "pub", "subject": "notifications.envoy.relayreply.padded", "data": "", "padding": padding + strings.Repeat("a", 400)})
	if code, _ := client.closeNext("the close a frame over the bound gets"); code != websocket.CloseMessageTooBig {
		t.Fatalf("a frame over the bound closed the socket %d, want %d", code, websocket.CloseMessageTooBig)
	}
}

// A frame the socket cannot read as the wire's - one that is not JSON, one of an unknown op, a sub or
// unsub that names no sid, and a sub of a sid the socket already holds - is answered the err frame
// bad_frame, and the socket is closed 1008 with reason bad_frame. Each row reads the err frame only
// once the client's reader has read the close behind it (closeQueued), the state load puts a run
// in, where a read that took the close first would miss the frame that came before it. Its waits
// for the close, the frame and the close's code set no span of their own: each ends on its fact or
// 30 s before the test binary's deadline (testwait), so a loaded machine slows the row and does not
// fail it. Control: a well-formed sub on the same rig is taken and closes nothing.
func TestARelaySocketClosesOnAFrameItCannotRead(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_bad_frames"
	for _, row := range []struct {
		name   string
		before []string
		frame  string
	}{
		{name: "not JSON", frame: `not json`},
		{name: "an unknown op", frame: `{"op":"publish","subject":"x"}`},
		{name: "a sub naming no sid", frame: `{"op":"sub","subject":"` + bus.DeliverSubject(session) + `"}`},
		{name: "an unsub naming no sid", frame: `{"op":"unsub"}`},
		{name: "a sub of a sid held", before: []string{`{"op":"sub","sid":1,"subject":"` + bus.DeliverSubject(session) + `"}`},
			frame: `{"op":"sub","sid":1,"subject":"` + bus.DeliverSubject(session) + `"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			client, _ := rig.open(t, session)
			for _, frame := range append(row.before, row.frame) {
				client.sendRaw(websocket.TextMessage, []byte(frame))
			}
			client.closeQueued()
			if frame := client.frame("the err frame ahead of the close"); frame.Op != "err" || frame.Code != relayErrBadFrame {
				t.Fatalf("%s was answered %s, want err %s", row.frame, frame.raw, relayErrBadFrame)
			}
			if code, text := client.closeNext("the close behind the err frame"); code != websocket.ClosePolicyViolation || text != relayErrBadFrame {
				t.Fatalf("%s closed the socket %d %q, want %d %q", row.frame, code, text, websocket.ClosePolicyViolation, relayErrBadFrame)
			}
		})
	}
	client, _ := rig.open(t, session)
	client.sub(1, bus.DeliverSubject(session))
	client.ping()
	select {
	case <-client.pongs:
	case err := <-client.closed:
		t.Fatalf("a well-formed sub closed the socket: %v", err)
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("no pong within the time this package gives a wait, after a well-formed sub")
	}
}

// A socket holds one subscription: a second sub of its relay subject, under another sid, is
// answered that sid's err subscription_limit and takes no subscription, so every frame reaches the
// client once; the socket stays open and goes on forwarding under the sid it holds. A frame
// published after the first is the marker that the first came once and the socket stayed open:
// the replica holds the one subscription, which hands its frames on in the order the server routed
// them, and the socket writes them in that order, so a second copy of the first frame would reach
// the client ahead of the second, and a socket that the sub past the limit closed writes neither.
// Control: the first sub is taken.
func TestARelaySocketRefusesASubscriptionPastItsLimit(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_many_subs"
	client, _ := rig.open(t, session)
	held := rig.client.Connection().NumSubscriptions()
	client.sub(1, bus.DeliverSubject(session))
	client.sub(2, bus.DeliverSubject(session))
	frame := client.frame("the answer to the second sub")
	if frame.Op != "err" || frame.Code != relayErrSubscriptionLimit || frame.sid(t) != 2 {
		t.Fatalf("a second sub was answered %s, want err %s for sid 2", frame.raw, relayErrSubscriptionLimit)
	}
	if now := rig.client.Connection().NumSubscriptions(); now != held+1 {
		t.Fatalf("the replica holds %d subscriptions for the socket, want the one it took", now-held)
	}
	first := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	if got := client.frame("the frame published once"); got.Op != "msg" || got.sid(t) != 1 || got.Reply != first.Reply {
		t.Fatalf("the socket sent %s, want the msg %s under sid 1", got.raw, first.Reply)
	}
	second := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	if got := client.frame("the frame published after the first"); got.Op != "msg" || got.sid(t) != 1 || got.Reply != second.Reply {
		t.Fatalf("the socket sent %s after the frame published once, want the msg %s published after it, under sid 1", got.raw, second.Reply)
	}
}

// Frames are handled in order: a socket's frames are read one at a time, and a sub is handled
// whole - its subscribe and the flush that makes the server hold it - before the next frame is
// read. The socket's replica reaches NATS over a link with a 400 ms round trip, held on a clock of
// the link's own (withSocketNATSLatency), which moves only as the row carries the link (carry): the
// row asks for what it waits for before each move, and moves only past chunks the link held when
// it last asked, so a reply sent without waiting for the subscribe is found while the link still
// holds the subscribe. The row sends a sub of its own relay subject and, at once, a sub of another
// session's: the second's err is written only once the first's subscribe is held, so a message
// published the moment that err arrives, with the link where it stood then, reaches the socket
// ahead of a marker published once the socket holds the subscription (requireAheadOfAMarker).
// Control: each frame handled on a goroutine of its own; the err arrives while the link still
// holds the subscribe, the message is lost, the marker reaches the socket first, and the row fails
// naming the message.
func TestARelaySocketHandlesASubWholeBeforeItReadsTheNextFrame(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withSocketNATSLatency(400*time.Millisecond))
	const session = "ses_edge_in_order"
	client, _ := rig.open(t, session)
	client.sub(1, bus.DeliverSubject(session))
	client.sub(2, bus.DeliverSubject("ses_someone_else"))
	var answer relayFrameRead
	rig.carry(t, "the answer to the second sub", client.nextFrame("the answer to the second sub", &answer))
	if answer.Code != relayErrSubjectNotGranted || answer.sid(t) != 2 {
		t.Fatalf("the second sub was answered %s, want err %s for sid 2", answer.raw, relayErrSubjectNotGranted)
	}
	sent := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	rig.requireAheadOfAMarker(t, client, session, sent, "the message published once the second sub was answered",
		"the second sub was answered before the first's subscribe was held")
}

// Row 10: the client's flush() is a ping, answered on the socket's one reader only after every
// frame read before it is handled, so a ping sent right after a sub gets its pong only once that
// sub's subscribe is held, and a message published the moment the pong arrives, with the link
// where it stood then, reaches the socket ahead of a marker published once the socket holds the
// subscription (requireAheadOfAMarker). The replica's link has a 400 ms round trip on its own
// clock, which the row carries, as above. Control: a ping handler that answers at once, off the
// reader (the reader hands each frame to a goroutine that handles them in order, and reads on, so
// gorilla answers the ping as it reads it); the pong precedes the subscribe, the message is lost,
// the marker reaches the socket first, and the row fails naming the message.
func TestAPingAfterASubIsAnsweredAfterTheSubscribe(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withSocketNATSLatency(400*time.Millisecond))
	const session = "ses_edge_flush"
	client, _ := rig.open(t, session)
	client.sub(1, bus.DeliverSubject(session))
	client.ping()
	rig.carry(t, "the pong of the ping sent after the sub", func() (string, bool) {
		select {
		case <-client.pongs:
			return "the pong", true
		default:
		}
		client.requireNothingYet("the pong of the ping sent after the sub")
		return "no pong yet", false
	})
	sent := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	rig.requireAheadOfAMarker(t, client, session, sent, "the message published the moment the pong arrived",
		"the pong came before the subscribe was held")
}

// Headers: msg.headers is always present, a JSON object mapping each header name as
// NATS carries it, its case kept, to every one of its values in order, and the relay's
// Envoy-Subject alone when the message carries no header of its own. Control: the headers read one
// value per name (Header.Get); the repeated values are lost and the row fails naming them.
func TestARelaySocketCarriesEveryValueOfAHeaderInOrder(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session = "ses_edge_headers"
	client, _ := rig.open(t, session)
	client.sub(7, bus.DeliverSubject(session))
	client.ping()
	<-client.pongs

	want := natsgo.Header{"X-Repeated": {"first", "second", "third"}, "lower-case-Name": {"kept"}}
	publishRelayFrame(t, rig.owner.url, session, want)
	frame := client.frame("the frame of the message carrying headers")
	if len(frame.Headers) != len(want) {
		t.Fatalf("the frame carries the headers %v, want exactly %v", frame.Headers, want)
	}
	for name, values := range want {
		if got := frame.Headers[name]; !slices.Equal(got, values) {
			t.Fatalf("the frame carries %s: %q, want %q in that order (all of %s)", name, got, values, frame.raw)
		}
	}

	bare := publishRelayFrame(t, rig.owner.url, session, nil)
	frame = client.frame("the frame of the message carrying only the relay's header")
	if subjects := frame.Headers[relayHeaderSubject]; len(frame.Headers) != 1 || !slices.Equal(subjects, bare.Header.Values(relayHeaderSubject)) {
		t.Fatalf("a frame of a message carrying only the relay's header is %s, want exactly %s %v", frame.raw, relayHeaderSubject, bare.Header.Values(relayHeaderSubject))
	}
}

// Row 9: a listener without ENVOY_EDGE_PROJECTS answers the relay socket's path 404, an upgrade
// included, through the same /v1 a deployment mounts. Control: the same path on the edge rig is
// upgraded.
func TestANonEdgeListenerServesNoRelaySocket(t *testing.T) {
	t.Parallel()
	routes := http.NewServeMux()
	logger := logging.NewWithWriter("not-edge", &bytes.Buffer{})
	registerV1Routes(routes, &listenerDeps{}, "not-edge", logger)
	server := httptest.NewServer(apiAuth(listenerAPIAuth{logger: logger}, routes))
	t.Cleanup(server.Close)
	const session = "ses_not_on_an_edge"

	plain, err := http.Get(server.URL + relaySocketPath(session))
	if err != nil {
		t.Fatalf("GET %s: %v", relaySocketPath(session), err)
	}
	_ = plain.Body.Close()
	if plain.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s on a listener that is not an edge = %d, want 404", relaySocketPath(session), plain.StatusCode)
	}
	dialer := websocket.Dialer{Subprotocols: []string{relaySubprotocol}, HandshakeTimeout: 5 * time.Second}
	if _, response, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+relaySocketPath(session), nil); err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("an upgrade of %s on a listener that is not an edge = %v (%s), want 404", relaySocketPath(session), err, describeUpgrade(response))
	}

	rig := newRelaySocketRig(t)
	rig.open(t, session)
}
