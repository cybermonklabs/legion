package testnats

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// HandshakeHold is a Proxy whose relay to the server can hold a reconnect's handshake: Hold drops
// every connection the proxy relays, so a client through it reconnects, and until Release it holds
// the server's answer to the CONNECT of each connection made meanwhile. The server's INFO reaches
// the client and the client's CONNECT reaches the server; nothing more the server sends does, as at
// a hub whose auth service has not answered the CONNECT. nats.go holds its connection lock through a
// reconnect attempt's dial and handshake: doReconnect takes it before the dial and lets it go only
// once the attempt has failed (v1.55.0 nats.go:3554-3588, then 3491), and processConnectInit writes
// the CONNECT under it and waits for the answer until the client's connect timeout (nats.go:2944,
// 2966). So while a handshake is held every read of the connection's state waits. The client's own
// connect timeout would end the attempt, and the next would be held in turn; HoldReconnect gives
// the client a connect timeout of the test's own (OwnConnectTimeout), so a held attempt ends only
// when the row ends it: Release lets the answer through, and Cut drops the connection. nats.go does
// not close a failed attempt's connection, so the hold cannot see an attempt end; it counts the
// CONNECTs (Attempts), and the rows read the end of an attempt from the lock's own state
// (lockTaken, lockLetGo).
//
// The proxy (Cut, Freeze, Restore) carries the client's connections, and the hold's own relay sits
// between it and the server. Restore ends a Cut or a Freeze and leaves a hold in force; Release
// ends the hold.
type HandshakeHold struct {
	*Proxy
	// target is the server's address, which the hold's relay dials.
	target   string
	listener net.Listener

	mu sync.Mutex
	// release is nil while connections are relayed as they are, and open from Hold until Release.
	release chan struct{}
	// held is closed once a CONNECT has reached the server under the hold Hold began.
	held     chan struct{}
	heldOnce *sync.Once
	// attempts counts the CONNECTs that have reached the server under a hold: each is one handshake
	// attempt whose answer the hold keeps.
	attempts atomic.Int64
	// dials counts the connections the hold has accepted while a hold was in force: each is a
	// handshake attempt that has dialed, its CONNECT counted in attempts or still on its way.
	dials atomic.Int64
	// ended counts the connections the hold has finished with, under a hold or not: join adds one
	// as the last thing it does for a connection, once the direction to the client has returned,
	// so whatever the hold counts for a connection it has counted by then.
	ended atomic.Int64
}

// StartHandshakeHold relays to the NATS server at target, a client URL such as Start returns,
// through a proxy and the hold's own relay, each on a loopback address and port of its own
// (testport.LoopbackIP), until the test ends. URL is the same server through both.
func StartHandshakeHold(t testing.TB, target string) *HandshakeHold {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the handshake hold relays to: %v", target, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the handshake hold: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	h := &HandshakeHold{target: server.Host, listener: listener}
	go h.serve()
	relayed := *server
	relayed.Host = listener.Addr().String()
	h.Proxy = StartProxy(t, relayed.String())
	return h
}

// Hold drops every connection the proxy relays (Cut, then Restore) and holds the answer to the
// CONNECT of each connection made from now until Release. The channel it returns is closed once a
// client's CONNECT has reached the server through the hold, so that client's handshake is waiting
// for the answer the hold keeps.
func (h *HandshakeHold) Hold() <-chan struct{} {
	h.mu.Lock()
	if h.release == nil {
		h.release, h.held, h.heldOnce = make(chan struct{}), make(chan struct{}), new(sync.Once)
	}
	held := h.held
	h.mu.Unlock()
	h.Cut()
	h.Restore()
	return held
}

// HoldReconnect gives conn a connect timeout of the test's own (OwnConnectTimeout), holds (Hold)
// and waits until conn's reconnect handshake is held (AwaitAttempt): a CONNECT made after the call
// has reached the server through the hold, and conn's connection lock is taken. The wait is
// testwait's, so a reconnect that never reaches the hold fails the test naming what it waited for.
// A bus client reconnects about a second after its connection drops (ReconnectWait). The attempt
// it holds ends only when the row ends it (Release, Cut).
func (h *HandshakeHold) HoldReconnect(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	OwnConnectTimeout(t, conn)
	before := h.Attempts()
	h.Hold()
	h.AwaitAttempt(t, conn, before)
}

// OwnConnectTimeout sets conn's connect timeout to what is left of the test binary's deadline less
// testwait's margin, and with no deadline (-timeout 0) to no bound a test can reach, as
// RequestTimeout bounds a JetStream request. nats.go's connect timeout (Options.Timeout, v1.55.0)
// bounds two things. One is the TCP dial, through the net.Dialer that Options.Connect builds from it
// once (nats.go:2017-2021); this leaves that dial's bound as Connect set it, since the proxy accepts
// every dial at once. The other is each connect and reconnect attempt's whole handshake, read at each
// attempt: processConnectInit sets the socket's deadline that far ahead (nats.go:2944) for the
// server's INFO, any TLS handshake, and the CONNECT and PING through the server's PONG
// (sendConnect). A hold keeps that PONG, so the handshake's deadline is what ends a held attempt, at
// the client's own timeout: nats.go's DefaultTimeout (2 s), or a bus client's connectTimeout (5 s).
// A row that reads a held attempt then turns on which of two real timers fires first, that one or
// the bound under test (an intake stop's callNATS, bus.ConnLockWait, 100 ms), and a loaded runner
// can reverse them. With the test's own timeout a held attempt ends only when the row ends it, so
// the bound under test is the one bound in the row. nats.go reads the timeout with its connection
// lock held (createConn and processConnectInit run under it), and its Conn.Opts doc calls a change
// to a running connection's options a race, so the timeout is set with that lock taken (connLock),
// as HoldWriteLock takes it.
func OwnConnectTimeout(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	timeout := left(t)
	lock := connLock(t, conn)
	testwait.Eventually(t, "nats.go to let its connection lock go, so the row can set the connect timeout under it", lock.TryLock)
	conn.Opts.Timeout = timeout
	lock.Unlock()
}

// AwaitLockHeld waits until conn's connection lock is taken (lockTaken), as a held handshake or a
// held write takes it.
func AwaitLockHeld(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	lock := connLock(t, conn)
	testwait.Eventually(t, "nats.go to hold its connection lock: a handshake or a write holding it", func() bool {
		return lockTaken(lock)
	})
}

// AwaitAttempt waits until a handshake attempt after the first after has reached the hold and conn's
// connection lock is taken (lockTaken), both read at once. nats.go writes an attempt's CONNECT under
// that lock and keeps it until the attempt fails at the client's connect timeout, or, under
// HoldReconnect, until the row ends it, so from the CONNECT on the lock is that attempt's. Two facts
// decide, the hold's count of CONNECTs and the lock's own state, and no reading of how long a call
// takes. It waits through the shared wait (testwait.Until), which has no bound of its own.
func (h *HandshakeHold) AwaitAttempt(t testing.TB, conn *natsgo.Conn, after int64) {
	t.Helper()
	lock := connLock(t, conn)
	what := fmt.Sprintf("a handshake attempt after the first %d to reach the hold, with nats.go's connection lock taken", after)
	testwait.Until(t, what, func() (string, bool) {
		attempts := h.Attempts()
		taken := lockTaken(lock)
		return fmt.Sprintf("%d attempt(s) at the hold, lock taken %t", attempts, taken), attempts > after && taken
	})
}

// Release ends a Hold: every answer it kept moves on, and connections made from now on are relayed
// as they are.
func (h *HandshakeHold) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release != nil {
		close(h.release)
		h.release = nil
	}
}

// HoldWriteLock holds conn's nats.go connection lock as a write to a socket that does not drain
// holds it, on a connection that stays connected: it takes that lock itself (TakeWriteLock), so
// from the moment it returns every call of conn's that reads or writes the connection waits, as
// each waits behind such a write, and nothing about the connection changes. The fixture takes the
// lock rather than filling the socket, so no span decides when a write blocked: the lock is held
// when the take returns, and let go when the row calls the func it returns, which the test's end
// calls too. The take's give-up fails the test. The proxy is left as it is. Since the lock is the
// row's own, no reset or close of the connection's socket lets it go: a row that measures what such
// a reset ends holds the lock with a write of nats.go's own (HoldStalledWrite).
func (h *HandshakeHold) HoldWriteLock(t testing.TB, conn *natsgo.Conn) (release func()) {
	t.Helper()
	release, err := h.TakeWriteLock(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

// TakeWriteLock takes conn's nats.go connection lock (connLockOf) as HoldWriteLock describes, with
// no t, for a hook the code under test runs on a goroutine of its own, where no method of t that
// ends the test may run: it waits for the lock through the shared wait's helper form
// (testwait.InHelper), with the bound every wait has, and returns that wait's give-up, or a nats.go
// whose Conn holds no connection lock, as an error rather than failing a test. It registers no
// cleanup: the hook hands the error to the test goroutine, which fails the row on it, and has the
// release run at the test's end. The release may be called more than once.
func (h *HandshakeHold) TakeWriteLock(conn *natsgo.Conn) (release func(), err error) {
	lock, err := connLockOf(conn)
	if err != nil {
		return nil, err
	}
	if err := testwait.InHelper("testnats", "nats.go to let its connection lock go, so the fixture can take it as a write does", lock.TryLock); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(lock.Unlock) }, nil
}

// HoldStalledWrite holds conn's nats.go connection lock with a write of nats.go's own, on a
// connection through the hold that stays connected: it freezes the proxy (Freeze) and publishes to
// conn until a write to the socket blocks under the lock, as a write to a socket whose peer has
// stopped reading blocks. HoldWriteLock takes the lock itself, so only its release lets the lock go;
// here the lock is the write's, so a reset or a close of the write's socket (bus.Client.Sever) fails
// the write and lets the lock go, as it does in production.
//
// It returns once that write can no longer complete before the release, which is what a row that
// then waits behind the lock needs (stalledForGood). A write parked on its socket is not enough: a
// parked write resumes whenever more of the socket's bytes move to the proxy's end, and after the
// proxy is frozen they can go on moving for a while, through a relay read already under way or as
// the proxy's receive window grows. The publish then returns and lets the lock go, and a publish
// waiting for the lock takes it, as the hand-back in
// TestALostHoldSeversAPendingHandBackBehindAConnectedRead did. The func it returns thaws the proxy
// (Restore) and stops the publishes; the test's end calls it too. It waits through the shared
// wait's helper form (testwait.InHelper), and when no write is stalled for good by the time that
// wait gives up, or when the kernel does not report the socket's state, it fails the test by name
// without ending its goroutine, which may be a hook's rather than the test's, and returns that
// func. A row calls RequireSocketQueues first, on its own goroutine, so that a kernel that reports
// no socket queues skips it outside CI rather than failing it here.
func (h *HandshakeHold) HoldStalledWrite(t *testing.T, conn *natsgo.Conn) (release func()) {
	t.Helper()
	h.Freeze()
	stop := make(chan struct{})
	release = sync.OnceFunc(func() {
		close(stop)
		h.Restore()
	})
	t.Cleanup(release)
	lock, err := connLockOf(conn)
	if err != nil {
		t.Errorf("testnats: %v", err)
		return release
	}
	publisher := make(chan uint64, 1)
	var returned atomic.Int64
	go publishUntil(conn, stop, publisher, &returned)
	id := <-publisher
	// stalledErr is what stalledForGood answered when it could not read the write's state: it ends
	// the wait, and the row fails on it.
	var stalledErr error
	if err := testwait.InHelper("testnats", "a write of nats.go's own to be stalled for good on a socket to the hold's proxy, holding its connection lock", func() bool {
		stalled, err := h.stalledForGood(id, &returned)
		if err != nil {
			stalledErr = err
			return true
		}
		return stalled && lockTaken(lock)
	}); err != nil {
		t.Errorf("%v", err)
		return release
	}
	if stalledErr != nil {
		t.Errorf("testnats: %v", stalledErr)
	}
	return release
}

// stalledForGood reports whether publishUntil's goroutine, numbered id, is parked in a write that
// cannot complete until the proxy is restored. That needs three facts, read in this order:
//   - the relay readers waiting at the freeze's gate (gatedReaders): such a reader reads no more of
//     its client;
//   - every established socket to the proxy that holds bytes to send has its zero-window probe
//     timer armed (clientSockets), and its connection's reader is among those at the gate: the
//     proxy's end has closed its receive window, the socket has nothing in flight, and the kernel
//     moves none of its bytes until the proxy reads, which the first fact rules out;
//   - the goroutine was parked in the same write (writeParked, with no publish returned) from
//     before the kernel's account was read until after it.
//
// Such a write waits for room in a send queue that nothing empties. It reads the state of the
// goroutine, the proxy and the kernel, and no time. It answers an error when the write stayed parked
// across the kernel's account and no socket to the proxy showed a send queue: the kernel reports no
// queues there, which a row rules out first with RequireSocketQueues, or conn does not go through the
// hold.
func (h *HandshakeHold) stalledForGood(id uint64, returned *atomic.Int64) (bool, error) {
	before := returned.Load()
	if !writeParked(id) {
		return false, nil
	}
	gated := h.gatedReaders()
	// The client dials the proxy's listener; h.listener is the hold's own relay, behind the proxy.
	server, ok := h.Proxy.listener.Addr().(*net.TCPAddr)
	if !ok {
		return false, fmt.Errorf("the hold's proxy listens on %s, not a TCP address", h.Proxy.listener.Addr())
	}
	sockets, err := clientSockets(server)
	if err != nil {
		return false, fmt.Errorf("read the sockets to the hold's proxy at %s: %w", server, err)
	}
	if !writeParked(id) || returned.Load() != before {
		return false, nil
	}
	loaded := 0
	for _, socket := range sockets {
		if socket.sendQueue == 0 {
			continue
		}
		loaded++
		if !socket.zeroWindowProbe || !gated[socket.local] {
			return false, nil
		}
	}
	if loaded == 0 {
		return false, fmt.Errorf("a write of nats.go's stayed parked on its socket while /proc/net/tcp showed no bytes to send on any of the %d established socket(s) to the hold's proxy at %s: the connection does not go through the hold, or this kernel reports no socket queues there and the row did not call RequireSocketQueues first, so HoldStalledWrite cannot read that the write is stalled for good", len(sockets), server)
	}
	return true, nil
}

// publishUntil sends its own goroutine's id on id, then publishes 64 KiB at a time to conn, counting
// each publish that returns in returned, until stop closes or a publish fails. Each publish of that
// size flushes in place, under nats.go's connection lock (natsWriter.appendBufs, nats.go
// v1.55.0:2364), so once the socket stops taking bytes the publish parks in the socket's write
// holding the lock.
func publishUntil(conn *natsgo.Conn, stop <-chan struct{}, id chan<- uint64, returned *atomic.Int64) {
	id <- goroutineID()
	payload := make([]byte, 64<<10)
	for {
		select {
		case <-stop:
			return
		default:
		}
		err := conn.Publish("testnats.stalled-write", payload)
		returned.Add(1)
		if err != nil {
			return
		}
	}
}

// goroutineID is the calling goroutine's id, as the first line of its stack names it.
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	var id uint64
	_, _ = fmt.Sscanf(string(buf), "goroutine %d ", &id)
	return id
}

// writeParked reports whether the goroutine numbered id, publishUntil's, is parked waiting for its
// socket to take a write: in the stacks the runtime reports for every goroutine, its own says "IO
// wait" and runs through internal/poll's FD.Write. That goroutine does nothing but publish, so a
// write it is parked in is a publish's. Its nats.go frames are not matched: the runtime names that
// package github.com/nats-io/nats%2ego, writing the dot of its last path element escaped. It reads
// the goroutine's state, not how long it has been in it.
func writeParked(id uint64) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	header := fmt.Sprintf("goroutine %d [", id)
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.HasPrefix(stack, header) {
			return strings.HasPrefix(stack[len(header):], "IO wait") && strings.Contains(stack, "internal/poll.(*FD).Write(")
		}
	}
	return false
}

// RequireLockHeld fails the test unless conn's connection lock is taken now (lockTaken): the
// handshake, HoldWriteLock or HoldStalledWrite the row holds it with still holds it. A row calls it
// once the calls it measures have answered, so their answers are known to have come while the lock
// was held. It reads the lock's own state and waits for nothing.
func RequireLockHeld(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	if !lockTaken(connLock(t, conn)) {
		t.Fatal("nats.go's connection lock was free once the calls had answered: neither a handshake nor a write held it then")
	}
}

// lockTaken reports whether lock, a connection's nats.go lock (connLock), is held by a writer or
// waited for by one now: whether a read of the connection's status would wait. A held handshake and
// HoldWriteLock each hold it for writing. It reads the lock's own state (TryRLock) at that instant
// and waits for nothing; which attempt holds it, it does not say: a check that runs after the held
// attempt has ended can find the next attempt's write lock taken (AnswersWithinAttempt).
func lockTaken(lock *sync.RWMutex) bool {
	if lock.TryRLock() {
		lock.RUnlock()
		return false
	}
	return true
}

// lockLetGo returns a channel closed once nats.go has let conn's connection lock go from now on: a
// read of conn's status begun now waits while a writer holds the lock, and sync.RWMutex hands a
// waiting read the lock at the writer's Unlock, ahead of the next writer; a read begun with the lock
// free closes it at once. The channel closes only once this goroutine has run on after that read,
// so a check that finds it open has learned nothing: it is one more way to fail, never a reason to
// pass.
func lockLetGo(conn *natsgo.Conn) <-chan struct{} {
	letGo := make(chan struct{})
	go func() {
		conn.Status()
		close(letGo)
	}()
	return letGo
}

// connLock is nats.go's connection lock on conn: the sync.RWMutex every call that reads or writes
// the connection takes (Conn.mu, nats.go v1.55.0 nats.go:659), which a reconnect's dial and
// handshake hold (doReconnect) and a write to a socket that does not drain holds. nats.go does not
// export it, so it is read through reflect, as cmd/listener's rows read the fields of the
// listener's own types; a nats.go whose Conn holds no such field fails the test, naming it.
func connLock(t testing.TB, conn *natsgo.Conn) *sync.RWMutex {
	t.Helper()
	lock, err := connLockOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

// connLockOf is connLock with no t: a nats.go whose Conn holds no such field is its error.
func connLockOf(conn *natsgo.Conn) (*sync.RWMutex, error) {
	field := reflect.ValueOf(conn).Elem().FieldByName("mu")
	if !field.IsValid() || field.Type() != reflect.TypeFor[sync.RWMutex]() {
		return nil, errors.New("nats.go's Conn holds no mu sync.RWMutex, the connection lock the handshake hold reads (nats.go v1.55.0 nats.go:659)")
	}
	return (*sync.RWMutex)(unsafe.Pointer(field.UnsafeAddr())), nil
}

// Answers runs each call at once, each on a goroutine of its own, and returns their answers in
// order. Each answer is waited for through testwait, so a call that never answers fails the test
// naming what and its place.
func Answers(t testing.TB, what string, calls ...func() error) []error {
	t.Helper()
	answers := make([]chan error, len(calls))
	for i, call := range calls {
		answers[i] = make(chan error, 1)
		go func() { answers[i] <- call() }()
	}
	got := make([]error, len(calls))
	for i, answer := range answers {
		got[i] = testwait.Receive(t, fmt.Sprintf("%s %d of %d to answer", what, i+1, len(calls)), answer)
	}
	return got
}

// AwaitLockFree waits until nats.go lets conn's connection lock go (lockTaken). A handshake held
// through HoldReconnect ends only when the row ends it; once Cut has dropped its connection, nats.go
// reads the connection as reconnecting and waits between attempts (ReconnectWait, 1 s for a bus
// client) with its lock free, and each attempt it makes while the proxy is cut ends at once.
func AwaitLockFree(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	lock := connLock(t, conn)
	testwait.Eventually(t, "nats.go to let its connection lock go: the held attempt to end", func() bool {
		return !lockTaken(lock)
	})
}

// Attempts is how many handshake attempts have reached the server under a hold: each CONNECT the
// hold has kept the answer to.
func (h *HandshakeHold) Attempts() int64 { return h.attempts.Load() }

// Dials is how many connections the hold has accepted while a hold was in force: nats.go dials for
// each handshake attempt right after taking its connection lock, before it writes the CONNECT
// Attempts counts.
func (h *HandshakeHold) Dials() int64 { return h.dials.Load() }

// AnswersWithinAttempt runs each call at once, each on a goroutine of its own, and returns their
// answers in order. It fails the test, naming what, unless every call answered within the handshake
// attempt the hold held when they began. Under HoldReconnect that attempt ends only when the row
// ends it (OwnConnectTimeout), so a call that waits for nats.go's lock gives no answer while the
// hold holds, and the wait for its answer (Answers) fails the test, naming the call. An attempt
// held without HoldReconnect ends at the client's own connect timeout, and a call that waits for the
// lock answers then. So once all have answered it reads, in this order: that the lock is taken
// (RequireLockHeld), that nats.go has not let it go since they began (lockLetGo), and last that no
// later attempt has reached the hold, by its dial (Dials) or its CONNECT (Attempts). Read in the gap
// after the attempt, the lock is free. Read later, however long the check was held up and wherever,
// either lockLetGo has closed its channel, since its read is granted at that attempt's Unlock, or
// the next attempt has taken the lock and dials the hold right after, so the counts read after the
// lock have moved. What is left takes two delays at once: lockLetGo's goroutine not yet run past its
// read through the gap after the attempt (ReconnectWait), and the check held up past that gap too.
// Then the next attempt's Lock waits behind lockLetGo's granted read with nothing dialed, or the
// lock read lands before the hold has accepted that attempt's dial, and the lock reads taken, the
// channel open and the counts unmoved. No delay of one goroutine alone gets there. No reading of how
// long a call takes decides, and its waits are testwait's, with no bound of their own.
func (h *HandshakeHold) AnswersWithinAttempt(t testing.TB, conn *natsgo.Conn, what string, calls ...func() error) []error {
	t.Helper()
	began, dialed := h.Attempts(), h.Dials()
	letGo := lockLetGo(conn)
	answers := Answers(t, what, calls...)
	RequireLockHeld(t, conn)
	select {
	case <-letGo:
		t.Fatalf("%s answered only after nats.go had let its connection lock go: it waited out the attempt held when it began", what)
	default:
	}
	if ended, dials := h.Attempts(), h.Dials(); ended != began || dials != dialed {
		t.Fatalf("%s answered only after %d more handshake attempt(s) had dialed the hold (%d more had reached it with a CONNECT): it waited out the attempt held when it began", what, dials-dialed, ended-began)
	}
	return answers
}

// serve relays each connection the proxy makes to the hold until the hold's listener closes.
func (h *HandshakeHold) serve() {
	for {
		client, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.join(client)
	}
}

// join relays client, a connection the proxy made for one of its clients, to the server, under the
// hold when one is in force as client arrives, until either side ends; then it closes both, waits
// for the direction to the client to return, and counts the connection as ended, last. The
// client's stream is read as NATS protocol (protocolStream) to find its CONNECT, which may come in
// pieces.
func (h *HandshakeHold) join(client net.Conn) {
	defer h.ended.Add(1)
	defer client.Close()
	h.mu.Lock()
	release, held, heldOnce := h.release, h.held, h.heldOnce
	h.mu.Unlock()
	if release != nil {
		h.dials.Add(1)
	}
	server, err := net.Dial("tcp", h.target)
	if err != nil {
		return
	}
	gone, relayed := make(chan struct{}), make(chan struct{})
	defer func() {
		close(gone)
		_ = server.Close()
		_ = client.Close()
		<-relayed
	}()
	go func() {
		defer close(relayed)
		defer client.Close()
		defer server.Close()
		answers := bufio.NewReader(server)
		if release != nil {
			info, err := answers.ReadBytes('\n')
			if err != nil {
				return
			}
			if _, err := client.Write(info); err != nil {
				return
			}
			select {
			case <-release:
			case <-gone:
				return
			}
		}
		_, _ = io.Copy(client, answers)
	}()
	var stream protocolStream
	buffer := make([]byte, 32<<10)
	for {
		read, err := client.Read(buffer)
		if read > 0 {
			if _, err := server.Write(buffer[:read]); err != nil {
				return
			}
			if release != nil && !stream.connected {
				stream.wrote(buffer[:read])
				if stream.connected {
					h.attempts.Add(1)
					heldOnce.Do(func() { close(held) })
				}
			}
		}
		if err != nil {
			return
		}
	}
}
