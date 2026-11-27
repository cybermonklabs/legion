package bus

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
)

// A connect that no server answers dials connectTries times and waits connectRetryWait after each
// dial that fails, the last one included, before it returns the last dial's error: every caller of
// Connect, ConnectOwningStream without a credential wait, Dial and a client's recovery keeps that
// start as it was before the listener's credential wait came in. The server here accepts each dial
// and closes it at once, so each dial fails as soon as it is made and records when it was made.
//
// A connect that dropped the wait after its last dial returns within milliseconds of that dial,
// and the gap this test reads fails.
func TestAConnectThatFailsEveryTryWaitsAfterItsLastAsBefore(t *testing.T) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var dials []time.Time
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			dials = append(dials, time.Now())
			mu.Unlock()
			_ = conn.Close()
		}
	}()
	url := "nats://" + listener.Addr().(*net.TCPAddr).String()

	conn, err := connectWithContext(context.Background(), slog.Default(), "trailing-wait-test", []string{url}, nil, nil, nil, nil, nil, nil, nil)
	returned := time.Now()
	if err == nil {
		conn.Close()
		t.Fatal("a connect to a server that closes every dial connected")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dials) != connectTries {
		t.Fatalf("the connect dialled %d times, want %d", len(dials), connectTries)
	}
	// Half the wait is the margin: a scheduler delay only lengthens a wait, and a connect without
	// the wait after its last dial returns within milliseconds of it.
	for i := 1; i < len(dials); i++ {
		if gap := dials[i].Sub(dials[i-1]); gap < connectRetryWait/2 {
			t.Fatalf("dial %d came %s after dial %d, want about %s", i+1, gap, i, connectRetryWait)
		}
	}
	if after := returned.Sub(dials[len(dials)-1]); after < connectRetryWait/2 {
		t.Fatalf("the connect returned %s after its last dial, want about %s: it waits after its last failed dial as after every other", after, connectRetryWait)
	}
}
