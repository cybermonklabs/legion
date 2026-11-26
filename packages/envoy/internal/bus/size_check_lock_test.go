package bus

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// handshakeHoldingProxy forwards the first connection it accepts to upstream, and holds every later
// one open and silent: a server that takes a reconnect's dial and never answers its handshake, so
// nats.go's reconnect attempt holds its connection lock across that handshake (nats.go v1.55.0
// doReconnect, :3554-3581) until the handshake's timeout. held receives each held connection.
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

// The size checks read the max payload the client keeps (Client.maxPayload) and no state of
// nats.go's, so they answer while a reconnect attempt holds nats.go's connection lock across its
// handshake, which nats.Conn.MaxPayload waits for. The client's connection is cut and its reconnect
// reaches a server that takes the dial and never answers the handshake, which nats.go waits for up
// to connectTimeout (5 s) holding the lock. TooLargeToPublish, and a publish of the same message,
// must answer ErrTooLarge well inside that. The control is nats.Conn.MaxPayload itself in the same
// moment: it does not answer within the same bound, so the lock was held while the checks answered.
func TestTheSizeChecksAnswerWhileAReconnectHoldsTheConnectionLock(t *testing.T) {
	t.Parallel()
	_, upstream := testnats.Start(t)
	proxy := newHandshakeHoldingProxy(t, strings.TrimPrefix(upstream, "nats://"))
	client, err := Connect([]string{"nats://" + proxy.ln.Addr().String()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := client.knownMaxPayload()
	if maxPayload <= 0 {
		t.Fatalf("UNKNOWN: the connected client keeps a max payload of %d, so its checks refuse nothing", maxPayload)
	}
	const topic = contracts.RoleTopicPrefix + "held-handshake"
	big := sizeEnvelope(topic, strings.Repeat("p", int(maxPayload)+1))

	proxy.cutFirst()
	testwait.Receive(t, "the client's reconnect to reach the server that holds its handshake", proxy.held)

	// The bound the checks must answer within, well inside the handshake's 5 s.
	const answerWithin = time.Second
	locked := make(chan int64, 1)
	go func() { locked <- client.Connection().MaxPayload() }()

	started := time.Now()
	if err := client.TooLargeToPublish(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("TooLargeToPublish while the reconnect holds the lock = %v, want ErrTooLarge", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.PublishCoreContext(ctx, topic, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the core publish while the reconnect holds the lock = %v, want ErrTooLarge", err)
	}
	if elapsed := time.Since(started); elapsed >= answerWithin {
		t.Fatalf("the size checks answered after %s while the reconnect held the lock, want inside %s", elapsed, answerWithin)
	}
	select {
	case read := <-locked:
		t.Fatalf("control: nats.Conn.MaxPayload answered %d inside %s, so the reconnect held no lock and the row shows nothing", read, answerWithin)
	case <-time.After(answerWithin):
	}
}
