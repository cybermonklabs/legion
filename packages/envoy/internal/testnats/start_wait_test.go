package testnats

import (
	"fmt"
	"net"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/testport"
)

// A server that cannot open a listener its configuration asks for logs a fatal error and will
// never accept connections, and its start fails at once with that log rather than waiting for the
// test's deadline (awaitReady). The row starts one whose leafnode port another listener holds, and
// needs the failure to name the fatal error: a wait that missed it would fail only at the test's
// deadline, naming that instead.
func TestAServerThatCannotOpenAListenerFailsItsStartAtOnce(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(recorder.message, "logged a fatal error") || !strings.Contains(recorder.message, "Error listening on leafnode port") {
		t.Fatalf("a start whose leafnode port %s is held failed with %q, want the fatal error naming the leafnode listener", held.Addr(), recorder.message)
	}
}

// A failed start of the package's shared server fails the test that asked for it, with its own
// reason, tears down what it set up, and the next test that asks starts the server afresh rather
// than fail on the earlier test's start (sharedServer). Under load a JetStream start can fail where
// a later one succeeds, and a failed start once failed every later test of the binary that asked.
// The row takes the shared server aside, plants a failure in the next start (startShared) after it
// registers a cleanup, as a real start registers its relay's and directory's, and asks twice: the
// first asker fails naming the planted failure with that cleanup run, and the second gets a server
// its client reaches. It counts the starts, so a server some earlier test had started cannot pass
// for one the second asker started.
func TestALaterTestStartsTheSharedServerAnEarlierStartFailedToStart(t *testing.T) {
	// Not parallel: it takes the package's shared server aside and sets startShared, which every
	// other test's URL reads, to a start that fails once.
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
	tornDown := false
	startShared = func(t testing.TB, accounts int) *accountServer {
		starts++
		if starts == 1 {
			t.Cleanup(func() { tornDown = true })
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
	if !tornDown {
		t.Fatal("the failed start of the shared server left the cleanup it registered unrun")
	}
	url := URL(t)
	if starts != 2 {
		t.Fatalf("the shared server was started %d times, want 2: the planted failure, then the next asker's start", starts)
	}
	Connect(t, url).Close()
}

// A server that logged a fatal error fails its start even once its client listener is up: nats-server
// v2.15.1 logs a profiler port it cannot bind as fatal and goes on starting (StartProfiler), and
// ReadyForConnections does not look at the profiler. The row holds the profiler's port at the address
// the profiler listens on, the server's own host (StartProfiler binds Options.Host), waits until the
// server both logged the fatal error and accepts connections, and only then asks awaitReady, so a wait
// that read the flag only while the server was not ready would pass it.
//
// The hold is a listener at that address, not on every address: whether a wildcard listener (0.0.0.0
// or [::]) keeps one address's bind off its port is the kernel's choice. Linux refuses that bind;
// gVisor's netstack takes it when both sockets set SO_REUSEADDR, as every net.Listen does, so under
// gVisor a wildcard hold let the profiler take the port, the server logged no fatal error, and the
// row failed on every run before it asked awaitReady. Linux and gVisor alike refuse a second listener
// on one address and port when neither sets SO_REUSEPORT, which net.Listen does not.
func TestAServerThatLoggedAFatalErrorFailsItsStartEvenOnceReady(t *testing.T) {
	t.Parallel()
	held, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("hold a port for the server's profiler: %v", err)
	}
	defer held.Close()
	at := held.Addr().(*net.TCPAddr)
	srv, err := natsserver.NewServer(&natsserver.Options{Host: at.IP.String(), Port: -1, ProfPort: at.Port, NoSigs: true})
	if err != nil {
		t.Fatalf("create the NATS server: %v", err)
	}
	log := &logCapture{}
	srv.SetLoggerV2(log, false, false, false)
	go srv.Start()
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	bound := boundOf(t)
	for !srv.ReadyForConnections(bound.step(retryInterval)) {
		if bound.over() {
			t.Fatalf("the server did not accept connections by %s, short of the test's deadline: %s", bound, log)
		}
	}
	if !log.fatalLogged() {
		t.Fatalf("the server accepts connections with no fatal error logged, so its profiler took the held port %s: %s", held.Addr(), log)
	}
	if err := awaitReady(t, srv, log); err == nil || !strings.Contains(err.Error(), "fatal error") {
		t.Fatalf("awaitReady of a server that accepts connections after logging a fatal error answered %v, want the fatal error: %s", err, log)
	}
}

// A server started again forgets a fatal error an earlier run of it logged: launch runs again at
// each Start, Restart and member restart, on the server's one log, and a flag the earlier run set
// would fail the new start at once. The row plants a fatal line in a running server's log, stops
// it and starts it again, which must answer.
func TestARestartedServerForgetsAFatalErrorOfItsEarlierRun(t *testing.T) {
	t.Parallel()
	server := start(t, "", []string{"-js"})
	server.log.Fatalf("a planted fatal error of the earlier run")
	server.Stop(t)
	server.Start(t)
	Connect(t, server.URL()).Close()
}
