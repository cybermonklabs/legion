package testnats

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// 2966). So while a handshake is held every read of the connection's state waits, until that timeout
// ends the attempt; the next attempt is held in turn. nats.go does not close a failed attempt's
// connection, so the hold cannot see an attempt end; it counts the CONNECTs (Attempts), and the rows
// read the end of an attempt from the lock's own state (lockTaken, lockLetGo).
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

// HoldReconnect holds (Hold) and waits until conn's reconnect handshake is held (AwaitAttempt): a
// CONNECT made after the call has reached the server through the hold, and conn's connection lock
// is taken. A bus client reconnects about a second after its connection drops (ReconnectWait).
func (h *HandshakeHold) HoldReconnect(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	before := h.Attempts()
	h.Hold()
	h.AwaitAttempt(t, conn, before)
}

// AwaitAttempt waits until a handshake attempt after the first after has reached the hold and conn's
// connection lock is taken (lockTaken), both read at once. nats.go writes an attempt's CONNECT under
// that lock and keeps it until the attempt fails at the client's connect timeout, so from the
// CONNECT on the lock is that attempt's. Two facts decide, the hold's count of CONNECTs and the
// lock's own state, and no reading of how long a call takes. Its one bound is the test binary's
// deadline less testwait's margin (testwait.Until), a guard against an attempt that never comes.
func (h *HandshakeHold) AwaitAttempt(t *testing.T, conn *natsgo.Conn, after int64) {
	t.Helper()
	lock, found := connLock(t, conn)
	if !found {
		t.FailNow()
	}
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

// HoldWriteLock holds conn's nats.go connection lock for writing on a connection through the hold
// that stays connected, as a write to a socket whose peer has stopped reading holds it: it freezes
// the proxy (Freeze), so nothing either side sends reaches the other, and takes the lock itself
// (takeConnLock). It returns once it holds the lock, which no reading of how long nats.go takes to
// answer decides: from then every nats.go call on conn that needs the lock - a publish, a flush, a
// close, a read of the status - waits until the func it returns thaws the proxy (Restore) and lets
// the lock go. The test's end calls that func too. When it cannot take the lock it fails the test by
// name without ending its goroutine, which may be a hook's rather than the test's (takeConnLock),
// and returns a func that only thaws the proxy. Since the lock is the row's own, no reset or close
// of the connection's socket lets it go: a row that measures what such a reset ends holds the lock
// with a write of nats.go's own (HoldStalledWrite).
func (h *HandshakeHold) HoldWriteLock(t *testing.T, conn *natsgo.Conn) (release func()) {
	t.Helper()
	h.Freeze()
	t.Cleanup(h.Restore)
	lock, taken := takeConnLock(t, conn)
	if !taken {
		return h.Restore
	}
	release = sync.OnceFunc(func() {
		h.Restore()
		lock.Unlock()
	})
	t.Cleanup(release)
	return release
}

// HoldStalledWrite holds conn's nats.go connection lock with a write of nats.go's own, on a
// connection through the hold that stays connected: it freezes the proxy (Freeze) and publishes to
// conn until a write to the socket blocks under the lock, as a write to a socket whose peer has
// stopped reading blocks. HoldWriteLock takes the lock itself, so only its release lets the lock go;
// here the lock is the write's, so a reset or a close of the write's socket (bus.Client.Sever) fails
// the write and lets the lock go, as it does in production.
//
// It returns once that write can no longer complete before the release, which is what a row that
// then waits behind the lock needs (stalledForGood). A write blocked on its socket is not enough:
// a blocked write resumes whenever more of the socket's bytes move to the proxy's end, and after the
// proxy is frozen they can go on moving for a while, through a relay read already under way or as
// the proxy's receive window grows. The publish then returns and lets the lock go, and a publish
// waiting for the lock takes it, as the hand-back in
// TestALostHoldSeversAPendingHandBackBehindAConnectedRead did. The func it returns thaws the proxy
// (Restore) and stops the publishes; the test's end calls it too. Its one bound is the test binary's
// deadline less stalledWriteSpare (testwait.Left), a guard against a write the socket always takes;
// then, with that much or less left to begin with, or when the kernel does not report the socket's
// state, it fails the test by name without ending its goroutine, which may be a hook's rather than
// the test's, and returns that func. A row calls RequireSocketQueues first, on its own goroutine, so
// that a kernel that reports no socket queues skips it outside CI rather than failing it here.
func (h *HandshakeHold) HoldStalledWrite(t *testing.T, conn *natsgo.Conn) (release func()) {
	t.Helper()
	h.Freeze()
	stop := make(chan struct{})
	release = sync.OnceFunc(func() {
		close(stop)
		h.Restore()
	})
	t.Cleanup(release)
	lock, found := connLock(t, conn)
	if !found {
		return release
	}
	bound, err := testwait.Left(t, stalledWriteSpare)
	if err != nil {
		t.Error(err)
		return release
	}
	publisher := make(chan uint64, 1)
	var returned atomic.Int64
	go publishUntil(conn, stop, publisher, &returned)
	id := <-publisher
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		stalled, err := h.stalledForGood(id, &returned)
		if err != nil {
			t.Error(err)
			return release
		}
		if stalled && lockTaken(lock) {
			return release
		}
	}
	t.Errorf("no write of nats.go's was stalled for good on a socket to the hold's proxy, holding its connection lock, %s before the test binary's deadline: the frozen proxy's socket kept taking the publishes", stalledWriteSpare)
	return release
}

// stalledForGood reports whether publishUntil's goroutine, numbered id, is blocked in a write that
// cannot complete until the proxy is restored. That needs three facts, read in this order:
//   - the relay readers waiting at the freeze's gate (gatedReaders): such a reader reads no more of
//     its client;
//   - every established socket to the proxy that holds bytes to send has its zero-window probe
//     timer armed (clientSockets), and its connection's reader is among those at the gate: the
//     proxy's end has closed its receive window, the socket has nothing in flight, and the kernel
//     moves none of its bytes until the proxy reads, which the first fact rules out;
//   - the goroutine was blocked in the same write (writeBlocked, with no publish returned) from
//     before the kernel's account was read until after it.
//
// Such a write waits for room in a send queue that nothing empties. It reads the state of the
// goroutine, the proxy and the kernel, and no time. It answers an error when the write stayed blocked
// across the kernel's account and no socket to the proxy showed a send queue: the kernel reports no
// queues there, which a row rules out first with RequireSocketQueues, or conn does not go through the
// hold.
func (h *HandshakeHold) stalledForGood(id uint64, returned *atomic.Int64) (bool, error) {
	before := returned.Load()
	if !writeBlocked(id) {
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
	if !writeBlocked(id) || returned.Load() != before {
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
		return false, fmt.Errorf("a write of nats.go's stayed blocked on its socket while /proc/net/tcp showed no bytes to send on any of the %d established socket(s) to the hold's proxy at %s: the connection does not go through the hold, or this kernel reports no socket queues there and the row did not call RequireSocketQueues first, so HoldStalledWrite cannot read that the write is stalled for good", len(sockets), server)
	}
	return true, nil
}

// stalledWriteSpare is what HoldStalledWrite leaves of the test binary's deadline for the failed
// test's cleanups.
const stalledWriteSpare = 30 * time.Second

// publishUntil sends its own goroutine's id on id, then publishes 64 KiB at a time to conn, counting
// each publish that returns in returned, until stop closes or a publish fails. Each publish of that
// size flushes in place, under nats.go's connection lock (natsWriter.appendBufs, nats.go
// v1.55.0:2364), so once the socket stops taking bytes the publish blocks in the socket's write
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

// writeBlocked reports whether the goroutine numbered id, publishUntil's, is blocked waiting for its
// socket to take a write: in the stacks the runtime reports for every goroutine, its own says "IO
// wait" and runs through internal/poll's FD.Write. That goroutine does nothing but publish, so a
// write it is blocked in is a publish's. Its nats.go frames are not matched: the runtime names that
// package github.com/nats-io/nats%2ego, writing the dot of its last path element escaped. It reads
// the goroutine's state, not how long it has been in it.
func writeBlocked(id uint64) bool {
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
	lock, found := connLock(t, conn)
	if !found {
		t.FailNow()
	}
	if !lockTaken(lock) {
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

// AnswersWithin runs each call at once, each on a goroutine of its own, and returns their answers in
// order. It fails the test, naming what, when any call has not answered within within of the start.
func AnswersWithin(t testing.TB, within time.Duration, what string, calls ...func() error) []error {
	t.Helper()
	answers := make([]chan error, len(calls))
	for i, call := range calls {
		answers[i] = make(chan error, 1)
		go func() { answers[i] <- call() }()
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	got := make([]error, len(calls))
	for i, answer := range answers {
		select {
		case got[i] = <-answer:
		case <-deadline.C:
			t.Fatalf("%s %d of %d gave no answer within %s while nats.go's lock was held: it waited for the lock", what, i+1, len(calls), within)
		}
	}
	return got
}

// AwaitLockFree waits until nats.go lets conn's connection lock go, failing the test when it has not
// by the test's deadline (testDeadline). With every reconnect's handshake held (HoldReconnect), that
// is the end of an attempt, when nats.go reads the connection as reconnecting and waits between
// attempts (ReconnectWait, 1 s for a bus client) with its lock free.
func AwaitLockFree(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	read := make(chan struct{})
	go func() {
		conn.Status()
		close(read)
	}()
	if bound := boundOf(t); !awaitSignal(bound, read) {
		t.Fatalf("nats.go still held its connection lock at %s, short of the test's deadline: no attempt's handshake ended", bound)
	}
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
// attempt the hold held when they began. Once all have answered it reads, in this order: that the
// lock is taken (RequireLockHeld), that nats.go has not let it go since they began (lockLetGo), and
// last that no later attempt has reached the hold, by its dial (Dials) or its CONNECT (Attempts). A
// call that waits for nats.go's lock answers only once that attempt has ended, at the client's
// connect timeout. Read in the gap after it, the lock is free. Read later, however long the check
// was held up and wherever, either lockLetGo has closed its channel, since its read is granted at
// that attempt's Unlock, or the next attempt has taken the lock and dials the hold right after, so
// the counts read after the lock have moved. What is left takes two delays at once: lockLetGo's
// goroutine not yet run past its read through the gap after the attempt (ReconnectWait), and the
// check held up past that gap too. Then the next attempt's Lock waits behind lockLetGo's granted
// read with nothing dialed, or the lock read lands before the hold has accepted that attempt's dial,
// and the lock reads taken, the channel open and the counts unmoved. No delay of one goroutine alone
// gets there. No reading of how long a call takes decides; the one wall-clock bound is a 30 s guard
// against a call that never answers.
func (h *HandshakeHold) AnswersWithinAttempt(t testing.TB, conn *natsgo.Conn, what string, calls ...func() error) []error {
	t.Helper()
	began, dialed := h.Attempts(), h.Dials()
	letGo := lockLetGo(conn)
	answers := AnswersWithin(t, 30*time.Second, what, calls...)
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
	server, err := net.DialTimeout("tcp", h.target, dialTimeout)
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
