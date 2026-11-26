package bus

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// smallMaxPayload is the max payload of the server this file starts: far under the 1 MiB default,
// so every message its rows measure is a few kilobytes whatever the machine's load. It is
// kvchunktest.MaxPayload's value, written out because that package imports this one.
const smallMaxPayload = 64 << 10

// handshakeHoldingProxy forwards the first connection it accepts to upstream, and holds every later
// one open and silent: a server that takes a reconnect's dial and never answers its handshake, so
// nats.go's reconnect attempt holds its connection lock across that handshake (nats.go v1.55.0
// doReconnect, :3554-3581) until the handshake's own timeout. held receives each held connection.
type handshakeHoldingProxy struct {
	ln       net.Listener
	upstream string
	held     chan net.Conn

	mu    sync.Mutex
	first []net.Conn
	open  []net.Conn
}

func newHandshakeHoldingProxy(t *testing.T, upstream string) *handshakeHoldingProxy {
	t.Helper()
	ln, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &handshakeHoldingProxy{ln: ln, upstream: upstream, held: make(chan net.Conn, 16)}
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, conn := range p.open {
			_ = conn.Close()
		}
	})
	go p.accept()
	return p
}

func (p *handshakeHoldingProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.open = append(p.open, client)
		forward := p.first == nil
		p.mu.Unlock()
		if !forward {
			select {
			case p.held <- client:
			default:
			}
			continue
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.first = []net.Conn{client, server}
		p.open = append(p.open, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

// cutFirst closes both sides of the forwarded connection, so the client reconnects.
func (p *handshakeHoldingProxy) cutFirst() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.first {
		_ = conn.Close()
	}
}

// stillHeld reports whether the held connection the proxy took is still open and unanswered, so
// the reconnect attempt that dialled it still holds nats.go's connection lock.
func (p *handshakeHoldingProxy) stillHeld(held net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.open, held) && p.first != nil
}

// The size checks read the max payload the client keeps (Client.maxPayload) and no state of
// nats.go's, so they answer while a reconnect attempt holds nats.go's connection lock across its
// handshake, which nats.Conn.MaxPayload waits for. The verdict is an order, not a duration: the
// checks answer while the held handshake is still unanswered and the control read of
// nats.Conn.MaxPayload, started before them, has still not returned. Nothing here is timed, and
// the server's max payload is the small one above, so each message the checks measure is a few
// kilobytes whatever the machine's load.
//
// The client's connection is cut, and its reconnect reaches a server that takes the dial and never
// answers the handshake, which nats.go waits out holding the lock (up to connectTimeout, 5 s).
// The only bound here is the test binary's own deadline, through testwait.
func TestTheSizeChecksAnswerWhileAReconnectHoldsTheConnectionLock(t *testing.T) {
	t.Parallel()
	_, upstream := testnats.StartConfig(t, "max_payload: "+strconv.Itoa(smallMaxPayload)+"\njetstream {}\n", []string{"-js"}, true)
	proxy := newHandshakeHoldingProxy(t, strings.TrimPrefix(upstream, "nats://"))
	client, err := Connect([]string{"nats://" + proxy.ln.Addr().String()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := client.maxPayload.Load()
	if maxPayload != smallMaxPayload {
		t.Fatalf("UNKNOWN: the connected client keeps a max payload of %d, want the server's %d", maxPayload, smallMaxPayload)
	}
	const topic = contracts.RoleTopicPrefix + "held-handshake"
	// A few kilobytes past the small bound: the encode the checks make is the same work whatever
	// the machine's load.
	big := sizeEnvelope(topic, strings.Repeat("p", int(maxPayload)+1))

	proxy.cutFirst()
	held := testwait.Receive(t, "the client's reconnect to reach the server that holds its handshake", proxy.held)

	// The control: a read of the same value through nats.go, which takes the lock the reconnect
	// holds. It must not have answered by the time the checks below have.
	locked := make(chan int64, 1)
	go func() { locked <- client.Connection().MaxPayload() }()

	if err := client.TooLargeToPublish(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("TooLargeToPublish while the reconnect holds the lock = %v, want ErrTooLarge", err)
	}
	if err := client.PublishCoreContext(context.Background(), topic, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the core publish while the reconnect holds the lock = %v, want ErrTooLarge", err)
	}

	// Both answers came while the handshake was still held and the control had not returned, so
	// the lock was held throughout and neither check waited for it.
	select {
	case read := <-locked:
		t.Fatalf("UNKNOWN: nats.Conn.MaxPayload answered %d before the checks did, so the reconnect was not holding the lock while they ran", read)
	default:
	}
	if !proxy.stillHeld(held) {
		t.Fatal("UNKNOWN: the held handshake ended before the checks answered, so the reconnect was not holding the lock while they ran")
	}
}
