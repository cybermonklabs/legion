package bus

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// proxiedLookup is a connect's environment that names the proxy at address.
func proxiedLookup(address string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == SOCKS5ProxyEnvVar {
			return address, true
		}
		return "", false
	}
}

// A proxied dial ends when the connect's deadline does, as a direct dial does (nats.go applies a
// connect's shortened timeout only to its own dialer). A proxy that accepts the connection and
// never answers the SOCKS5 greeting holds the dial open: the dial arms its bound at the connect's
// one-second deadline, not at the bus's five-second connect timeout, and lets go of the proxy's
// connection once that deadline passes.
//
// The connect's deadline is a bound armed on a clocktest.Fake (armBound, as a publish's replacement
// dial has it), and the dialer arms each dial's bound on the same fake, so the row reads the moment
// each bound is armed for rather than timing either, and moves the fake to the deadline itself.
func TestAProxiedDialEndsAtTheConnectsDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the silent proxy: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	greeted := make(chan struct{}, 16)
	closed := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// Read until the client closes its end, and say when the dial's greeting has
				// arrived: the proxy answers nothing.
				buf := make([]byte, 1<<10)
				for first := true; ; first = false {
					if _, err := conn.Read(buf); err != nil {
						closed <- struct{}{}
						return
					}
					if first {
						greeted <- struct{}{}
					}
				}
			}()
		}
	}()
	dialer, err := socks5Proxy(proxiedLookup(listener.Addr().String()))
	if err != nil {
		t.Fatalf("read the proxy setting: %v", err)
	}
	fake := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	dialer.clock = fake

	deadline := fake.Now().Add(time.Second)
	ctx, cancel := (&Client{clock: fake}).armBound(context.Background(), time.Second)
	defer cancel()
	connected := make(chan error, 1)
	go func() {
		conn, err := connectWithContext(ctx, "deadline-test", []string{"nats://nats.example.test:4222"}, nil, dialer, nil, nil, nil, nil, nil)
		if conn != nil {
			conn.Close()
		}
		connected <- err
	}()
	// The dial arms its bound before it reaches the proxy, so once the proxy has its greeting the
	// fake holds both bounds: the connect's and the dial's.
	testwait.Receive(t, "the proxied dial's greeting to reach the proxy", greeted)
	if armed := fake.Armed(); len(armed) != 2 || !armed[0].Equal(deadline) || !armed[1].Equal(deadline) {
		t.Fatalf("the connect and its proxied dial armed bounds at %v, want both at the connect's deadline %v", armed, deadline)
	}
	if len(closed) != 0 {
		t.Fatal("the proxied dial let go of the proxy's connection before the connect's deadline")
	}
	if len(connected) != 0 {
		t.Fatalf("the connect returned %v before its deadline", <-connected)
	}

	fake.Set(deadline)
	if err := testwait.Receive(t, "the connect to end at its deadline", connected); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a connect through a proxy that never answers returned %v, want its deadline's error", err)
	}
	testwait.Receive(t, "the proxied dial to let go of the proxy's connection", closed)
}

// Two connects through one dialer at once record their dial errors in the one record and read it
// back, as a client's background recovery and a publish's replacement dial do: each dial stores on
// the goroutine nats.go dials from while the other connect loads at the end of its attempts. Run
// under -race (the envoy-go-race job), a record that is not synchronized fails here. Each connect
// makes the bus's ten attempts a second apart, so this takes about ten seconds.
func TestConnectsSharingADialerRecordItsLastError(t *testing.T) {
	dialer, err := socks5Proxy(proxiedLookup("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("read the proxy setting: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 500 * time.Millisecond)
			conn, err := connectWithContext(context.Background(), "shared-dialer-test", []string{"nats://nats.example.test:4222"}, nil, dialer, nil, nil, nil, nil, nil)
			if conn != nil {
				conn.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("connect %d through a dead proxy succeeded", i)
		}
		for _, want := range []string{SOCKS5ProxyEnvVar, "127.0.0.1:1", "connection refused"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("connect %d error %q does not name %q", i, err, want)
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("connect %d error %q is a deadline's, with no deadline set", i, err)
		}
	}
}
