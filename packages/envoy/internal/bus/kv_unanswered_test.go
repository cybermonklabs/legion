package bus

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// openSubject is the request a lookup of a bucket sends: the STREAM.INFO of the bucket's stream,
// which nats.go builds from the bucket's name for the legacy js.KeyValue (nats.go@v1.53.1
// kv.go:396-411, js.go:189-192) and for the jetstream handle wrapKeyValue opens on it.
func openSubject(bucket string) string { return "$JS.API.STREAM.INFO.KV_" + bucket }

// createSubject is the request a create of a bucket sends: the STREAM.CREATE of the bucket's
// stream, which nats.go builds from the bucket's name for js.CreateKeyValue (nats.go@v1.53.1
// kv.go:524-534, js.go:189-192).
func createSubject(bucket string) string { return "$JS.API.STREAM.CREATE.KV_" + bucket }

// accountInfoSubject is the request js.CreateKeyValue sends before its STREAM.CREATE: the
// account's JetStream info, which nats.go asks for first (nats.go@v1.53.1 kv.go:432-434,
// jsm.go:409). It names no bucket, so a row that drops it drops the account info of whatever
// this connection asks the server for next, and only a row that makes one call may use it.
func accountInfoSubject() string { return "$JS.API.INFO" }

// unansweredRelay is a TCP relay between a client and a real NATS server that counts the requests
// the client sends on one subject and drops the first drops of them: the request never reaches the
// server, as a member of a metadata group younger than lostQuorumInterval, or one that has not
// yet seen its leader go, leaves one unanswered (sentAgain), so no answer is ever published and
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
// it is about (arm).
type unansweredRelay struct {
	subject  string
	url      string
	listener net.Listener
	mu       sync.Mutex
	drops    int
	sends    int
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
	r.drops = drops
	r.sends = 0
}

// sent is how many sends of its subject the client has made since the relay was armed.
func (r *unansweredRelay) sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

// send records one send of subject and reports whether it is dropped: it is while drops are left,
// and each such send spends one.
func (r *unansweredRelay) send(subject string) bool {
	if subject != r.subject {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	if r.drops > 0 {
		r.drops--
		return true
	}
	return false
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
// publishes the relay drops. A client sends only CONNECT, SUB, UNSUB, PING, PONG and publishes, so
// every line that is not a publish is passed on as it is, and a publish is passed on with the
// payload its header counts.
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
			if _, err := io.WriteString(server, line); err != nil {
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
		if r.send(fields[1]) {
			continue
		}
		if _, err := io.WriteString(server, line); err != nil {
			return
		}
		if _, err := server.Write(payload); err != nil {
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
// returns refusedSend at its early exit, so a row on one would never reach the record at all.
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
	// here is a lookup this call made.
	handle, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
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
	// Three lookups ride this subject: the one whose send was lost, the one that answered, and the
	// STREAM.INFO wrapKeyValue sends through its open to build the bucket's jetstream handle,
	// which is the same request on the same subject.
	if sends := relay.sent(); sends != 3 {
		t.Fatalf("lookups sent = %d, want 3: the lost one, the one that answered, and the handle open", sends)
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
	handle, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("a start whose first bucket create drew no answer = %v, want it to go on", err)
	}

	if _, err := js.KeyValue(bucket); err != nil {
		t.Fatalf("the bucket the start created is not on the server: %v", err)
	}
	if _, err := handle.ReadStreamState(); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
	// Two creates ride this subject: the one whose send was lost and the one that made the bucket.
	// The handle open rides the lookup's subject, which this relay passes on.
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("creates sent = %d, want 2: the lost one and the one that made the bucket", sends)
	}
}

// A start whose account info draws no answer sends the create step again and goes on, rather than
// ending the program with "open envoy_unanswered_info KV bucket: KV_envoy_unanswered_info create:
// context deadline exceeded" with no bucket made. That request is nats.go's own, sent inside
// js.CreateKeyValue before the STREAM.CREATE (nats.go@v1.53.1 kv.go:432-434), and a member that is
// not the metadata leader leaves it unanswered behind the same gate it leaves the create behind
// (sentAgain). The first account info never reaches the server, so the bucket is the second
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
	handle, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("a start whose first account info drew no answer = %v, want it to go on", err)
	}

	if _, err := js.KeyValue(bucket); err != nil {
		t.Fatalf("the bucket the start created is not on the server: %v", err)
	}
	if _, err := handle.ReadStreamState(); err != nil {
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
