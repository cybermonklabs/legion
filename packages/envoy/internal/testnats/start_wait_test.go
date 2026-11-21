package testnats

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// A server that cannot open a listener its configuration asks for logs a fatal error and will
// never accept connections, and its start fails at once with that log rather than waiting for the
// test's deadline (awaitReady). The row starts one whose leafnode port another listener holds,
// through a t with no deadline (fatalRecorder has no Deadline method), so a wait that missed the
// fatal error would last as long as the binary.
func TestAServerThatCannotOpenAListenerFailsItsStartAtOnce(t *testing.T) {
	held, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("hold a port for the server's leafnode listener: %v", err)
	}
	defer held.Close()
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		start(recorder, fmt.Sprintf("leafnodes {\n  listen: %q\n}\n", held.Addr().String()), nil)
	}()
	<-done
	if !strings.Contains(recorder.message, "logged a fatal error") || !strings.Contains(recorder.message, "leafnode") {
		t.Fatalf("a start whose leafnode port %s is held failed with %q, want the fatal error naming the leafnode listener", held.Addr(), recorder.message)
	}
}

// A failed start of the package's shared server fails the test that asked for it, with its own
// reason, and the next test that asks starts the server afresh rather than fail on the earlier
// test's start (sharedServer). Under load a JetStream start can fail where a later one succeeds,
// and a failed start once failed every later test of the binary that asked. The row takes the
// shared server aside, plants a failure in the next start (startShared), and asks twice: the first
// asker fails naming the planted failure, and the second gets a server its client reaches. It
// counts the starts, so a server some earlier test had started cannot pass for one the second
// asker started.
func TestALaterTestStartsTheSharedServerAnEarlierStartFailedToStart(t *testing.T) {
	sharedMu.Lock()
	aside, asideCleanup := shared, sharedCleanup
	shared, sharedCleanup = nil, nil
	sharedMu.Unlock()
	startServer := startShared
	t.Cleanup(func() {
		startShared = startServer
		sharedMu.Lock()
		defer sharedMu.Unlock()
		if sharedCleanup != nil {
			sharedCleanup.run()
		}
		shared, sharedCleanup = aside, asideCleanup
	})
	starts := 0
	startShared = func(t testing.TB, accounts int) *accountServer {
		starts++
		if starts == 1 {
			t.Fatalf("a planted failure of the shared server's start")
		}
		return startServer(t, accounts)
	}
	first := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		URL(first)
	}()
	<-done
	if !strings.Contains(first.message, "planted failure") {
		t.Fatalf("the test whose start of the shared server failed reported %q, want the planted failure", first.message)
	}
	url := URL(t)
	if starts != 2 {
		t.Fatalf("the shared server was started %d times, want 2: the planted failure, then the next asker's start", starts)
	}
	Connect(t, url).Close()
}
