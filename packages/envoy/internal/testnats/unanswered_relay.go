package testnats

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// UnansweredRelay is a TCP relay between a client and a real NATS server that counts the requests
// the client sends on one subject and drops the first drops of them: the request never reaches the
// server, so no answer is ever published and the caller's wait runs out with the deadline it held,
// as a JetStream member leaves a request unanswered while its leader changes. Everything else
// both sides send is relayed untouched, so the connection is a real one on a real server and every
// other request is answered as it always is. The sends a test's setup makes are counted apart from
// the sends of the call it is about (Arm).
//
// Drop cuts every link the relay holds, as a pod's TCP connection dies under it: the client reads
// EOF and reconnects through the relay, which accepts the new link as it accepted the first. A
// request dropped and then cut is the order of one whose bytes reached the socket and died with it,
// which nats.go fails at no cut.
type UnansweredRelay struct {
	subject  string
	url      string
	listener net.Listener
	mu       sync.Mutex
	pass     int
	drops    int
	sends    int
	// links holds both ends of every link the relay has joined, under mu, for Drop.
	links []net.Conn
	// pings counts the PINGs the client sent and the relay passed on, pongs the PONGs the server
	// answered and the relay handed the client, both under mu; pingsAtDrop is pings when the last
	// drop was made. The server answers PINGs in order, so on a link where every PING passed on got
	// its PONG, the PONG that takes pongs past pingsAtDrop answers the first PING sent after the
	// dropped request: the flush behind it (FlushedSinceDrop). Both counts run across every link
	// and nothing resets them, so a PING a Drop cut off before its PONG came back leaves pongs one
	// behind on every later link: after a later drop, the PONG that takes pongs past pingsAtDrop
	// answers a later PING. The answer comes late then, never early.
	pings, pongs, pingsAtDrop int
}

// StartUnansweredRelayOn relays to the NATS server at target, a client URL such as URL returns, on
// a loopback port and address of its own (testport.LoopbackIP) until the test ends, counting and
// dropping the client's sends on subject. Its own URL is that one with the host and port it
// listens on, credentials included, as StartProxy builds its own. It drops nothing until it is
// armed.
func StartUnansweredRelayOn(t testing.TB, target, subject string) *UnansweredRelay {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the relay relays to: %v", target, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &UnansweredRelay{
		subject:  subject,
		url:      relayed.String(),
		listener: listener,
	}
	t.Cleanup(func() { _ = listener.Close() })
	go relay.serve(server.Host)
	return relay
}

// URL is the relay's own URL, the one a client dials to go through it.
func (r *UnansweredRelay) URL() string { return r.url }

// Subject is the subject whose sends the relay counts and drops.
func (r *UnansweredRelay) Subject() string { return r.subject }

// Arm has the relay drop the next drops sends of its subject and counts the sends from now, so a
// test counts the sends of the call it is about to make and not the ones that set it up.
func (r *UnansweredRelay) Arm(drops int) {
	r.PassThenDrop(0, drops)
}

// PassThenDrop lets the next pass sends of the subject through and drops the drops after them,
// counting the sends from now: for a call whose earlier requests share the subject with the one a
// test means to drop.
func (r *UnansweredRelay) PassThenDrop(pass, drops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pass = pass
	r.drops = drops
	r.sends = 0
}

// Sent is how many sends of its subject the client has made since the relay was armed.
func (r *UnansweredRelay) Sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

// send records one send of subject and reports whether it is dropped. A send of another subject
// is not recorded and never dropped. A send of subject is passed while passes are left, spending
// one, and otherwise dropped while drops are left, spending one.
func (r *UnansweredRelay) send(subject string) bool {
	if subject != r.subject {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	if r.pass > 0 {
		r.pass--
		return false
	}
	if r.drops > 0 {
		r.drops--
		r.pingsAtDrop = r.pings
		return true
	}
	return false
}

// FlushedSinceDrop reports whether pongs has passed pingsAtDrop: whether the server has answered,
// and the relay handed the client, the first PING the client sent after the request the relay
// last dropped, on a link where every PING passed on got its PONG. A flush behind that request
// then has its answer in the client's socket, ahead of anything the relay does after. After a
// Drop that cut a PING off before its PONG, it answers true only at a later PING, never sooner;
// before any drop, pingsAtDrop is 0 and it answers true at the first PONG.
func (r *UnansweredRelay) FlushedSinceDrop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pongs > r.pingsAtDrop
}

// Drop closes both ends of every link the relay has joined so far: a real cut of the client's
// connection, which nats.go reads as EOF, rather than its own ForceReconnect, which also fails
// every request its response subscription is waiting on (a KV read among them) and so would hide
// the request a cut leaves waiting.
func (r *UnansweredRelay) Drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, link := range r.links {
		_ = link.Close()
	}
	r.links = nil
}

// serve joins each connection the relay accepts to the server until the listener closes.
func (r *UnansweredRelay) serve(target string) {
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
		r.mu.Lock()
		r.links = append(r.links, client, server)
		r.mu.Unlock()
		go r.filter(server, client)
		go r.toClient(client, server)
	}
}

// filter relays what the client sends to the server, a protocol message at a time, leaving out the
// publishes the relay drops. A client sends only CONNECT, SUB, UNSUB, PING, PONG and publishes, so
// every line that is not a publish is passed on as it is, and a publish is passed on with the
// payload its header counts.
func (r *UnansweredRelay) filter(server, client net.Conn) {
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
			if strings.EqualFold(strings.TrimSpace(line), "PING") {
				r.mu.Lock()
				r.pings++
				r.mu.Unlock()
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

// toClient relays what the server sends to the client, a protocol message at a time, counting
// each PONG once it is in the client's socket (FlushedSinceDrop). A message the server delivers,
// `MSG <subject> <sid> [reply] <bytes>` or `HMSG <subject> <sid> [reply] <header bytes> <bytes>`,
// is passed on with the payload its header counts, so no payload is read as a PONG; every other
// line is passed on as it is.
func (r *UnansweredRelay) toClient(client, server net.Conn) {
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	reader := bufio.NewReader(server)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		var payload []byte
		if len(fields) >= 4 && (strings.EqualFold(fields[0], "MSG") || strings.EqualFold(fields[0], "HMSG")) {
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			payload = make([]byte, size+2)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
		}
		if _, err := io.WriteString(client, line); err != nil {
			return
		}
		if payload != nil {
			if _, err := client.Write(payload); err != nil {
				return
			}
		}
		if strings.EqualFold(strings.TrimSpace(line), "PONG") {
			r.mu.Lock()
			r.pongs++
			r.mu.Unlock()
		}
	}
}
