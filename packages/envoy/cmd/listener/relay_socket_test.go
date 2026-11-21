package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
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
	rig := newRelaySocketRig(t)
	const session = "ses_edge_forward"
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)

	stored, seq := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	frame := client.next(5 * time.Second)
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
// client publishes nothing on the receipt family but receipts a word long. Control, in the same
// row: the same subject with done is published, and the owner acks the message.
func TestARelaySocketPublishesOnlyAReceiptsWord(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_receipt_body"
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	_, seq := rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	frame := client.next(5 * time.Second)
	if frame.Op != "msg" {
		t.Fatalf("the socket sent %s, want a msg", frame.raw)
	}
	watcher := testnats.Connect(t, rig.owner.url)
	heard, err := watcher.SubscribeSync(frame.Reply)
	if err != nil {
		t.Fatalf("watch %s: %v", frame.Reply, err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	client.pub(frame.Reply, strings.Repeat("not a receipt ", 4096))
	if got := client.next(5 * time.Second); got.Op != "err" || got.Code != relayErrPublishNotGranted {
		t.Fatalf("a pub of a body that is no reply word was answered %s, want err %s", got.raw, relayErrPublishNotGranted)
	}
	if msg, err := heard.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the replica published %d bytes on %s, which is no receipt", len(msg.Data), frame.Reply)
	}
	client.pub(frame.Reply, relayReplyDone)
	awaitAcked(t, rig.owner.consumers, session, seq)
}

// Row 2, in one process: a sub to another session's relay subject and to the session's own
// agent-stream control subject is answered subject_not_granted under its sid, and the replica
// holds no subscription to either, so a frame published to the other session never reaches this
// socket; a pub to a reply subject the socket never forwarded is publish_not_granted, and nothing
// is published on it. Control: the subject check removed; the other session's frame reaches the
// socket and the row fails naming it.
func TestARelaySocketRefusesASubjectAndAReplyItWasNotGranted(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session, other = "ses_edge_refused", "ses_someone_else"
	client, _ := rig.open(t, session)
	held := rig.client.Connection().NumSubscriptions()

	// refused waits for sid's err, and answers whether one came: a sub answered nothing within the
	// wait is one the replica took, which the frame below then shows.
	refused := func(sid int, subject string) bool {
		t.Helper()
		select {
		case frame := <-client.frames:
			if frame.Op != "err" || frame.Code != "subject_not_granted" || frame.sid(t) != int64(sid) {
				t.Fatalf("a sub to %s was answered %s, want err subject_not_granted for sid %d", subject, frame.raw, sid)
			}
			return true
		case <-time.After(2 * time.Second):
			t.Errorf("a sub to %s was answered nothing within 2 s, want err subject_not_granted for sid %d", subject, sid)
			return false
		}
	}
	client.sub(1, bus.DeliverSubject(other))
	refused(1, bus.DeliverSubject(other))
	client.sub(2, "agentstream."+session+".control")
	refused(2, "agentstream."+session+".control")
	if now := rig.client.Connection().NumSubscriptions(); now != held {
		t.Errorf("the replica holds %d subscriptions after two refused subs, want the %d it held before", now, held)
	}
	publisher := testnats.Connect(t, rig.owner.url)
	stray := &natsgo.Msg{Subject: bus.DeliverSubject(other), Reply: bus.RelayReplyPrefix + "stray.1", Data: []byte(`{"stray":true}`), Header: natsgo.Header{}}
	if err := publisher.PublishMsg(stray); err != nil {
		t.Fatalf("publish a frame for %s: %v", other, err)
	}
	select {
	case frame := <-client.frames:
		t.Fatalf("the replica subscribed %s for this socket: the other session's frame reached it as %s", bus.DeliverSubject(other), frame.raw)
	case <-time.After(time.Second):
	}
	if t.Failed() {
		return
	}

	watcher := testnats.Connect(t, rig.owner.url)
	never := bus.RelayReplyPrefix + "never.forwarded"
	heard, err := watcher.SubscribeSync(never)
	if err != nil {
		t.Fatalf("watch %s: %v", never, err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	client.pub(never, relayReplyDone)
	if frame := client.next(5 * time.Second); frame.Op != "err" || frame.Code != "publish_not_granted" || frame.SID != nil {
		t.Fatalf("a pub to a reply never forwarded was answered %s, want err publish_not_granted naming no sid", frame.raw)
	}
	if msg, err := heard.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the replica published %q on %s, which it never forwarded", msg.Data, never)
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
	waitFor(t, 5*time.Second, "the replica to drop the socket's subscription", func() bool {
		return rig.client.Connection().NumSubscriptions() == held
	})
	if probeSessionConnection(t.Context(), prober, session, logger) {
		t.Fatal("the probe of a session whose socket closed read connected, want stopped")
	}
}

// Rows 4, 5, 7 and 8, in one process, each on its own socket: the broker's answer for the
// socket's token changes, and the socket closes with the code the wire gives that case within a
// second of the first check that reads it (the rig's verifier clock moved past the five seconds a
// live answer is reused for, so the next check asks the broker). The last case is the negative
// control of all of them: an answer that has not changed keeps the socket open. Each row's own
// control is the check of its case removed, and the socket stays open.
func TestAnOpenSocketClosesByTheBrokersAnswer(t *testing.T) {
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
		want  *outcome
	}{
		{name: "its token's exp passed", life: 2 * time.Second, want: &expired,
			later: func(rig *relaySocketRig, session, token string) {
				rig.clock.advance(3 * time.Second)
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
		{name: "its session changed", life: time.Hour, want: &refused,
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Says(token, relaySocketAnswer("ses_another", rig.caller.JKT(), time.Hour, relaySocketProject))
			}},
		{name: "the broker cannot be asked", life: time.Hour, want: &outcome{websocket.CloseTryAgainLater, relaySocketReasonBrokerUnavailable},
			later: func(rig *relaySocketRig, session, token string) {
				rig.broker.Server.Close()
			}},
		{name: "its answer stands", life: time.Hour,
			later: func(rig *relaySocketRig, session, token string) {}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newRelaySocketRig(t)
			const session = "ses_edge_checked"
			token := "access-token-of-a-checked-socket"
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), row.life, relaySocketProject))
			client := rig.openWith(t, session, token)
			client.stillOpen(5*relaySocketTestCheck, "before the answer changed")

			row.later(rig, session, token)
			rig.clock.advance(ciphertoken.AffirmativeCacheTTL + time.Second)
			if row.want == nil {
				client.stillOpen(10*relaySocketTestCheck, "an answer that stands")
				return
			}
			code, text := client.closeCode(time.Second)
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
		if code, text := client.closeCode(2 * time.Second); code != websocket.CloseServiceRestart || text != relaySocketReasonStopping {
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
				case <-time.After(5 * time.Second):
					t.Fatalf("the socket of the session %q was opened and answered no ping; want 400 before the upgrade", session)
				}
				publishRelayFrame(t, rig.owner.url, victim, natsgo.Header{})
				select {
				case frame := <-client.frames:
					t.Fatalf("the socket of the session %q was opened, and %s's frame reached it: %s", session, victim, frame.raw)
				case <-time.After(2 * time.Second):
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

// A socket whose client reads nothing, with a data frame's write blocked on it, is closed within
// relaySocketCloseGrace of the close's start: after a revoke, its connection is dropped within one
// check and the grace of the broker's answer changing, and the listener's stop returns within the
// grace. A write blocked on such a client holds the connection for up to relaySocketWriteWait, so
// neither the close nor the watch's ping may wait for it. Control: end taking the data writers'
// lock, or the watch writing its ping in line; the socket stays open until the blocked write's 10 s
// run out, and the row fails naming how long it took.
func TestARelaySocketWhoseClientReadsNothingClosesWithinTheGrace(t *testing.T) {
	for _, row := range []struct {
		name  string
		close func(t *testing.T, rig *relaySocketRig, token string)
		// within is how long after close the connection must be dropped.
		within time.Duration
	}{
		{name: "a revoke", within: 2*relaySocketTestCheck + relaySocketCloseGrace + time.Second,
			close: func(_ *testing.T, rig *relaySocketRig, token string) { rig.revoke(token) }},
		{name: "the listener's stop", within: relaySocketCloseGrace + time.Second,
			close: func(t *testing.T, rig *relaySocketRig, _ string) {
				started := time.Now()
				rig.sockets.stop()
				if took := time.Since(started); took > relaySocketCloseGrace+time.Second {
					t.Errorf("the listener's stop took %s with a write blocked on a client that reads nothing, want it within %s", took.Round(time.Millisecond), relaySocketCloseGrace)
				}
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			// Limits that hold every frame, so no slow consumer closes the socket first.
			rig := newRelaySocketRig(t, withPendingLimits(1<<16, 1<<30))
			const session = "ses_edge_reads_nothing"
			token := "access-token-of-a-client-that-reads-nothing"
			rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
			// The client sends its sub and reads nothing from here on, and far more than the
			// connection's buffers hold is published: the socket's write blocks, and the rest wait
			// in its queue.
			_, socket := rig.dialStalled(t, session, token)
			flood(t, rig.owner.url, session, 60)
			awaitBlockedWrite(t, socket)

			started := time.Now()
			row.close(t, rig, token)
			if !testwait.Holds(row.within, 10*time.Millisecond, func() bool { return rig.holds(session) == 0 }) {
				t.Fatalf("the socket was still open %s after %s, want it dropped within %s", time.Since(started).Round(time.Millisecond), row.name, row.within)
			}
			t.Logf("the socket was dropped %s after %s", time.Since(started).Round(time.Millisecond), row.name)
		})
	}
}

// A data write that fails - one that ran out its relaySocketWriteWait on a client whose window
// stayed shut - ends the socket and drops its connection at once, though the client goes on
// sending frames that would keep its read deadline from running out. The rig checks once an hour,
// so no check and no ping closes the socket meanwhile, and its limits hold every frame, so no slow
// consumer does. Control: end of code 0 closing nothing; the connection stays as long as the
// client sends, and the row fails naming how long it waited.
func TestARelaySocketWhoseWriteFailsIsDroppedAtOnce(t *testing.T) {
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30))
	const session = "ses_edge_write_fails"
	token := "access-token-of-a-socket-whose-write-fails"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 2*time.Hour, relaySocketProject))
	conn, socket := rig.dialStalled(t, session, token)
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
	awaitBlockedWrite(t, socket)
	blocked := time.Now()
	within := relaySocketWriteWait + relaySocketCloseGrace + time.Second
	if !testwait.Holds(within, 10*time.Millisecond, func() bool { return rig.holds(session) == 0 }) {
		t.Fatalf("the socket was still open %s after its write blocked, want it dropped within %s of the write's deadline", time.Since(blocked).Round(time.Millisecond), relaySocketCloseGrace)
	}
	t.Logf("the socket was dropped %s after its write blocked", time.Since(blocked).Round(time.Millisecond))
}

// A socket whose client reads its frames slower than they come falls past what one socket may
// hold for its client (relaySocketPendingMsgs, relaySocketPendingBytes): the frame that would pass
// them closes the socket 1013 slow_consumer, on which its client opens it again and the owning
// relay sends the frames it held again, since it heard no receipt for them. The client here reads
// one frame each 50 ms, and 80 frames of 512 KiB come at once, past the socket's 32 frames and
// 16 MiB: it reads the frames written before the close and then the close. Control: the socket's
// own check of what it holds removed (enqueue); the replica's budget is far past the flood, so
// nothing closes the socket, and the row fails naming it.
func TestARelaySocketWhoseSubscriptionFallsBehindClosesSlowConsumer(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_slow_consumer"
	token := "access-token-of-a-slow-consumer"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
	conn, _ := rig.dialStalled(t, session, token)
	ended := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				ended <- err
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	flood(t, rig.owner.url, session, 80)
	select {
	case err := <-ended:
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.CloseTryAgainLater || closed.Text != relaySocketReasonSlowConsumer {
			t.Fatalf("the socket that fell behind ended with %v, want a close %d %q", err, websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the socket whose client reads one frame each 50 ms was still open 10s after 80 frames of 512 KiB came at once, want it closed %d %q", websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer)
	}
}

// A frame the NATS client drops ahead of the socket's queue, past nats.go's own pending limits on
// the subscription, closes nothing: the queue is the bound on what a socket holds, and the owning
// relay sends that frame again, since it hears no receipt for it. The row holds the queue's lock,
// so the subscription's goroutine waits in enqueue with the first frame, lowers nats.go's limit on
// the socket's subscription to 2 frames and sends 6: nats.go keeps 2, that one among them, and
// drops 4. Once the lock is let go the client reads the 2 kept, and the socket stays open. Control:
// the socket closing 1013 slow_consumer once its subscription dropped a frame (forward reading
// Subscription.Dropped); the client reads the first frame and then that close, and the row fails
// naming it.
func TestARelaySocketStaysOpenWhenTheNATSClientDropsAFrameAheadOfItsQueue(t *testing.T) {
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
	accounted := testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool {
		kept, _, _ = sub.Pending()
		dropped, _ = sub.Dropped()
		return kept+dropped == sent
	})
	unlock()
	if !accounted || dropped == 0 {
		t.Fatalf("nats.go kept %d and dropped %d of the %d frames sent, want all %d accounted for and some dropped", kept, dropped, sent, sent)
	}
	t.Logf("nats.go kept %d and dropped %d of the %d frames sent", kept, dropped, sent)
	for i := range kept {
		if frame := client.next(5 * time.Second); frame.Op != "msg" {
			t.Fatalf("frame %d of the %d nats.go kept reached the client as %s, want a msg", i+1, kept, frame.raw)
		}
	}
	client.stillOpen(time.Second, "frames nats.go dropped ahead of the queue")
}

// What a replica's sockets hold for their clients together stays within its budget
// (relaySocketReplicaPendingBytes; here 8 MiB), whatever each socket may hold alone: four clients
// that read nothing are each sent 40 frames of 512 KiB at once, 80 MiB in all, and the frame that
// would take the replica past the budget closes its socket 1013 slow_consumer. The replica's count
// of the bytes held never passes the budget, the sockets that held frames close, and once they
// have, the count is 0 again. Control: the budget's check removed; the count passes the budget,
// and the row fails naming it.
func TestARelaySocketsReplicaHoldsNoMoreThanItsBudget(t *testing.T) {
	const budget = 8 << 20
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withReplicaBudget(budget))
	sessions := []string{"ses_edge_budget_a", "ses_edge_budget_b", "ses_edge_budget_c", "ses_edge_budget_d"}
	var conns []*websocket.Conn
	for _, session := range sessions {
		token := "access-token-of-" + session
		rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject))
		conn, _ := rig.dialStalled(t, session, token)
		conns = append(conns, conn)
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
	for _, session := range sessions {
		flood(t, rig.owner.url, session, 40)
	}
	closed := 0
	for i, conn := range conns {
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				var ended *websocket.CloseError
				if errors.As(err, &ended) && ended.Code == websocket.CloseTryAgainLater && ended.Text == relaySocketReasonSlowConsumer {
					closed++
				} else {
					t.Logf("the socket of %s ended with %v", sessions[i], err)
				}
				break
			}
		}
	}
	close(watching)
	<-watched
	if most.Load() > budget {
		t.Fatalf("the replica's sockets held %d bytes at once, past its budget of %d", most.Load(), budget)
	}
	if closed == 0 {
		t.Fatalf("no socket closed %d %q though 80 MiB came for a replica whose budget is %d bytes", websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer, budget)
	}
	if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool { return rig.sockets.buffered.Load() == 0 }) {
		t.Fatalf("the replica still counts %d bytes held 5s after its sockets closed, want 0", rig.sockets.buffered.Load())
	}
	t.Logf("the replica held at most %d of its %d bytes; %d of %d sockets closed %s", most.Load(), budget, closed, len(sessions), relaySocketReasonSlowConsumer)
}

// A replica holds at most its cap of sockets, of every session: the next upgrade is refused 503,
// on which its client opens it on another replica. Control: once a socket closes, the same upgrade
// opens.
func TestARelaySocketPastTheListenersCapIsRefused503(t *testing.T) {
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
	if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool { return rig.holds("ses_edge_cap_a") == 0 }) {
		t.Fatal("the replica still holds the first socket 5s after its client closed it")
	}
	rig.openWith(t, third, token)
}

// A binary frame closes the socket 1003 text_frames_only: the wire is text frames of JSON.
// Control: the binary frame's branch removed; the socket stays open, and the row fails naming it.
func TestARelaySocketClosesOnABinaryFrame(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_binary"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	client.sendRaw(websocket.BinaryMessage, []byte(`{"op":"unsub","sid":1}`))
	if code, text := client.closeCode(5 * time.Second); code != websocket.CloseUnsupportedData || text != relaySocketReasonTextOnly {
		t.Fatalf("a binary frame closed the socket %d %q, want %d %q", code, text, websocket.CloseUnsupportedData, relaySocketReasonTextOnly)
	}
}

// A socket whose subscription its connection lost closes 1013 nats_unavailable at its next check,
// on which its client opens it again after 5 s, on whichever replica answers. Control: the
// subscription's check removed; the socket stays open, and the row fails naming it.
func TestARelaySocketClosesWhenItsSubscriptionIsLost(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_lost_subscription"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	rig.client.Close()
	if code, text := client.closeCode(2 * time.Second); code != websocket.CloseTryAgainLater || text != relaySocketReasonNATSUnavailable {
		t.Fatalf("a socket whose subscription was lost closed %d %q, want %d %q", code, text, websocket.CloseTryAgainLater, relaySocketReasonNATSUnavailable)
	}
}

// An open socket closes 4401 at its token's expiry, not at the next check, which here is an hour
// away: its client trades for a fresh token at once. The stand-in answers the token live all
// along with exp the whole second at or before its expiry, as the real broker answers it for up
// to a second past that exp, so the close rests on the socket reading the exp and not on the
// broker's refusal. Control: the verdict reading only the broker's refusal as expired; the socket
// stays open past the exp, and the row fails naming it.
func TestAnOpenSocketClosesAtItsTokensExpiry(t *testing.T) {
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour))
	const session = "ses_edge_expiring"
	token := "access-token-that-expires-in-two-seconds"
	rig.broker.Says(token, relaySocketAnswer(session, rig.caller.JKT(), 2*time.Second, relaySocketProject))
	client := rig.openWith(t, session, token)
	if code, text := client.closeCode(4 * time.Second); code != relaySocketCloseExpired || text != relaySocketReasonExpired {
		t.Fatalf("the socket closed %d %q at its token's expiry, want %d %q\n%s", code, text, relaySocketCloseExpired, relaySocketReasonExpired, rig.logs.String())
	}
}

// A socket the listener closes drops its connection as soon as the client answers the close
// frame, not when relaySocketCloseGrace runs out: a client that reopens once its connection ends
// (Bun's WebSocket fires close only then) reopens at once, a 4401's "at once" included. Two closes
// the listener begins, each on its own socket: a 4403 the check writes (the token revoked) and a
// 1003 the reader writes (a binary frame). The client answers the close as gorilla/websocket does,
// and the row times the end of the connection from the close frame. Control: the reader leaving
// the connection to end's grace once end has run (read's conn.Close removed); each case's
// connection ends a whole grace after the close, and the row fails naming how long.
func TestARelaySocketDropsItsConnectionWhenTheClientAnswersTheClose(t *testing.T) {
	for _, row := range []struct {
		name  string
		close func(rig *relaySocketRig, client *relaySocketClient, token string)
		code  int
		text  string
	}{
		{name: "a 4403 the check writes", code: relaySocketCloseRefused, text: relaySocketReasonRefused,
			close: func(rig *relaySocketRig, _ *relaySocketClient, token string) { rig.revoke(token) }},
		{name: "a 1003 the reader writes", code: websocket.CloseUnsupportedData, text: relaySocketReasonTextOnly,
			close: func(_ *relaySocketRig, client *relaySocketClient, _ string) {
				client.sendRaw(websocket.BinaryMessage, []byte(`{"op":"unsub","sid":1}`))
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newRelaySocketRig(t)
			client, token := rig.open(t, "ses_edge_close_answered")
			row.close(rig, client, token)
			if code, text := client.closeCode(5 * time.Second); code != row.code || text != row.text {
				t.Fatalf("the socket closed %d %q, want %d %q", code, text, row.code, row.text)
			}
			closed := time.Now()
			raw := client.conn.NetConn()
			if err := raw.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatalf("set the read deadline: %v", err)
			}
			_, err := raw.Read(make([]byte, 1))
			ended := time.Since(closed)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("the connection after the answered close read %v, want the listener's end of it (EOF)", err)
			}
			if ended > relaySocketCloseGrace/4 {
				t.Fatalf("the connection ended %s after the answered close, want it dropped at the client's answer, well inside the %s grace", ended.Round(time.Millisecond), relaySocketCloseGrace)
			}
			t.Logf("the connection ended %s after the answered close", ended.Round(time.Millisecond))
		})
	}
}

// A client that reads promptly keeps its socket however large the session's backlog: an edge
// relay holds at most relayEdgeInFlight of a session's frames in flight (its consumers'
// MaxAckPending), within the socket's pending limits, so a restart at the ack floor with 300
// frames waiting sends them as fast as the client answers them. The backlog is stored while no
// socket subscribes, and the session's next registration restarts the relay at the floor.
// Control: the edge's consumers at bus.SessionConsumerMaxAckPending; the restart sends hundreds
// at once, past the socket's limits, and the socket closes 1013 slow_consumer.
func TestARelaySocketOfAPromptClientTakesABacklogPastItsPendingLimits(t *testing.T) {
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
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-2"))
	seen := map[string]bool{}
	deadline := time.After(60 * time.Second)
	for len(seen) < backlog {
		select {
		case frame := <-client.frames:
			if frame.Op != "msg" {
				t.Fatalf("the socket sent %s after %d of %d frames, want only msg frames", frame.raw, len(seen), backlog)
			}
			var item contracts.Envelope
			if err := json.Unmarshal(frame.data(t), &item); err != nil || !stored[item.EventID] {
				t.Fatalf("the socket sent %q (%v), which is none of the stored backlog", frame.data(t), err)
			}
			seen[item.EventID] = true
			client.pub(frame.Reply, relayReplyDone)
		case err := <-client.closed:
			t.Fatalf("the socket of a prompt client closed after %d of %d frames: %v\n%s", len(seen), backlog, err, rig.logs.String())
		case <-deadline:
			t.Fatalf("the socket of a prompt client was sent %d of %d frames in 60s", len(seen), backlog)
		}
	}
	client.stillOpen(time.Second, "after the backlog")
}

// A frame over 1 MiB closes the socket 1009 (control: a frame just under the bound is read and
// answered).
func TestARelaySocketClosesOnAFrameOverItsReadLimit(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_oversized"
	client, _ := rig.open(t, session)
	// A pub of a reply never forwarded, padded to just under the bound, is read and refused.
	padding := strings.Repeat("a", relaySocketReadLimit-200)
	client.send(map[string]any{"op": "pub", "subject": "notifications.envoy.relayreply.padded", "data": "", "padding": padding})
	if frame := client.next(5 * time.Second); frame.Code != "publish_not_granted" {
		t.Fatalf("a frame under the bound was answered %s, want it read and refused publish_not_granted", frame.raw)
	}
	client.send(map[string]any{"op": "pub", "subject": "notifications.envoy.relayreply.padded", "data": "", "padding": padding + strings.Repeat("a", 400)})
	if code, _ := client.closeCode(5 * time.Second); code != websocket.CloseMessageTooBig {
		t.Fatalf("a frame over the bound closed the socket %d, want %d", code, websocket.CloseMessageTooBig)
	}
}

// A frame the socket cannot read as the wire's - one that is not JSON, one of an unknown op, a sub or
// unsub that names no sid, and a sub of a sid the socket already holds - is answered the err frame
// bad_frame, and the socket is closed 1008 with reason bad_frame. Control: a well-formed sub on the
// same rig is taken and closes nothing.
func TestARelaySocketClosesOnAFrameItCannotRead(t *testing.T) {
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
			if frame := client.next(5 * time.Second); frame.Op != "err" || frame.Code != relayErrBadFrame {
				t.Fatalf("%s was answered %s, want err %s", row.frame, frame.raw, relayErrBadFrame)
			}
			if code, text := client.closeCode(5 * time.Second); code != websocket.ClosePolicyViolation || text != relayErrBadFrame {
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
	case <-time.After(5 * time.Second):
		t.Fatal("no pong within 5 s of a well-formed sub")
	}
}

// A socket holds one subscription: a second sub of its relay subject, under another sid, is
// answered that sid's err subscription_limit and takes no subscription, so every frame reaches the
// client once; the socket stays open and goes on forwarding under the sid it holds. Control: the
// first sub is taken.
func TestARelaySocketRefusesASubscriptionPastItsLimit(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_many_subs"
	client, _ := rig.open(t, session)
	held := rig.client.Connection().NumSubscriptions()
	client.sub(1, bus.DeliverSubject(session))
	client.sub(2, bus.DeliverSubject(session))
	frame := client.next(5 * time.Second)
	if frame.Op != "err" || frame.Code != relayErrSubscriptionLimit || frame.sid(t) != 2 {
		t.Fatalf("a second sub was answered %s, want err %s for sid 2", frame.raw, relayErrSubscriptionLimit)
	}
	if now := rig.client.Connection().NumSubscriptions(); now != held+1 {
		t.Fatalf("the replica holds %d subscriptions for the socket, want the one it took", now-held)
	}
	publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	if got := client.next(5 * time.Second); got.Op != "msg" || got.sid(t) != 1 {
		t.Fatalf("the socket sent %s, want a msg under sid 1", got.raw)
	}
	client.none(time.Second, "a frame sent once")
	client.stillOpen(time.Second, "a sub past the limit")
}

// Frames are handled in order: a socket's frames are read one at a time, and a sub is handled
// whole - its subscribe and the flush that makes the server hold it - before the next frame is
// read. The socket's replica reaches NATS over a link with a 400 ms round trip, so a subscribe
// takes that long to be held. The row sends a sub of its own relay subject and, at once, a sub of
// another session's: the second's err is written only once the first is held, so a message
// published the moment that err arrives reaches the socket. Control: each frame handled on a
// goroutine of its own; the err arrives before the subscribe is held, the message is lost, and the
// row fails naming it.
func TestARelaySocketHandlesASubWholeBeforeItReadsTheNextFrame(t *testing.T) {
	rig := newRelaySocketRig(t, withSocketNATSLatency(400*time.Millisecond))
	const session = "ses_edge_in_order"
	client, _ := rig.open(t, session)
	client.sub(1, bus.DeliverSubject(session))
	client.sub(2, bus.DeliverSubject("ses_someone_else"))
	if frame := client.next(5 * time.Second); frame.Code != "subject_not_granted" || frame.sid(t) != 2 {
		t.Fatalf("the second sub was answered %s, want err subject_not_granted for sid 2", frame.raw)
	}
	sent := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	select {
	case frame := <-client.frames:
		if frame.Op != "msg" || frame.sid(t) != 1 || frame.Reply != sent.Reply {
			t.Fatalf("the message published once the second sub was answered reached the socket as %s, want the msg %s under sid 1", frame.raw, sent.Reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the message %s published once the second sub was answered never reached the socket: the second sub was answered before the first's subscribe was held", sent.Reply)
	}
}

// Row 10: the client's flush() is a ping, answered on the socket's one
// reader only after every frame read before it is handled, so a ping sent right after a sub gets
// its pong only once that sub's subscribe is held, and a message published the moment the pong
// arrives reaches the socket. The replica's link has a 400 ms round trip, as above. Control: a
// ping handler that answers at once, off the reader; the pong precedes the subscribe, the message
// is lost, and the row fails naming it.
func TestAPingAfterASubIsAnsweredAfterTheSubscribe(t *testing.T) {
	rig := newRelaySocketRig(t, withSocketNATSLatency(400*time.Millisecond))
	const session = "ses_edge_flush"
	client, _ := rig.open(t, session)
	client.sub(1, bus.DeliverSubject(session))
	client.ping()
	select {
	case <-client.pongs:
	case <-time.After(5 * time.Second):
		t.Fatal("no pong within 5 s of the ping")
	}
	sent := publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	select {
	case frame := <-client.frames:
		if frame.Op != "msg" || frame.Reply != sent.Reply {
			t.Fatalf("the socket sent %s, want the msg %s published the moment the pong arrived", frame.raw, sent.Reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the message %s published the moment the pong arrived never reached the socket: the pong came before the subscribe was held", sent.Reply)
	}
}

// Headers: msg.headers is always present, a JSON object mapping each header name as
// NATS carries it, its case kept, to every one of its values in order, and {} when the message
// carries none. Control: the headers read one value per name (Header.Get); the repeated values are
// lost and the row fails naming them.
func TestARelaySocketCarriesEveryValueOfAHeaderInOrder(t *testing.T) {
	rig := newRelaySocketRig(t)
	const session = "ses_edge_headers"
	client, _ := rig.open(t, session)
	client.sub(7, bus.DeliverSubject(session))
	client.ping()
	<-client.pongs

	want := natsgo.Header{"X-Repeated": {"first", "second", "third"}, "lower-case-Name": {"kept"}}
	publishRelayFrame(t, rig.owner.url, session, want)
	frame := client.next(5 * time.Second)
	if len(frame.Headers) != len(want) {
		t.Fatalf("the frame carries the headers %v, want exactly %v", frame.Headers, want)
	}
	for name, values := range want {
		if got := frame.Headers[name]; !slices.Equal(got, values) {
			t.Fatalf("the frame carries %s: %q, want %q in that order (all of %s)", name, got, values, frame.raw)
		}
	}

	publishRelayFrame(t, rig.owner.url, session, nil)
	bare := client.next(5 * time.Second)
	if !strings.Contains(bare.raw, `"headers":{}`) {
		t.Fatalf("a frame of a message with no headers is %s, want \"headers\":{}", bare.raw)
	}
}

// Row 9: a listener without ENVOY_EDGE_PROJECTS answers the relay socket's path 404, an upgrade
// included, through the same /v1 a deployment mounts. Control: the same path on the edge rig is
// upgraded.
func TestANonEdgeListenerServesNoRelaySocket(t *testing.T) {
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
