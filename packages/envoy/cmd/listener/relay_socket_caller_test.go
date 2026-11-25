package main

// relay_socket_caller_test.go holds the relay socket's in-process rows of what one caller takes
// from a replica and is handed: at most its cap of the replica's sockets (relaySocketMaxPerCaller)
// and its share of the replica's byte budget (relaySocketCallerPendingBytes), each given back once
// its sockets close, and the frames of its own session's receipts alone (relayReceiptOf). They run
// on the rig of relay_socket_fixture_test.go.

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// relaySocketOtherPerson is a second person, whose sockets the rows hold apart from the rig's.
const relaySocketOtherPerson = "another-outside@example.com"

// One person holds at most relaySocketMaxPerCaller sockets on a replica, over all of their
// sessions, and the next is refused 429, so other people still open theirs. The replica's cap here
// is one place more than that: one person opens their cap over four sessions of their own, their
// next socket is refused 429, and another person's socket opens in the last place and is sent its
// frame. Control: admit counting no caller; the person's next socket takes the last place, and the
// row fails naming it.
func TestAReplicaServesOnePersonNoMoreSocketsThanTheirCap(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withMaxSockets(relaySocketMaxPerCaller+1))
	sessionOf := func(i int) string { return "ses_edge_many_" + strconv.Itoa(i/relaySocketMaxPerSession) }
	for i := range relaySocketMaxPerCaller {
		rig.open(t, sessionOf(i))
	}
	next := sessionOf(relaySocketMaxPerCaller)
	token := "access-token-of-" + next
	rig.broker.Says(token, relaySocketAnswer(next, rig.caller.JKT(), time.Hour, relaySocketProject))
	if _, response, err := rig.dialAs(t, next, rig.upgradeHeader(t, next, token), relaySubprotocol); err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the person's socket past their cap of %d = %v (%s), want 429", relaySocketMaxPerCaller, err, describeUpgrade(response))
	}

	const other = "ses_edge_other_person"
	client, _ := rig.openAs(t, relaySocketOtherPerson, other)
	client.subscribeOwn(other)
	sent := publishRelayFrame(t, rig.owner.url, other, natsgo.Header{})
	if frame := client.next(5 * time.Second); frame.Op != "msg" || frame.Reply != sent.Reply {
		t.Fatalf("the other person's socket was sent %s, want the msg %s", frame.raw, sent.Reply)
	}
}

// What one person's sockets on a replica hold for their clients together stays within their share
// of its budget (relaySocketCallerPendingBytes; here 2 MiB of 8 MiB), so their slow clients cannot
// fill the budget another person's socket needs. The person's four sockets are held from writing:
// each pump's write waits on its socket's write lock, which the row holds, so every frame sent to
// them stays counted. They are sent 15 frames of 512 KiB, past their share and within the budget,
// and past the share their sockets close 1013 slow_consumer. Another person's socket, open all the
// while, is then sent one frame of 512 KiB, and receives it. Control: enqueue reserving nothing of
// the caller's share; the person's sockets hold all 15 frames, the other person's frame would take
// the replica past its budget, and the row fails naming that socket's close.
func TestOnePersonsSocketsHoldNoMoreThanTheirShareOfTheBudget(t *testing.T) {
	const budget, share = 8 << 20, 2 << 20
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withReplicaBudget(budget), withCallerBudget(share))
	const other = "ses_edge_share_other"
	client, _ := rig.openAs(t, relaySocketOtherPerson, other)
	client.subscribeOwn(other)

	sent := []int{4, 4, 4, 3}
	sockets := make([]*relaySocket, len(sent))
	for i := range sent {
		session := "ses_edge_share_" + strconv.Itoa(i)
		held, _ := rig.open(t, session)
		held.subscribeOwn(session)
		sockets[i] = rig.openSocket(t, session)
		sockets[i].writeMu.Lock()
		t.Cleanup(sync.OnceFunc(sockets[i].writeMu.Unlock))
	}
	for i, n := range sent {
		flood(t, rig.owner.url, sockets[i].session, n)
	}
	// Each socket has either taken every frame sent to it or closed.
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool {
		for i, socket := range sockets {
			if frames, _ := socket.held(); !socket.ended.Load() && frames != sent[i] {
				return false
			}
		}
		return true
	}) {
		t.Fatal("the person's sockets neither took the frames sent to them nor closed in time")
	}
	t.Logf("the person's sockets hold %d bytes of their %d share; the replica holds %d of its %d", sockets[0].caller.buffered.Load(), share, rig.sockets.buffered.Load(), budget)

	flood(t, rig.owner.url, other, 1)
	if frame := client.next(5 * time.Second); frame.Op != "msg" {
		t.Fatalf("the other person's socket was sent %s, want its msg", frame.raw)
	}
}

// A person who opened their relaySocketMaxPerCaller sockets on a replica and closed them all gets
// every place back (release), and opens another there; the replica keeps no record of them.
// Control: release giving back no place of the person's; the next socket is refused 429, and the
// row fails naming it.
func TestAPersonsClosedSocketsGiveTheirPlacesBack(t *testing.T) {
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour))
	sessionOf := func(i int) string { return "ses_edge_places_" + strconv.Itoa(i/relaySocketMaxPerSession) }
	var clients []*relaySocketClient
	for i := range relaySocketMaxPerCaller {
		client, _ := rig.open(t, sessionOf(i))
		clients = append(clients, client)
	}
	for _, client := range clients {
		_ = client.conn.Close()
	}
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool {
		for i := range relaySocketMaxPerCaller {
			if rig.holds(sessionOf(i)) != 0 {
				return false
			}
		}
		return true
	}) {
		t.Fatal("the replica still holds the person's sockets 10 s after their clients closed")
	}
	if records := rig.callerRecords(); records != 0 {
		t.Errorf("the replica keeps %d caller records after the person's %d sockets closed, want 0", records, relaySocketMaxPerCaller)
	}
	rig.open(t, "ses_edge_places_after")
}

// A frame the replica's budget refuses leaves nothing on its person's count: enqueue gives back
// the person's reservation of a frame the replica's count refused, so with the person's other
// socket idle and open, the person's count is 0 once the refused socket has closed and given back
// what it held. The refused socket's pump is held from writing (its write lock), so the frames it
// takes stay counted until the replica's 2 MiB budget refuses one. Control: enqueue keeping the
// person's reservation of a refused frame; the person's count stays at that frame's bytes, and the
// row fails naming it.
func TestAFrameTheReplicaRefusesLeavesNothingOnItsPerson(t *testing.T) {
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour), withPendingLimits(1<<16, 1<<30), withReplicaBudget(2<<20))
	const idle, session = "ses_edge_refund_idle", "ses_edge_refund_held"
	kept, _ := rig.open(t, idle)
	kept.subscribeOwn(idle)
	person := rig.openSocket(t, idle).caller

	held, _ := rig.open(t, session)
	held.subscribeOwn(session)
	socket := rig.openSocket(t, session)
	socket.writeMu.Lock()
	unlock := sync.OnceFunc(socket.writeMu.Unlock)
	t.Cleanup(unlock)
	flood(t, rig.owner.url, session, 6)
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, socket.ended.Load) {
		t.Fatalf("the held socket did not close in time; the replica holds %d bytes", rig.sockets.buffered.Load())
	}
	unlock()
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool { return rig.sockets.buffered.Load() == 0 }) {
		t.Fatalf("the replica still counts %d bytes 10 s after the held socket closed", rig.sockets.buffered.Load())
	}
	if got := person.buffered.Load(); got != 0 {
		t.Fatalf("the person's count is %d bytes with only an idle socket open, want 0: a frame the replica refused stayed on it", got)
	}
}

// A socket's place and its person's record are given back only once its pump has given back the
// frame it holds, so the person's count never leaves a closed socket's frame on a record the
// replica has forgotten. The row holds a socket's write lock, so its pump keeps one frame counted,
// and closes the client: while the pump holds that frame the replica keeps the socket's place and
// its person's record; once the pump lets the frame go, both are given back and the record holds
// nothing. Control: release run as soon as the reader returns (read waiting for no pump); the
// replica gives the socket's place back while the person's record still counts the frame, and the
// row fails naming it.
func TestARelaySocketGivesItsPlaceBackOnlyAfterItsPumpsLastFrame(t *testing.T) {
	rig := newRelaySocketRig(t, withCheckEvery(time.Hour))
	const session = "ses_edge_pump_last"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	socket := rig.openSocket(t, session)
	socket.writeMu.Lock()
	unlock := sync.OnceFunc(socket.writeMu.Unlock)
	t.Cleanup(unlock)
	publishRelayFrame(t, rig.owner.url, session, natsgo.Header{})
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool { frames, _ := socket.held(); return frames == 1 }) {
		t.Fatal("the socket took no frame within the time this package gives a wait")
	}
	_ = client.conn.Close()
	// The pump cannot let its frame go while the row holds the write lock, so with the fix no
	// release can come in this window; without it the release comes as soon as the reader ends.
	if testwait.Holds(time.Second, 10*time.Millisecond, func() bool { return rig.holds(session) == 0 }) {
		t.Fatalf("the replica gave the socket's place back while its pump still held a frame; the person's record counts %d bytes and the replica keeps %d caller records", socket.caller.buffered.Load(), rig.callerRecords())
	}
	unlock()
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool { return rig.holds(session) == 0 }) {
		t.Fatal("the replica still holds the socket 5 s after its pump let its frame go")
	}
	if got, records := socket.caller.buffered.Load(), rig.callerRecords(); got != 0 || records != 0 {
		t.Fatalf("after the socket was given back its person's record counts %d bytes and the replica keeps %d caller records, want 0 and 0", got, records)
	}
}

// A frame on a session's relay subject whose receipt names another session, or is no receipt of
// the shape an edge relay names (relayReceiptOf), is not forwarded: the socket hands its client
// only receipts of its own session's frames, so a frame anyone puts on the subject cannot have the
// client publish another session's receipt through the edge. The frames go out on one connection,
// those first and then a frame of the session's own, so a frame forwarded wrongly reaches the
// client first. Control: forward taking any receipt under the family (relayReceiptOf's session
// test removed); the first frame reaches the client, and the row fails naming it.
func TestARelaySocketForwardsNoFrameWhoseReceiptNamesAnotherSession(t *testing.T) {
	t.Parallel()
	rig := newRelaySocketRig(t)
	const session, other = "ses_edge_own_receipts", "ses_edge_someone_else"
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)

	publisher := testnats.Connect(t, rig.owner.url)
	// The strays carry the same relay header subject of the token's project as the session's own
	// frame, so the frame rule passes them and only the receipt check (relayReceiptOf) keeps them
	// from the client: without it the strays would be withheld by the frame rule whether or not
	// their receipt named another session, and the control below could not fail.
	strayHeader := natsgo.Header{relayHeaderSubject: {"notifications.dispatch.issue." + relaySocketProject + "." + relaySocketProject + "-1.comment.created"}}
	for _, reply := range []string{
		relayFrameReceipt(other),
		bus.RelayReplyPrefix + "edge-rows.rows." + session,
		relayFrameReceipt(session) + ".more",
	} {
		if err := publisher.PublishMsg(&natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: reply, Header: strayHeader, Data: []byte(`{"stray":true}`)}); err != nil {
			t.Fatalf("publish a frame whose receipt is %s: %v", reply, err)
		}
	}
	own := &natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: relayFrameReceipt(session), Data: []byte(`{"own":true}`),
		Header: natsgo.Header{relayHeaderSubject: {"notifications.dispatch.issue." + relaySocketProject + "." + relaySocketProject + "-1.comment.created"}}}
	if err := publisher.PublishMsg(own); err != nil {
		t.Fatalf("publish the session's own frame: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the frames: %v", err)
	}
	if frame := client.next(5 * time.Second); frame.Op != "msg" || frame.Reply != own.Reply {
		t.Fatalf("the socket sent %s first, want the msg %s of its own session: a frame whose receipt is no receipt of the session was forwarded", frame.raw, own.Reply)
	}
	client.none(500*time.Millisecond, "after the session's own frame")
}
