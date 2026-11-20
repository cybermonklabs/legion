package bus

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

// unansweredBucket is the bucket the handle-open rows below open a jetstream handle on, created
// afresh by each of them on the package's shared server and removed when it ends.
const unansweredBucket = "envoy_unanswered_test"

// openSubject is the request a lookup of a bucket sends: the STREAM.INFO of the bucket's stream,
// which nats.go builds from the bucket's name for the legacy js.KeyValue (nats.go@v1.55.0
// kv.go:397-418, js.go:193-199) and for the jetstream handle wrapKeyValue opens on it.
func openSubject(bucket string) string { return "$JS.API.STREAM.INFO.KV_" + bucket }

// createSubject is the request a create of a bucket sends: the STREAM.CREATE of the bucket's
// stream, which nats.go builds from the bucket's name for js.CreateKeyValue (nats.go@v1.55.0
// kv.go:422-548, js.go:193-199).
func createSubject(bucket string) string { return "$JS.API.STREAM.CREATE.KV_" + bucket }

// updateSubject is the request an update of a bucket's stream sends, a raise or a new age: the
// STREAM.UPDATE of the bucket's stream (nats.go@v1.55.0 jsm.go's UpdateStream).
func updateSubject(bucket string) string { return "$JS.API.STREAM.UPDATE.KV_" + bucket }

// accountInfoSubject is the request js.CreateKeyValue sends before its STREAM.CREATE: the
// account's JetStream info, which nats.go asks for first (nats.go@v1.55.0 kv.go:422-434,
// jsm.go:409). It names no bucket, so a row that drops it drops the account info of whatever
// this connection asks the server for next, and only a row that makes one call may use it.
func accountInfoSubject() string { return "$JS.API.INFO" }

// unansweredRelay is a TCP relay between a client and a real NATS server that counts the requests
// the client sends on one subject and drops the first drops of them: the request never reaches the
// server, as a member of a metadata group younger than lostQuorumInterval, or one that has not
// yet seen its leader go, leaves one unanswered (readAgain), so no answer is ever published and
// the caller's wait runs out with the deadline it held. Everything else both sides send is
// relayed untouched, so the connection is a real one on a real server and every other request is
// answered as it always is.
//
// The requests a start makes of a bucket are nats.go's own: the lookup and the create are sent
// inside js.KeyValue and js.CreateKeyValue, and the jetstream handle wrapKeyValue opens is built
// from the connection inside that function (jetstream.New(conn)), which takes the concrete
// *nats.Conn. So no JetStream context a test passes in can reach any of those sends: the seam a
// test has for them is the connection's own wire, which is this relay. The bucket's own name is
// in openSubject and in createSubject, so a row dropping one of those touches only the requests
// about its own bucket; accountInfoSubject holds no bucket name, so only a row that makes one
// call may drop it. The sends a row's setup makes are counted apart from the sends of the call
// it is about (arm). A relay told to hold its drops (holdNext) passes each of them on late instead,
// as a request that waits in the server's JetStream API queue reaches it late.
type unansweredRelay struct {
	subject  string
	url      string
	listener net.Listener
	mu       sync.Mutex
	pass     int
	drops    int
	hold     time.Duration
	sends    int
	// written serializes what the relay writes to the server, a held send among it.
	written sync.Mutex
}

// startUnansweredRelayOn relays to the NATS server at target, a client URL such as testnats.URL
// returns, on a loopback port of its own until the test ends, counting and dropping the client's
// sends on subject: openSubject or createSubject of the row's bucket, or accountInfoSubject,
// which names no bucket and so is for a row that makes one call. Its own URL is that one with
// the host and port it listens on, credentials included, as testnats.StartProxy builds its own. It
// drops nothing until it is armed.
func startUnansweredRelayOn(t *testing.T, target, subject string) *unansweredRelay {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the relay relays to: %v", target, err)
		return nil
	}
	listener, err := testport.Listen()
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
		return nil
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &unansweredRelay{
		subject:  subject,
		url:      relayed.String(),
		listener: listener,
	}
	t.Cleanup(func() { _ = listener.Close() })
	go relay.serve(server.Host)
	return relay
}

// arm has the relay drop the next drops sends of its subject and counts the sends from now, so a
// row counts the sends of the call it is about to make and not the ones that set it up.
func (r *unansweredRelay) arm(drops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pass = 0
	r.drops = drops
	r.hold = 0
	r.sends = 0
}

// passThenDrop lets the next pass sends of the subject through and drops the drops after them,
// counting the sends from now. A row that drives a whole start needs this where arm does not:
// EnsureKeyValue looks its bucket up with the same STREAM.INFO on the same subject as the handle
// open, so a relay armed to drop the first send would drop the lookup instead of the open.
func (r *unansweredRelay) passThenDrop(pass, drops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pass = pass
	r.drops = drops
	r.hold = 0
	r.sends = 0
}

// holdNext has the relay hold the next holds sends of its subject for hold each and pass them on
// then, counting the sends from now: the server applies and answers each one hold after it was
// sent, as it does a request that waited that long in its JetStream API queue.
func (r *unansweredRelay) holdNext(holds int, hold time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pass = 0
	r.drops = holds
	r.hold = hold
	r.sends = 0
}

// sent is how many sends of its subject the client has made since the relay was armed.
func (r *unansweredRelay) sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

// send records one send of subject and reports whether it is dropped, and for how long it is held
// first when it is held rather than dropped: it is while drops are left, and each such send spends
// one.
func (r *unansweredRelay) send(subject string) (dropped bool, held time.Duration) {
	if subject != r.subject {
		return false, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	if r.pass > 0 {
		r.pass--
		return false, 0
	}
	if r.drops > 0 {
		r.drops--
		return true, r.hold
	}
	return false, 0
}

// write passes line, and the payload after it when there is one, on to server, whole: a held send
// is written from a goroutine of its own, beside the relay's own writes.
func (r *unansweredRelay) write(server net.Conn, line string, payload []byte) error {
	r.written.Lock()
	defer r.written.Unlock()
	if _, err := io.WriteString(server, line); err != nil {
		return err
	}
	_, err := server.Write(payload)
	return err
}

// serve joins each connection the relay accepts to the server until the listener closes.
func (r *unansweredRelay) serve(target string) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()
			continue
		}
		go r.filter(server, client)
		go func() {
			_, _ = io.Copy(client, server)
			_ = client.Close()
			_ = server.Close()
		}()
	}
}

// filter relays what the client sends to the server, a protocol message at a time, leaving out the
// publishes the relay drops and passing on the ones it holds once their hold has passed. A client
// sends only CONNECT, SUB, UNSUB, PING, PONG and publishes, so every line that is not a publish is
// passed on as it is, and a publish is passed on with the payload its header counts.
func (r *unansweredRelay) filter(server, client net.Conn) {
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		// `PUB <subject> [reply] <bytes>` and `HPUB <subject> [reply] <header bytes> <bytes>` are
		// each followed by that many bytes and a CRLF. A line with fewer than three fields is
		// neither, whatever its first word says.
		if len(fields) < 3 || (!strings.EqualFold(fields[0], "PUB") && !strings.EqualFold(fields[0], "HPUB")) {
			if err := r.write(server, line, nil); err != nil {
				return
			}
			continue
		}
		size, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if dropped, held := r.send(fields[1]); dropped {
			if held > 0 {
				time.AfterFunc(held, func() { _ = r.write(server, line, payload) })
			}
			continue
		}
		if err := r.write(server, line, payload); err != nil {
			return
		}
	}
}

// relayedJetStream is a connection through relay and the JetStream context a start is given,
// opened with the MaxWait the listener and Dispatch open theirs with, so a request the relay drops
// runs the caller's wait out exactly as it does in a program.
//
// The connection is a client's, as every production caller of EnsureKeyValue hands it one, so each
// row runs the path that call takes in a program: clientOwning finds the client that owns the
// connection, and the dropped request is sent again only because that client recorded no refusal
// of it. The denied row holds the other half of that path, where the record names a refusal and
// the send is not made again (TestADeniedBucketLookupIsNotSentAgain). A connection no client owns
// returns refusedBy at its early exit, so a row on one would never reach the record at all.
func relayedJetStream(t *testing.T, relay *unansweredRelay) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	client, err := Connect([]string{relay.url})
	if err != nil {
		t.Fatalf("connect the client whose connection the start opens the bucket on: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream through the relay: %v", err)
	}
	return client.Conn, js
}

// A start whose lookup of a bucket draws no answer sends it again and goes on, rather than ending
// the program with "open envoy_unanswered_lookup KV bucket: KV_envoy_unanswered_lookup
// STREAM.INFO: context deadline exceeded", which is what it does today. The first lookup never
// reaches the server, so the bucket the start is given is the one the second lookup answered, and
// the handle on it reads and writes keys.
func TestAnUnansweredBucketLookupIsSentAgainAndTheStartGoesOn(t *testing.T) {
	const bucket = "envoy_unanswered_lookup"
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	// The bucket is there before the call, so the call under test takes the lookup path and not
	// the create path.
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the bucket the start looks up: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	relay.arm(1)
	// Replicas is left at one copy, so no raise is attempted and every send the relay counts from
	// here is a read of the bucket's stream this call made.
	handle, err := EnsureKeyValue(countedReads(t.Context()), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("a start whose first bucket lookup drew no answer = %v, want it to go on", err)
	}

	if _, err := handle.Put("key", []byte("value")); err != nil {
		t.Fatalf("write a key through the handle the start was given: %v", err)
	}
	entry, err := handle.Get("key")
	if err != nil {
		t.Fatalf("read the key back through the handle the start was given: %v", err)
	}
	if got := string(entry.Value()); got != "value" {
		t.Fatalf("the key read back = %q, want %q", got, "value")
	}
	// Four reads ride this subject: the lookup whose send was lost, the lookup that answered, the
	// read of the found bucket's stream for its leader (a start at one copy can find a stream
	// another program has just raised), and the STREAM.INFO wrapKeyValue sends through its open
	// to build the bucket's jetstream handle, each the same request on the same subject.
	if sends := relay.sent(); sends != 4 {
		t.Fatalf("lookups sent = %d, want 4: the lost lookup, the one that answered, the stream read, and the handle open", sends)
	}
}

// A start whose create of a bucket draws no answer sends it again and goes on, rather than ending
// the program with "open envoy_unanswered_create KV bucket: KV_envoy_unanswered_create create:
// context deadline exceeded", which is what it does today. The first create never reaches the
// server, so the bucket is the second create's, and the handle on it is usable.
func TestAnUnansweredBucketCreateIsSentAgainAndTheStartGoesOn(t *testing.T) {
	const bucket = "envoy_unanswered_create"
	relay := startUnansweredRelayOn(t, testnats.URL(t), createSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	// The bucket is not there, so the call under test takes the create path. It is deleted however
	// the row ends, since a create whose send was lost leaves nothing behind but one that answered
	// does.
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	relay.arm(1)
	handle, err := EnsureKeyValue(countedReads(t.Context()), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("a start whose first bucket create drew no answer = %v, want it to go on", err)
	}

	if _, err := js.KeyValue(bucket); err != nil {
		t.Fatalf("the bucket the start created is not on the server: %v", err)
	}
	if _, err := handle.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
	// Two creates ride this subject: the one whose send was lost and the one that made the bucket.
	// The handle open rides the lookup's subject, which this relay passes on.
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("creates sent = %d, want 2: the lost one and the one that made the bucket", sends)
	}
}

// A start whose raise of a bucket draws no answer sends the raise again and goes on, rather than
// failing at the raise's own ten second wait with "raise KV_envoy_unanswered_raise:
// ENVOY_NATS_REPLICAS=3: context deadline exceeded", which is what it did. nats-server drops an
// update with no answer when its metadata proposal fails (updateStream); here the first raise
// never reaches the server, so the stream, read again, still has one copy, and the raise is sent
// again from that read and lands.
func TestAnUnansweredRaiseIsSentAgainAndTheStartGoesOn(t *testing.T) {
	const bucket = "envoy_unanswered_raise"
	cluster := testnats.StartCluster(t, 3)
	relay := startUnansweredRelayOn(t, cluster.Servers[0].URL(), updateSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket the start raises: %v", err)
	}

	relay.arm(1)
	if _, err := EnsureKeyValue(countedReads(t.Context()), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3}); err != nil {
		t.Fatalf("a start whose first raise drew no answer = %v, want it to go on", err)
	}

	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the raised stream: %v", err)
	}
	if info.Config.Replicas != 3 {
		t.Fatalf("the raised stream has %d copies, want 3", info.Config.Replicas)
	}
	// Two raises ride this subject: the one whose send was lost and the one that landed.
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("raises sent = %d, want 2: the lost one and the one that landed", sends)
	}
}

// A start whose raise the server never answers sends it again until the start's hang guard
// passes, and ends with an error that names the raise and the guard and no setting: a server that
// answered nothing refused none (refusedSettings). It once ended at the first raise's own ten
// second wait with "raise KV_envoy_unanswered_raise_never: ENVOY_NATS_REPLICAS=3: context
// deadline exceeded", which named a setting the server never saw. The guard is shortened here
// (ensureKeyValue) to two seconds past that wait, so it passes during the second raise. Every
// raise is dropped, so a single server serves: no raise reaches it.
func TestARaiseTheServerNeverAnswersEndsAtTheHangGuardNamingNoSetting(t *testing.T) {
	const bucket = "envoy_unanswered_raise_never"
	relay := startUnansweredRelayOn(t, testnats.URL(t), updateSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the one-copy bucket the start raises: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	relay.arm(1 << 30)
	guard := RequestTimeout + 2*time.Second
	_, err := ensureKeyValue(countedReads(context.Background()), guard, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, Replicas: 3})
	if !errors.Is(err, errHangGuard) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start whose raise the server never answers = %v, want the start's hang guard, with context.DeadlineExceeded", err)
	}
	if want := "raise KV_" + bucket + ": the update drew no answer when the wait ended"; !strings.Contains(err.Error(), want) {
		t.Fatalf("a start whose raise the server never answers = %v, want it to name %q", err, want)
	}
	if strings.Contains(err.Error(), ReplicasEnvVar) {
		t.Fatalf("a start whose raise the server never answers = %v, want no setting named: the server refused nothing", err)
	}
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("raises sent = %d, want 2: the raise sent again after its own wait, and the guard passing during the second", sends)
	}
}

// A setting of a bucket's age whose update draws no answer sends the update again and lands,
// rather than failing the session registry's start at the update's own ten second wait with "set
// KV_envoy_unanswered_age max age: context deadline exceeded", which is what it did.
func TestAnUnansweredMaxAgeUpdateIsSentAgain(t *testing.T) {
	const bucket = "envoy_unanswered_age"
	relay := startUnansweredRelayOn(t, testnats.URL(t), updateSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the bucket whose age is set: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	relay.arm(1)
	age, err := SetKeyValueMaxAge(countedReads(t.Context()), conn, js, bucket, time.Hour)
	if err != nil || age != time.Hour {
		t.Fatalf("a setting of the age whose first update drew no answer = %s, %v, want 1h and nil", age, err)
	}
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	if info.Config.MaxAge != time.Hour {
		t.Fatalf("the bucket's age = %s, want 1h", info.Config.MaxAge)
	}
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("updates sent = %d, want 2: the lost one and the one that landed", sends)
	}
}

// A start whose account info draws no answer sends the create step again and goes on, rather than
// ending the program with "open envoy_unanswered_info KV bucket: KV_envoy_unanswered_info create:
// context deadline exceeded" with no bucket made. That request is nats.go's own, sent inside
// js.CreateKeyValue before the STREAM.CREATE (nats.go@v1.55.0 kv.go:422-434), and a member that is
// not the metadata leader leaves it unanswered behind the same gate it leaves the create behind
// (readAgain). The first account info never reaches the server, so the bucket is the second
// send's, and the handle on it is usable.
func TestAnUnansweredAccountInfoIsSentAgainAndTheStartGoesOn(t *testing.T) {
	const bucket = "envoy_unanswered_info"
	relay := startUnansweredRelayOn(t, testnats.URL(t), accountInfoSubject())
	conn, js := relayedJetStream(t, relay)
	// The bucket is not there, so the call under test takes the create path, the only path that
	// asks for the account's info. It is deleted however the row ends, since a create whose
	// account info was lost leaves nothing behind but one that answered does.
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	relay.arm(1)
	handle, err := EnsureKeyValue(countedReads(t.Context()), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("a start whose first account info drew no answer = %v, want it to go on", err)
	}

	if _, err := js.KeyValue(bucket); err != nil {
		t.Fatalf("the bucket the start created is not on the server: %v", err)
	}
	if _, err := handle.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
	// Two account infos ride this subject: the one whose send was lost and the one of the send
	// that made the bucket. One rides each send of the create step and nothing else does: the
	// lookup is a STREAM.INFO, the handle open is another, and the STREAM.CREATE of the first
	// send was never reached, since js.CreateKeyValue returns the moment the account info draws
	// no answer. Each of those rides a subject of its own, which this relay passes on.
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("account infos sent = %d, want 2: the lost one and the one of the send that made the bucket", sends)
	}
}

// createStepReadBound is how long after it begins a start whose create step loses its own read of
// the stream may take: half the JetStream wait the read is sent under. A start that waited that read
// out took the whole wait, RequestTimeout, and one that sends the step again after its read limit
// (firstReadLimit) takes a fraction of a second, so the bound sits far from both on a loaded machine.
const createStepReadBound = RequestTimeout / 2

// A start whose create meets a bucket another start created a moment before, and whose create step
// loses the read of the stream nats.go makes inside it, sends the step again after its read limit
// and goes on, rather than waiting out the JetStream wait that read is sent under. On "stream name
// already in use" nats.go's js.CreateKeyValue reads the stream itself (nats.go@v1.55.0
// kv.go:534-545), and nothing the start passes reaches that read. In the moment after another
// start's create lands, no member answers it (the members that hold the new assignment are not the
// stream's preferred member, which has not applied it yet), and a read no member answered is never
// answered later: such a start once waited the whole ten seconds, in 103 of 1,600 runs of
// TestAStartAtOneCopyWhoseCreateMeetsAnotherStartsCreateAnswersOnceItHasALeader. Here the relay
// drops that read, the first STREAM.INFO of the bucket on the wire: the start's own lookup is
// answered "bucket not found" before anything is sent (missingOnceJetStream), as a lookup is that
// lands just before another start's create.
func TestACreateStepThatLosesItsReadOfAnotherStartsBucketGoesOnWithinItsReadLimit(t *testing.T) {
	const bucket = "envoy_unanswered_create_read"
	cluster := testnats.StartCluster(t, 3)
	relay := startUnansweredRelayOn(t, cluster.Servers[0].URL(), openSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	// The other start's bucket, at three copies, whose create this start's meets: its count is not
	// this start's, so the server answers this start's create "stream name already in use". The
	// fixture creates it (Cluster.CreateKeyValue), on this connection before the relay is armed:
	// the create of more than one copy is answered only at the bucket's first election, which a
	// loaded machine can hold past the connection's own ten-second wait.
	cluster.CreateKeyValue(t, conn, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})

	relay.arm(1)
	started := time.Now()
	kv, err := EnsureKeyValue(t.Context(), conn, &missingOnceJetStream{JetStreamContext: js}, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	took := time.Since(started)
	if err != nil {
		t.Fatalf("a start whose create step lost its read of the stream = %v, want it to go on", err)
	}
	// The dropped read, and at least the step's read sent again, the lookup that follows and the
	// handle open: a relay that dropped another request would leave fewer.
	if sends := relay.sent(); sends < 3 {
		t.Fatalf("reads of the bucket's stream sent = %d, want the lost one inside the create step and the ones after it", sends)
	}
	if took >= createStepReadBound {
		t.Fatalf("the start answered %s after it began, want within %s: a create step whose own read is lost is sent again after its read limit, %s, and not waited out for the JetStream wait, %s",
			took.Round(time.Millisecond), createStepReadBound, firstReadLimit, RequestTimeout)
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// unansweredKeyValue creates the bucket on a connection through relay and returns that connection
// with the legacy handle a start holds when it comes to open the jetstream handle of it. The
// connection is relayedJetStream's, a client's, so a row that drives the whole of EnsureKeyValue
// on it reads the refusal record that call reads (refusedBy, clientOwning) rather than the
// early exit a connection no client owns takes.
func unansweredKeyValue(t *testing.T, relay *unansweredRelay) (*nats.Conn, nats.JetStreamContext, nats.KeyValue) {
	t.Helper()
	conn, js := relayedJetStream(t, relay)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: unansweredBucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(unansweredBucket) })
	return conn, js, kv
}

// A start whose open of a bucket's jetstream handle draws no answer sends it again and goes on,
// rather than ending the program with "open the jetstream handle of KV bucket <name>: context
// deadline exceeded". The first open never reaches the server, so the handle the start is given
// is the second one's, and it reads the bucket's stream. This is the start that created the
// bucket.
func TestAnUnansweredHandleOpenIsSentAgainAndTheStartGoesOn(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(unansweredBucket))
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(1)
	handle, err := wrapKeyValue(countedReads(context.Background()), conn, kv, true, startSendsAgain)
	if err != nil {
		t.Fatalf("a start whose first handle open drew no answer = %v, want it to go on", err)
	}

	if sends := relay.sent(); sends != 2 {
		t.Fatalf("handle opens sent = %d, want 2: the lost one and the one that answered", sends)
	}
	if handle.Bucket() != unansweredBucket {
		t.Fatalf("the handle is on bucket %q, want %q", handle.Bucket(), unansweredBucket)
	}
	if _, err := handle.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
}

// A start that found its bucket rather than creating it sends the open again too. This is the
// ordinary start, a restart of a deployment whose buckets are already there, and it was the one
// the resend missed while the create and not the caller chose it: EnsureKeyValue leaves created
// false on that path, so one lost answer still ended the program.
func TestAnUnansweredHandleOpenOfAFoundBucketIsSentAgainAndTheStartGoesOn(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(unansweredBucket))
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(1)
	handle, err := wrapKeyValue(countedReads(context.Background()), conn, kv, false, startSendsAgain)
	if err != nil {
		t.Fatalf("a start that found its bucket and lost its first handle open = %v, want it to go on", err)
	}

	if sends := relay.sent(); sends != 2 {
		t.Fatalf("handle opens sent = %d, want 2: the lost one and the one that answered", sends)
	}
	if _, err := handle.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
}

// An answer the server gives ends the sending: a start sends the open once and is done, so
// nothing a server answers is ever asked twice.
func TestAnAnsweredHandleOpenIsSentOnce(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(unansweredBucket))
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(0)
	if _, err := wrapKeyValue(countedReads(context.Background()), conn, kv, true, startSendsAgain); err != nil {
		t.Fatalf("a start whose handle open the server answered = %v, want it to go on", err)
	}

	if sends := relay.sent(); sends != 1 {
		t.Fatalf("handle opens sent = %d, want 1: an answer is the server's and is not sent again", sends)
	}
}

// A caller that is no start sends the open once, whatever the server does with it.
// roles.Buckets.bucket reaches WrapKeyValue through OpenKeyValue for a read whose caller waits two
// seconds for it, so an open that kept sending would leave that read and every one after it
// unanswered instead of saving a start.
func TestAnUnansweredHandleOpenOffAStartIsSentOnce(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(unansweredBucket))
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(1)
	_, err := WrapKeyValue(conn, kv)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an open the server left unanswered off a start = %v, want %v", err, context.DeadlineExceeded)
	}
	if sends := relay.sent(); sends != 1 {
		t.Fatalf("handle opens sent = %d, want 1: this path is a request's, not a start's", sends)
	}
}

// A start that opens its bucket the way every start of this deployment does, through
// EnsureKeyValue, sends the handle open again when the server leaves it unanswered.
//
// The rows above hold wrapKeyValue with the choice already handed to it, and the choice is the
// half that was wrong: while created made it, a start that found its bucket sent the open once
// and one lost answer ended the program with "open the jetstream handle of KV bucket <name>:
// context deadline exceeded". Nothing held EnsureKeyValue's own choice - with it handing oneSend
// the whole of internal/bus passes and go vet is clean - so this row drives the entry point a
// start calls and reads the sends off the wire.
//
// How many requests a clean start sends on this subject is read rather than counted here: the
// lookup EnsureKeyValue makes is the same STREAM.INFO on the same subject as the open that
// follows it, so the drop is placed behind what a clean start sends, and a step added to the
// found-bucket path moves it along rather than landing it on another request.
func TestAStartThatOpensAFoundBucketThroughEnsureKeyValueSendsTheOpenAgain(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(unansweredBucket))
	conn, js, _ := unansweredKeyValue(t, relay)
	config := &nats.KeyValueConfig{Bucket: unansweredBucket, Storage: nats.MemoryStorage}

	relay.arm(0)
	if _, err := EnsureKeyValue(countedReads(t.Context()), conn, js, config); err != nil {
		t.Fatalf("a start whose every request the server answered = %v, want it to go on", err)
	}
	answered := relay.sent()
	if answered < 1 {
		t.Fatalf("a clean start sent %d requests on the bucket's stream, want at least the handle open itself", answered)
	}

	relay.passThenDrop(answered-1, 1)
	if _, err := EnsureKeyValue(countedReads(t.Context()), conn, js, config); err != nil {
		t.Fatalf("a start that found its bucket and lost its handle open = %v, want it to go on", err)
	}
	if sends := relay.sent(); sends != answered+1 {
		t.Fatalf("the start sent %d requests on the bucket's stream, want %d: a clean start's %d with the lost open sent again",
			sends, answered+1, answered)
	}
}

// A start that moves the role lanes off a stream that still carries them, and whose list of the
// stream's consumers draws no answer, lists them again and moves the consumer's filter. nats.go's
// legacy ConsumerNames reports no error, so the lost list was an empty one: the migration moved
// nothing and said nothing, and the start went on with the consumer still filtering the lanes. The
// relay drops the first list, on a single server, as a member whose meta leader changes leaves a
// request unanswered.
func TestALostListOfTheStreamsConsumersIsSentAgainAndTheMigrationMovesThem(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), "$JS.API.CONSUMER.NAMES."+Stream)
	_, js := relayedJetStream(t, relay)
	legacy := *streamCfg
	legacy.Subjects = []string{"notifications.>"}
	if _, err := js.AddStream(&legacy); err != nil {
		t.Fatalf("create the stream that carries the role lanes: %v", err)
	}
	const durable = "legacy-role-lane-durable"
	if _, err := js.AddConsumer(Stream, &nats.ConsumerConfig{Durable: durable, FilterSubject: "notifications.>", AckPolicy: nats.AckExplicitPolicy}); err != nil {
		t.Fatalf("create the durable that filters the role lanes: %v", err)
	}

	relay.arm(1)
	cfg := *streamCfg
	if _, err := ensureStreamWithConfig(t.Context(), js, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose list of the stream's consumers drew no answer = %v, want it to move the role lanes off", err)
	}
	info, err := js.ConsumerInfo(Stream, durable)
	if err != nil {
		t.Fatalf("read the durable after the start: %v", err)
	}
	if info.Config.FilterSubject != "" || !slices.Equal(info.Config.FilterSubjects, consumerFilterSubjects(streamCfg.Subjects)) {
		t.Fatalf("the durable filters %q and %v after the start, want the stream's own subjects in place of notifications.>: the migration moved nothing",
			info.Config.FilterSubject, info.Config.FilterSubjects)
	}
	if sends := relay.sent(); sends < 2 {
		t.Fatalf("lists of the stream's consumers sent = %d, want the lost one and the one sent again", sends)
	}
}

// shortListJetStream answers the first list of the stream's consumers with none and no error, as
// nats.go's legacy ConsumerNames ends a list whose page the server answered with an error (a
// member that sees its metadata leader gone answers JSClusterNotAvailError at once, rather than
// leave the request unanswered). Every later list is js's own.
type shortListJetStream struct {
	nats.JetStreamContext
	lists atomic.Int32
}

func (js *shortListJetStream) ConsumerNames(stream string, opts ...nats.JSOpt) <-chan string {
	if js.lists.Add(1) == 1 {
		none := make(chan string)
		close(none)
		return none
	}
	return js.JetStreamContext.ConsumerNames(stream, opts...)
}

// A list of the stream's consumers that ends short at once, with no error, is read again and the
// migration moves the consumer: the list is read against the stream's own count of its consumers
// (consumerNames). With no such count the list was taken as it came, the migration moved nothing,
// and the start went on with the consumer still filtering the role lanes.
func TestAListOfTheStreamsConsumersThatEndsShortIsReadAgainAndTheMigrationMovesThem(t *testing.T) {
	js := streamTestJetStream(t)
	legacyRoleLaneStream(t, js)
	const durable = "legacy-role-lane-short-list"
	if _, err := js.AddConsumer(Stream, &nats.ConsumerConfig{Durable: durable, FilterSubject: "notifications.>", AckPolicy: nats.AckExplicitPolicy}); err != nil {
		t.Fatalf("create the durable that filters the role lanes: %v", err)
	}

	short := &shortListJetStream{JetStreamContext: js}
	cfg := *streamCfg
	if _, err := ensureStreamWithConfig(t.Context(), short, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose first list of the stream's consumers ended short = %v, want it to move the role lanes off", err)
	}
	info, err := js.ConsumerInfo(Stream, durable)
	if err != nil {
		t.Fatalf("read the durable after the start: %v", err)
	}
	if info.Config.FilterSubject != "" || !slices.Equal(info.Config.FilterSubjects, consumerFilterSubjects(streamCfg.Subjects)) {
		t.Fatalf("the durable filters %q and %v after the start, want the stream's own subjects in place of notifications.>: the migration moved nothing",
			info.Config.FilterSubject, info.Config.FilterSubjects)
	}
	if lists := short.lists.Load(); lists < 2 {
		t.Fatalf("lists of the stream's consumers = %d, want the short one and the one read again", lists)
	}
}

// heldRequest is how long the held rows below hold one request before it reaches the server: past
// RequestTimeout, the JetStream wait of the context a program opens (relayedJetStream), which a
// start once waited out for a write whose answer had not come. heldRequestBound is how long such a
// start may take: a start that waited that wait out took all of it, and one that sends the write
// again after its read limit (firstReadLimit) takes a fraction of a second, so the bound sits far
// from both on a loaded machine.
const (
	heldRequest      = RequestTimeout + time.Second
	heldRequestBound = RequestTimeout / 2
)

// A start whose raise of a bucket reaches the server only after the JetStream wait, as a raise that
// waits in the server's JetStream API queue does, reads the stream after its read limit, finds the
// raise not landed, sends it again from that read and goes on. It once waited the raise's whole
// JetStream wait, ten seconds, before it read the stream.
// The relay holds the first raise heldRequest; the start's own context, a hang guard of 36 s,
// ends no send here. A start whose raise is never answered ends at its context instead
// (TestARaiseTheServerNeverAnswersEndsAtTheHangGuardNamingNoSetting).
//
// The row times the send again, from the held raise's send to the raise's second send, and not
// the whole start: after the raise the start waits for the raised stream's first election
// (awaitReplicasCurrent), which takes seconds on a slow or loaded cluster, and which no bound of
// this row may decide.
func TestARaiseHeldPastTheJetStreamWaitIsSentAgainAfterItsReadLimit(t *testing.T) {
	const bucket = "envoy_held_raise"
	cluster := testnats.StartCluster(t, 3)
	relay := startUnansweredRelayOn(t, cluster.Servers[0].URL(), updateSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket the start raises: %v", err)
	}

	relay.holdNext(1, heldRequest)
	timed := &updateWaitJetStream{JetStreamContext: js, stream: "KV_" + bucket}
	started := time.Now()
	_, err := EnsureKeyValue(t.Context(), conn, timed, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	took := time.Since(started)
	if err != nil {
		t.Fatalf("a start whose first raise was held %s = %v after %s, want it to go on", heldRequest, err, took.Round(time.Millisecond))
	}
	sent, resent := timed.sent.Load(), timed.resent.Load()
	if sent == 0 || resent == 0 {
		t.Fatalf("raises sent through the start's JetStream context = %d, want the held one and the one sent again", timed.sends.Load())
	}
	if again := time.Duration(resent - sent); again >= heldRequestBound {
		t.Fatalf("the raise was sent again %s after the held one went out, want within %s: a raise whose answer has not come is read back after its read limit, %s, and not waited for up to the JetStream wait, %s",
			again.Round(time.Millisecond), heldRequestBound, firstReadLimit, RequestTimeout)
	}
	if info, err := js.StreamInfo("KV_" + bucket); err != nil || info.Config.Replicas != 3 {
		t.Fatalf("the raised stream = %+v (%v), want 3 copies", info, err)
	}
	if sends := relay.sent(); sends < 2 {
		t.Fatalf("raises sent = %d, want the held one and the one sent again", sends)
	}
}

// A start whose create of ENVOY_NOTIFICATIONS reaches the server only after the JetStream wait, as
// a create that waits in the server's JetStream API queue does, sends the create again after its
// read limit and goes on: the create sent again is the first to reach the server, which makes the
// stream and answers it, and the held one lands later as an equal create, carrying the same id
// (ensureStreamWithConfig). Sent under the caller's whole wait, the start waited the held create's
// JetStream wait, ten seconds, before it sent it again. The bucket's create step has a row of its
// own (TestACreateStepThatLosesItsReadOfAnotherStartsBucketGoesOnWithinItsReadLimit).
func TestANotificationStreamCreateHeldPastTheJetStreamWaitIsSentAgainAfterItsReadLimit(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), jsAPI("STREAM.CREATE", Stream))
	conn, js := relayedJetStream(t, relay)

	relay.holdNext(1, heldRequest)
	started := time.Now()
	cfg := *streamCfg
	_, err := ensureStreamWithConfig(t.Context(), readsRefusals(conn, js), &cfg, capsAnUnsetStream, atStart)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("a start whose first create of %s was held %s = %v after %s, want it to go on", Stream, heldRequest, err, took.Round(time.Millisecond))
	}
	if took >= heldRequestBound {
		t.Fatalf("the start answered %s after it began, want within %s: a create whose answer has not come is sent again after its read limit, %s, and not waited for up to the JetStream wait, %s",
			took.Round(time.Millisecond), heldRequestBound, firstReadLimit, RequestTimeout)
	}
	if _, err := js.StreamInfo(Stream); err != nil {
		t.Fatalf("read the stream the start created: %v", err)
	}
	if sends := relay.sent(); sends < 2 {
		t.Fatalf("creates of %s sent = %d, want the held one and the one sent again", Stream, sends)
	}
}

// legacyRoleLaneStream creates, through js, the stream an Envoy release from before 2026-09-25
// left deployed: its subjects capture the role lanes, and it holds one message on a role lane, so
// a start that moves the lanes off it has a purge to make and a lane a read finds unpurged.
func legacyRoleLaneStream(t *testing.T, js nats.JetStreamContext) {
	t.Helper()
	legacy := *streamCfg
	legacy.Subjects = []string{"notifications.>"}
	if _, err := js.AddStream(&legacy); err != nil {
		t.Fatalf("create the stream that carries the role lanes: %v", err)
	}
	if _, err := js.Publish(contracts.RoleTopicPrefix+"reviewer", []byte("kept on the role lane")); err != nil {
		t.Fatalf("store a message on a role lane: %v", err)
	}
}

// A start that moves a consumer's filter off the role lanes, whose move reaches the server only
// after the JetStream wait, reads the consumer after its read limit, finds it still on the legacy
// filter, moves it again from that read and goes on. It once sent the move once and waited out its
// JetStream wait, ten seconds, and the start failed "nats: timeout". The js the start is handed is
// a client's, as in a program (readsRefusals).
func TestAMoveOfAConsumerOffTheRoleLanesHeldPastTheJetStreamWaitIsSentAgain(t *testing.T) {
	const durable = "legacy-role-lane-held"
	relay := startUnansweredRelayOn(t, testnats.URL(t), "$JS.API.CONSUMER.CREATE."+Stream+"."+durable)
	conn, js := relayedJetStream(t, relay)
	legacyRoleLaneStream(t, js)
	if _, err := js.AddConsumer(Stream, &nats.ConsumerConfig{Durable: durable, FilterSubject: "notifications.>", AckPolicy: nats.AckExplicitPolicy}); err != nil {
		t.Fatalf("create the durable that filters the role lanes: %v", err)
	}

	relay.holdNext(1, heldRequest)
	started := time.Now()
	cfg := *streamCfg
	_, err := ensureStreamWithConfig(t.Context(), readsRefusals(conn, js), &cfg, capsAnUnsetStream, atStart)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("a start whose first move of the durable was held %s = %v after %s, want it to go on", heldRequest, err, took.Round(time.Millisecond))
	}
	if took >= heldRequestBound {
		t.Fatalf("the start answered %s after it began, want within %s: a move whose answer has not come is read back after its read limit, %s, and not waited for up to the JetStream wait, %s",
			took.Round(time.Millisecond), heldRequestBound, firstReadLimit, RequestTimeout)
	}
	info, err := js.ConsumerInfo(Stream, durable)
	if err != nil {
		t.Fatalf("read the durable after the start: %v", err)
	}
	if info.Config.FilterSubject != "" || !slices.Equal(info.Config.FilterSubjects, consumerFilterSubjects(streamCfg.Subjects)) {
		t.Fatalf("the durable filters %q and %v after the start, want the stream's own subjects in place of notifications.>",
			info.Config.FilterSubject, info.Config.FilterSubjects)
	}
	if sends := relay.sent(); sends < 2 {
		t.Fatalf("moves of the durable sent = %d, want the held one and the one sent again", sends)
	}
}

// A start's purge of a role lane is waited for up to its read limit, so the start waits no
// JetStream wait: nats.go's PurgeStream ignores any context it is handed and waits the context's
// own ten seconds (nats.go@v1.55.0 jsm.go:1529-1576), and a start once sent the purge once that way
// and failed "nats: timeout" when its answer had not come by then. With the first purge held past
// that wait, the start reads the lane after its read limit, finds its message still there, purges
// it again and goes on. With every purge held, the start ends when its context ends, naming the
// purge, and not when a held purge lands.
func TestARoleLanePurgeHeldPastTheJetStreamWaitEndsAtAnAnswerOrTheContext(t *testing.T) {
	t.Run("the first purge held", func(t *testing.T) {
		relay := startUnansweredRelayOn(t, testnats.URL(t), jsAPI("STREAM.PURGE", Stream))
		conn, js := relayedJetStream(t, relay)
		legacyRoleLaneStream(t, js)

		relay.holdNext(1, heldRequest)
		started := time.Now()
		cfg := *streamCfg
		_, err := ensureStreamWithConfig(t.Context(), readsRefusals(conn, js), &cfg, capsAnUnsetStream, atStart)
		took := time.Since(started)
		if err != nil {
			t.Fatalf("a start whose first purge was held %s = %v after %s, want it to go on", heldRequest, err, took.Round(time.Millisecond))
		}
		if took >= heldRequestBound {
			t.Fatalf("the start answered %s after it began, want within %s: a purge whose answer has not come is read back after its read limit, %s, and not waited for up to the JetStream wait, %s",
				took.Round(time.Millisecond), heldRequestBound, firstReadLimit, RequestTimeout)
		}
		if left, err := laneMessages(js, roleLaneSubjects[0]); err != nil || left != 0 {
			t.Fatalf("messages left on %s = %d (%v), want none", roleLaneSubjects[0], left, err)
		}
		// Five purges: the held one and the one sent again after the read found the lane's message
		// still there, the other lane's, and one of each lane after the stream's update
		// (ensureStreamWithConfig purges the lanes again once the update has landed). The purges
		// after the update empty the lane whatever the first two did, so only this count tells a
		// start that read the lane back and purged it again from one that took the held purge as
		// landed, which sends four.
		if sends := relay.sent(); sends < 5 {
			t.Fatalf("purges sent = %d, want 5: the held one, the one sent again, the other lane's and one of each lane after the update", sends)
		}
	})
	t.Run("every purge held", func(t *testing.T) {
		relay := startUnansweredRelayOn(t, testnats.URL(t), jsAPI("STREAM.PURGE", Stream))
		conn, js := relayedJetStream(t, relay)
		legacyRoleLaneStream(t, js)

		relay.holdNext(1<<30, heldRequest)
		const wait = 3 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), wait)
		defer cancel()
		started := time.Now()
		cfg := *streamCfg
		_, err := ensureStreamWithConfig(ctx, readsRefusals(conn, js), &cfg, capsAnUnsetStream, atStart)
		took := time.Since(started)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the purge of "+roleLaneSubjects[0]) {
			t.Fatalf("a start whose every purge is held = %v, want it to end with its context, naming the purge of %s", err, roleLaneSubjects[0])
		}
		if took >= heldRequest {
			t.Fatalf("the start ended %s after it began, want at its context's %s, before a held purge lands", took.Round(time.Millisecond), wait)
		}
	})
}
