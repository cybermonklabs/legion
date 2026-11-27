package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lostRequestWait is the deadline the rows below give the request whose connection is cut: the
// listener's sessionConsumerCallTimeout, the wait a relay restart's calls hold, which is what a
// request lost with its connection used to cost in full.
const lostRequestWait = 5 * time.Second

// lostRequestAfterReconnect bounds how long after the reconnect a request lost with its
// connection may take to fail: the client's connection event and one check of the connection,
// with room for a loaded test machine, and far inside lostRequestWait.
const lostRequestAfterReconnect = 500 * time.Millisecond

// awaitRelaySent waits until relay has counted want sends of its subject since it was armed.
func awaitRelaySent(t *testing.T, relay *testnats.UnansweredRelay, want int) {
	t.Helper()
	deadline := time.Now().Add(lostRequestWait)
	for relay.Sent() < want {
		if time.Now().After(deadline) {
			t.Fatalf("the relay counted %d sends of %s, want %d", relay.Sent(), relay.Subject(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitFlushedSinceDrop waits until the server's answer to the flush behind the request relay
// dropped is in the client's socket (testnats.UnansweredRelay.FlushedSinceDrop), so a cut made
// after it finds the publish confirmed and the call waiting on its reply rather than on its flush,
// which nats.go would fail at the cut on its own.
func awaitFlushedSinceDrop(t *testing.T, relay *testnats.UnansweredRelay) {
	t.Helper()
	deadline := time.Now().Add(lostRequestWait)
	for !relay.FlushedSinceDrop() {
		if time.Now().After(deadline) {
			t.Fatalf("the server did not answer the flush behind the dropped %s within %s", relay.Subject(), lostRequestWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// lostAnswer is what a call whose request a row lost answered, and when.
type lostAnswer struct {
	err error
	at  time.Time
}

// cutAndAwaitReconnect cuts every link of relay once its dropped request is out, and returns when
// conn has reconnected through it.
func cutAndAwaitReconnect(t *testing.T, relay *testnats.UnansweredRelay, conn *nats.Conn) time.Time {
	t.Helper()
	before := conn.Stats().Reconnects
	relay.Drop()
	deadline := time.Now().Add(lostRequestWait)
	for conn.Stats().Reconnects == before {
		if time.Now().After(deadline) {
			t.Fatalf("the connection did not reconnect through the relay within %s", lostRequestWait)
		}
		time.Sleep(time.Millisecond)
	}
	return time.Now()
}

// requireLostAtTheReconnect requires the call to have answered ErrRequestLostWithConnection
// within lostRequestAfterReconnect of the reconnect.
func requireLostAtTheReconnect(t *testing.T, what string, answers <-chan lostAnswer, reconnected time.Time) {
	t.Helper()
	var answer lostAnswer
	select {
	case answer = <-answers:
	case <-time.After(lostRequestWait + time.Second):
		t.Fatalf("%s whose connection was cut has not answered %s after the reconnect", what, lostRequestWait+time.Second)
	}
	after := answer.at.Sub(reconnected)
	if !errors.Is(answer.err, ErrRequestLostWithConnection) || after > lostRequestAfterReconnect {
		t.Fatalf("%s whose connection was cut under it answered %v, %s after the reconnect; want %v within %s of it, not the %s deadline",
			what, answer.err, after.Round(time.Millisecond), ErrRequestLostWithConnection, lostRequestAfterReconnect, lostRequestWait)
	}
}

// loseWithTheConnection arms relay to drop the next send of its subject, makes call within bound on
// conn's client, and cuts relay's links once that send is out: the order of a request whose bytes
// reached the socket and then died with it. It returns when call began, when conn reconnected, and
// where call's answer comes.
func loseWithTheConnection(t *testing.T, relay *testnats.UnansweredRelay, conn *nats.Conn, bound time.Duration, call func(context.Context) error) (began, reconnected time.Time, answers <-chan lostAnswer) {
	t.Helper()
	relay.Arm(1)
	began = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	t.Cleanup(cancel)
	answered := make(chan lostAnswer, 1)
	go func() {
		err := call(ctx)
		answered <- lostAnswer{err: err, at: time.Now()}
	}()
	awaitRelaySent(t, relay, 1)
	return began, cutAndAwaitReconnect(t, relay, conn), answered
}

// requireLostWithTheConnection makes call within lostRequestWait on conn's client, loses its
// request with the connection (loseWithTheConnection), and requires call to answer
// ErrRequestLostWithConnection within lostRequestAfterReconnect of the reconnect. It suits a call
// whose request waits on nats.go's response subscription with no flush behind it - a KV read or
// write, a JetStream publish - which nats.go fails at no cut, so the one thing that can end it
// early is the connection watch.
func requireLostWithTheConnection(t *testing.T, relay *testnats.UnansweredRelay, conn *nats.Conn, what string, call func(context.Context) error) {
	t.Helper()
	_, reconnected, answers := loseWithTheConnection(t, relay, conn, lostRequestWait, call)
	requireLostAtTheReconnect(t, what, answers, reconnected)
}

// lostReadBound is the bound the reads requireLostBeforeHalfItsBound loses are held to: well above
// the reconnect the row cuts its connection into (nats.go dials the same server again after its
// ReconnectWait, 1 s), which the row requires to take at most a quarter of it, so a read that fails
// before half of it failed at the reconnect and not at its bound. roles.Buckets holds its reads to
// 2 s (readTimeout), which leaves no such margin over that second; the takeover records and the
// kept lanes are read within their caller's context, the claim route's handover window of 10
// minutes among them.
const lostReadBound = 8 * time.Second

// requireLostBeforeHalfItsBound makes call within bound on conn's client, loses its request with
// the connection (loseWithTheConnection), and requires the reconnect to come within a quarter of
// bound from call's start and call to answer ErrRequestLostWithConnection before half of it: a call
// that fails that soon failed at the reconnect, and one that waited for its bound fails the row.
func requireLostBeforeHalfItsBound(t *testing.T, relay *testnats.UnansweredRelay, conn *nats.Conn, what string, bound time.Duration, call func(context.Context) error) {
	t.Helper()
	began, reconnected, answers := loseWithTheConnection(t, relay, conn, bound, call)
	if took := reconnected.Sub(began); took > bound/4 {
		t.Fatalf("the connection reconnected %s after %s began, past a quarter of its %s bound: the row cannot tell a call that failed at the reconnect from one that failed at its bound",
			took.Round(time.Millisecond), what, bound)
	}
	var answer lostAnswer
	select {
	case answer = <-answers:
	case <-time.After(bound + time.Second):
		t.Fatalf("%s whose connection was cut has not answered %s after it began", what, bound+time.Second)
	}
	if took := answer.at.Sub(began); !errors.Is(answer.err, ErrRequestLostWithConnection) || took >= bound/2 {
		t.Fatalf("%s whose connection was cut under it answered %v, %s after it began and %s after the reconnect; want %v before half its %s bound",
			what, answer.err, took.Round(time.Millisecond), answer.at.Sub(reconnected).Round(time.Millisecond), ErrRequestLostWithConnection, bound)
	}
}

// A consumer call whose request went out, and whose connection is then cut before the answer
// comes, fails with ErrRequestLostWithConnection at the reconnect rather than waiting out its
// deadline. The relay drops the call's request but passes the flush behind it, so the publish
// is confirmed and the call is waiting on its reply when the link is cut: the order a relay
// restart met on a pod whose link died, where nats.go wakes no sync subscription and the call
// waited the restart's whole 5 s.
// It must not run in parallel: the call must fail within 500 ms of the reconnect it times.
func TestAConsumerCallLostWithItsConnectionFailsAtTheReconnect(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	const session = "ses_call_lost_with_connection"
	name, ok := SessionConsumerName(session)
	if !ok {
		t.Fatalf("no consumer name for %s", session)
	}
	relay := testnats.StartUnansweredRelayOn(t, uri, consumerAPI("INFO", name))
	consumers, err := ConnectSessionConsumers([]string{relay.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers through the relay: %v", err)
	}
	t.Cleanup(consumers.Close)
	conn := consumers.client.Connection()

	relay.Arm(1)
	ctx, cancel := context.WithTimeout(context.Background(), lostRequestWait)
	defer cancel()
	answers := make(chan lostAnswer, 1)
	go func() {
		_, err := consumers.Info(ctx, session)
		answers <- lostAnswer{err: err, at: time.Now()}
	}()
	awaitRelaySent(t, relay, 1)
	awaitFlushedSinceDrop(t, relay)
	reconnected := cutAndAwaitReconnect(t, relay, conn)

	requireLostAtTheReconnect(t, "a consumer info", answers, reconnected)
}

// A KV read waited for through BoundedOn, the read a relay restart makes of the session's interest
// row (store.Registry.RefreshAt), fails with ErrRequestLostWithConnection at the reconnect when
// its connection is cut under it. nats.go sends the read on its shared response subscription and
// fails it only at its own MaxWait (10 s), and only ForceReconnect or a close clears that
// subscription's waiting requests, so a read sent into a socket that then died waited out its
// caller's whole deadline.
// It must not run in parallel: the read must fail within 500 ms of the reconnect it times.
func TestAKeyValueReadLostWithItsConnectionFailsAtTheReconnect(t *testing.T) {
	const (
		bucket = "envoy_read_lost_with_connection"
		key    = "ses_read_lost"
	)
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$JS.API.DIRECT.GET.KV_"+bucket+".$KV."+bucket+"."+key)
	conn, js := relayedJetStream(t, relay)
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	if _, err := kv.Put(key, []byte("row")); err != nil {
		t.Fatalf("write the key the row reads: %v", err)
	}

	requireLostWithTheConnection(t, relay, conn, "a KV read", func(ctx context.Context) error {
		_, _, err := BoundedOn(ctx, kv, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		return err
	})

	// The read made again on the connection that came back is answered.
	entry, answered, err := BoundedOn(context.Background(), kv, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
	if err != nil || !answered || string(entry.Value()) != "row" {
		t.Fatalf("the read again after the reconnect = %v, answered %t; want the row", err, answered)
	}
}

// A read through BoundedOn waits for no other client's connection. The listener's self-health
// probe sends its role lane's subscription again whenever it finds the lane inactive, as it does
// while the lane's connection reconnects (cmd/listener's checkRoleLane), and that send
// (Client.ResendRefusedCore) reads the lane's connection status under the client's own lock and
// through nats.go's connection lock, which nats.go holds through a reconnect's dial and handshake.
// Client.CoreSubOK, which /healthz asks of the lane, and the recovery's subscriptionsHealthy read
// that status with the client's lock released (connOpen), so neither waits under it. Finding the
// client that owns a read's connection (clientOwning) takes each live client's lock in turn, so a
// read that took its connection watch before its call started waited out the role lane's held
// handshake, up to its connect timeout (5 s), past its own bound: the role lane reads a claim
// within 2 s (cmd/listener's roleHolderReadTimeout). Here another client's reconnect handshake is
// held (testnats.HandshakeHold) and its ResendRefusedCore holds that client's lock meanwhile, and
// the read must answer within that handshake attempt. It reads through a handle on a connection no
// client owns, for which that search passes every live client: a handle whose own client the
// search found first would end it before the held one on some reads and not on others.
// It must not run in parallel: its read's watch holds liveClientsMu, the process's, up to 5 s.
func TestAReadThroughBoundedOnWaitsForNoOtherClientsHeldHandshake(t *testing.T) {
	const (
		bucket = "envoy_read_beside_held_handshake"
		key    = "ses_read_beside"
	)
	conn := testnats.Connect(t, testnats.URL(t))
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	if _, err := kv.Put(key, []byte("row")); err != nil {
		t.Fatalf("write the key the row reads: %v", err)
	}

	_, url := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, url)
	lane, err := Connect([]string{hold.URL()})
	if err != nil {
		t.Fatalf("connect the other client through the handshake hold: %v", err)
	}
	t.Cleanup(lane.Close)
	t.Cleanup(hold.Release)
	laneConn := lane.Connection()
	hold.HoldReconnect(t, laneConn)
	// The self-health probe's send, made while the lane reconnects, holds the lane's lock until the
	// held attempt ends; resent closes once it returns.
	resent := make(chan struct{})
	go func() {
		_ = lane.ResendRefusedCore()
		close(resent)
	}()
	// The other client's lock is held by its ResendRefusedCore once it stays taken across two
	// looks 50 ms apart: every other hold of it lasts microseconds.
	deadline := time.Now().Add(lostRequestWait)
	for taken := 0; taken < 2; time.Sleep(50 * time.Millisecond) {
		select {
		case <-resent:
			t.Fatal("the other client's ResendRefusedCore returned during its held handshake: it did not hold its lock")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the other client's ResendRefusedCore did not hold its lock within %s of its held handshake", lostRequestWait)
		}
		if !lane.mu.TryLock() {
			taken++
			continue
		}
		lane.mu.Unlock()
		taken = 0
	}

	answers := hold.AnswersWithinAttempt(t, laneConn, "a read through BoundedOn beside another client's held handshake", func() error {
		entry, answered, err := BoundedOn(t.Context(), kv, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		if err == nil && (!answered || string(entry.Value()) != "row") {
			return errors.New("the read answered no row")
		}
		return err
	})
	if answers[0] != nil {
		t.Fatalf("a read through BoundedOn beside another client's held handshake: %v", answers[0])
	}
}

// A role read waited for through BoundedOnConnection, the wait roles.Buckets holds each of its
// reads to (within), fails with ErrRequestLostWithConnection at the reconnect when its connection
// is cut under it once its watch is taken, rather than waiting out its bound: Dispatch's outbox
// makes two such reads to resolve a role's holder, and a hub link that drops under one cost the
// resolution that read's whole bound. The read is the one Buckets makes - a direct get of the
// role's claim on a handle opened on the client's connection - held to lostReadBound; the relay
// drops its request, and the cut comes once that request is out, by when the watch, which starts
// beside the call, has read the connection's reconnect count. The reconnect takes at most a
// quarter of the bound, and the read fails before half of it.
// It must not run in parallel: the reconnect must come within 2 s, a quarter of lostReadBound.
func TestARoleReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const role = "reviewer"
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$JS.API.DIRECT.GET.KV_"+RoleClaimBucket+".$KV."+RoleClaimBucket+"."+role)
	client, err := Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	t.Cleanup(func() { _ = js.DeleteKeyValue(RoleClaimBucket) })
	if _, err := EnsureKeyValue(t.Context(), client.Connection(), js, &nats.KeyValueConfig{Bucket: RoleClaimBucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	kv, err := OpenKeyValue(t.Context(), client.Connection(), js, RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket on the client's connection: %v", err)
	}
	if _, err := kv.Put(role, []byte(`{"holder_session_id":"ses_role_read_lost"}`)); err != nil {
		t.Fatalf("write the claim the row reads: %v", err)
	}
	read := func(ctx context.Context) (nats.KeyValueEntry, bool, error) {
		return BoundedOnConnection(ctx, client, func() (nats.KeyValueEntry, error) { return kv.Get(role) })
	}

	requireLostBeforeHalfItsBound(t, relay, client.Connection(), "a role read", lostReadBound, func(ctx context.Context) error {
		_, _, err := read(ctx)
		return err
	})

	// The read made again on the connection that came back is answered.
	ctx, cancel := context.WithTimeout(context.Background(), lostReadBound)
	defer cancel()
	if entry, answered, err := read(ctx); err != nil || !answered || entry.Revision() == 0 {
		t.Fatalf("the role read again after the reconnect = %v, answered %t; want the claim", err, answered)
	}
}

// A leader read waited for through BoundedOn fails with ErrRequestLostWithConnection at the
// reconnect when its connection is cut under it, rather than waiting out its bound. The
// listener's leader reads run so, internal/cistore's two aside, among them: the delete of a
// session's entry and its conditional delete (session.SessionRegistry.Delete and DeleteIf), the
// delete of a session's interest row (store's deleteInterest), each read of a session's record
// through session.SessionRegistry.RefreshFromLeader (before its consumer is deleted, in the
// interest and role claim reapers' test of a session the cache does not hold, in a soft claim's
// reads of the holder it would take the role from, in the claim route's read of its claimant,
// and in the registration view of GET /v1/registry/<id>, whose caller decides a delete on the
// client instance id it reads), the read of a role's claim that the session relay judges a role
// message by and the kept lane's handover resolves the role's holder with
// (store.Registry.RoleClaimFromLeader), and the read of a controller role's claim that decides
// whether a session's consumer filter keeps the role's notice lane (store's roleClaimFromLeader,
// under store.Registry.ReadLaneHolders).
// cistore's two, the reads of a CI record main's #5132 added (Store.casState and
// Store.ClaimStillHeld), are not waited for through BoundedOn: each passes context.Background, so
// RequestTimeout alone bounds it (GetFromLeader), and one whose request died with its connection
// waits all of that bound out. A leader read is a stream message get,
// `$JS.API.STREAM.MSG.GET.KV_<bucket>` (NewestFromLeader), whose answer nats.go waits for on its
// shared response subscription, and nats.go fails no request its connection drops, so a bare
// leader read whose request died with the old socket waits out its whole bound. The read is
// GetFromLeader of a role's claim on a handle opened on the client's connection, held to
// lostReadBound; the relay drops its request, and the cut comes once that request is out. The
// reconnect takes at most a quarter of the bound, and the read fails before half of it.
// It must not run in parallel: the reconnect must come within 2 s, a quarter of lostReadBound.
func TestALeaderReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		role  = "reviewer"
		claim = `{"holder_session_id":"ses_leader_read_lost"}`
	)
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$JS.API.STREAM.MSG.GET.KV_"+RoleClaimBucket)
	client, err := Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	t.Cleanup(func() { _ = js.DeleteKeyValue(RoleClaimBucket) })
	if _, err := EnsureKeyValue(t.Context(), client.Connection(), js, &nats.KeyValueConfig{Bucket: RoleClaimBucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	kv, err := OpenKeyValue(t.Context(), client.Connection(), js, RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket on the client's connection: %v", err)
	}
	if _, err := kv.Put(role, []byte(claim)); err != nil {
		t.Fatalf("write the claim the row reads: %v", err)
	}
	read := func(ctx context.Context) (nats.KeyValueEntry, bool, error) {
		return BoundedOn(ctx, kv, func() (nats.KeyValueEntry, error) { return kv.GetFromLeader(ctx, role) })
	}

	requireLostBeforeHalfItsBound(t, relay, client.Connection(), "a leader read", lostReadBound, func(ctx context.Context) error {
		_, _, err := read(ctx)
		return err
	})

	// The read made again on the connection that came back is answered.
	ctx, cancel := context.WithTimeout(context.Background(), lostReadBound)
	defer cancel()
	if entry, answered, err := read(ctx); err != nil || !answered || string(entry.Value()) != claim {
		t.Fatalf("the leader read again after the reconnect = %v, answered %t; want the claim", err, answered)
	}
}

// streamInfoThroughRelay connects the stream's owner to the test server directly, so the stream
// exists, and a client through a relay that drops the stream's STREAM.INFO once armed. The client
// owns no stream, so no ensure sends a STREAM.INFO of its own at the reconnect, and it reads the
// deployed subjects at its connect alone, before the row arms the relay.
func streamInfoThroughRelay(t *testing.T) (*testnats.UnansweredRelay, *Client) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	relay := testnats.StartUnansweredRelayOn(t, uri, streamInfoSubject)
	client, err := Connect([]string{relay.URL()}, WithStreamSubjectsRefresh(time.Hour))
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	return relay, client
}

// A read of the takeover records (Client.TakeoverRecords) fails with ErrRequestLostWithConnection
// at the reconnect when its connection is cut under its STREAM.INFO, rather than waiting out its
// bound: the claim route's handover makes that read under the role's guard and within its 10-minute
// window (cmd/listener's runTakenRoleDrain), and nats.go fails no request its connection drops, so
// a read whose answer died with the old socket held the guard for that whole window. The relay
// drops the STREAM.INFO, and the cut comes once it is out. The reconnect takes at most a quarter
// of the read's bound, and the read fails before half of it.
// It must not run in parallel: the reconnect must come within 2 s, a quarter of lostReadBound.
func TestATakeoverRecordsReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	relay, client := streamInfoThroughRelay(t)
	requireLostBeforeHalfItsBound(t, relay, client.Connection(), "a read of the takeover records", lostReadBound, func(ctx context.Context) error {
		_, err := client.TakeoverRecords(ctx, "")
		return err
	})

	// The read made again on the connection that came back is answered.
	ctx, cancel := context.WithTimeout(context.Background(), lostReadBound)
	defer cancel()
	if _, err := client.TakeoverRecords(ctx, ""); err != nil {
		t.Fatalf("the read of the takeover records again after the reconnect = %v, want an answer", err)
	}
}

// A read of the kept lanes (Client.KeptRoleLanes), the kept lane's sweep's first read of each pass,
// fails with ErrRequestLostWithConnection at the reconnect when its connection is cut under its
// STREAM.INFO, rather than waiting out its bound, as the read of the takeover records does. The
// reconnect takes at most a quarter of the read's bound, and the read fails before half of it.
// It must not run in parallel: the reconnect must come within 2 s, a quarter of lostReadBound.
func TestAKeptLanesReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	relay, client := streamInfoThroughRelay(t)
	requireLostBeforeHalfItsBound(t, relay, client.Connection(), "a read of the kept lanes", lostReadBound, func(ctx context.Context) error {
		_, err := client.KeptRoleLanes(ctx)
		return err
	})

	// The read made again on the connection that came back is answered.
	ctx, cancel := context.WithTimeout(context.Background(), lostReadBound)
	defer cancel()
	if _, err := client.KeptRoleLanes(ctx); err != nil {
		t.Fatalf("the read of the kept lanes again after the reconnect = %v, want an answer", err)
	}
}

// reconnectAfterTheWatch is a link a row cuts through a proxy that refuses every dial while it is
// cut, so a watch reads the disconnect before any reconnect can come, and restores from the first
// watch that goes on past an event that left its request unlost (went, the client's
// WithWatchWentOn), which then waits for the reconnect's hooks (hook, the client's reconnect hook):
// those run after the client reported the reconnect's event (queueReconnect), so that watch goes
// on only once the event has come.
type reconnectAfterTheWatch struct {
	proxy       *testnats.Proxy
	armed       atomic.Bool
	restore     sync.Once
	heard       sync.Once
	reconnected chan struct{}
	// at is when the reconnect's hooks ran, written before reconnected is closed.
	at time.Time
}

func newReconnectAfterTheWatch(t *testing.T, uri string) *reconnectAfterTheWatch {
	t.Helper()
	return &reconnectAfterTheWatch{proxy: testnats.StartProxy(t, uri), reconnected: make(chan struct{})}
}

func (r *reconnectAfterTheWatch) went() {
	if !r.armed.Load() {
		return
	}
	r.restore.Do(func() {
		r.proxy.Restore()
		select {
		case <-r.reconnected:
		case <-time.After(lostRequestWait):
		}
	})
}

func (r *reconnectAfterTheWatch) hook(context.Context, *nats.Conn) error {
	if r.armed.Load() {
		r.heard.Do(func() {
			r.at = time.Now()
			close(r.reconnected)
		})
	}
	return nil
}

// cut cuts the link and returns when the reconnect's hooks have run: the link comes back only once
// a watch has gone on past the disconnect (went).
func (r *reconnectAfterTheWatch) cut(t *testing.T) time.Time {
	t.Helper()
	r.armed.Store(true)
	r.proxy.Cut()
	select {
	case <-r.reconnected:
		return r.at
	case <-time.After(2 * lostRequestWait):
		t.Fatalf("the link was not restored within %s of the cut: no watch went on past the disconnect", 2*lostRequestWait)
		return time.Time{}
	}
}

// A request whose watch wakes at the disconnect and reads its connection unchanged, the reconnect
// not yet come, fails with ErrRequestLostWithConnection at that reconnect: after each event the
// watch takes the channel it waits on next before it reads the connection (connWatch.bound), so
// the reconnect's event, which closes the channel taken when it comes and none when none is taken
// (Client.connectionChangedLocked), closes the one the watch waits on. The request goes to a
// subscriber that never answers, and the reconnect lands just after the watch's read
// (reconnectAfterTheWatch).
//
// A watch that read the connection first and took its next channel after took it once the
// reconnect had reported its event to no channel, and waited on it for an event that had already
// come: the request waited out its whole deadline.
// It must not run in parallel: the request must fail within 500 ms of the reconnect it times.
func TestARequestWhoseWatchReadTheDisconnectFailsAtTheReconnectThatFollows(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	const subject = "request-lost.unanswered"
	silent := testnats.Connect(t, uri)
	asked := make(chan struct{}, 1)
	if _, err := silent.Subscribe(subject, func(*nats.Msg) {
		select {
		case asked <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe the subscriber that never answers: %v", err)
	}
	if err := silent.Flush(); err != nil {
		t.Fatalf("flush the subscriber that never answers: %v", err)
	}
	link := newReconnectAfterTheWatch(t, uri)
	client, err := Connect([]string{link.proxy.URL()}, WithStreamSubjectsRefresh(time.Hour), WithWatchWentOn(link.went))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	client.AddReconnectHook(link.hook)
	conn := client.Connection()

	ctx, cancel := context.WithTimeout(context.Background(), lostRequestWait)
	defer cancel()
	answers := make(chan lostAnswer, 1)
	go func() {
		_, err := RequestOnConnection(ctx, client, func(ctx context.Context) (*nats.Msg, error) {
			return conn.RequestWithContext(ctx, subject, nil)
		})
		answers <- lostAnswer{err: err, at: time.Now()}
	}()
	select {
	case <-asked:
	case <-time.After(lostRequestWait):
		t.Fatalf("the request never reached the subscriber that never answers")
	}
	reconnected := link.cut(t)

	requireLostAtTheReconnect(t, "a request to a subscriber that never answers", answers, reconnected)
}

// A pull whose watch wakes at the disconnect and reads its connection unchanged, the reconnect not
// yet come, ends with ErrSessionPullReconnected at that reconnect, rather than at the silence of
// twice its heartbeat: the pull's watch takes the channel it waits on next before it reads the
// connection (connWatch.lost), as a request's does (connWatch.bound), and the reconnect lands just
// after that read (reconnectAfterTheWatch).
//
// A watch that read the connection first and took its next channel after waited on a channel the
// reconnect had already passed by, and the pull went on waiting for a request the server had
// dropped with the old connection.
// It must not run in parallel: the pull must end within 500 ms of the reconnect it times.
func TestAPullWhoseWatchReadTheDisconnectEndsAtTheReconnectThatFollows(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	link := newReconnectAfterTheWatch(t, uri)
	consumers, err := ConnectSessionConsumers([]string{link.proxy.URL()}, 0, nil, WithWatchWentOn(link.went))
	if err != nil {
		t.Fatalf("connect session consumers through the proxy: %v", err)
	}
	t.Cleanup(consumers.Close)
	consumers.client.AddReconnectHook(link.hook)
	const session = "ses_pull_watch_order"
	agent := contracts.LegacyAgentSubject(session)
	if _, err := consumers.Ensure(context.Background(), session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	pull := pullSession(t, consumers, session)
	// A message taken shows the pull's request is out and answered on the connection the row
	// cuts next.
	before := publishStored(t, owner, agent, "before the cut")
	if msg := next(t, pull); msg.StreamSeq != before {
		t.Fatalf("before the cut got %d, want %d", msg.StreamSeq, before)
	} else if err := msg.Ack(); err != nil {
		t.Fatalf("ack %d: %v", msg.StreamSeq, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lostRequestWait)
	defer cancel()
	answers := make(chan lostAnswer, 1)
	go func() {
		_, err := pull.Next(ctx)
		answers <- lostAnswer{err: err, at: time.Now()}
	}()
	reconnected := link.cut(t)

	var answer lostAnswer
	select {
	case answer = <-answers:
	case <-time.After(lostRequestWait + time.Second):
		t.Fatalf("the pull whose connection was cut has not answered %s after the reconnect", lostRequestWait+time.Second)
	}
	if after := answer.at.Sub(reconnected); !errors.Is(answer.err, ErrSessionPullReconnected) || after > lostRequestAfterReconnect {
		t.Fatalf("the pull whose connection was cut under it answered %v, %s after the reconnect; want %v within %s of it",
			answer.err, after.Round(time.Millisecond), ErrSessionPullReconnected, lostRequestAfterReconnect)
	}
}
