// packages/envoy/internal/broker/airlockproxy/upgrade_test.go
//
// A WebSocket upgrade on a bound host: the swap reaches the upgrade request's own headers, the
// Upgrade and Connection headers survive it, and once the service answers 101 the two
// connections are joined and bytes pass both ways until either side closes. Two things end such
// a connection: the idle limit, which is the command's own connection's and so one the proxy
// already bounds, and the grant behind it, which no request on that connection reads again - so
// the proxy reads it for the connection itself and closes the connection when the grant stops
// carrying it.
package airlockproxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// upgradePath is the path these rows upgrade at: the stand-in service answers an upgrade
// there and its own body everywhere else, as S does in the broker's local stack.
const upgradePath = "/v1/socket"

// upgradeStandIn makes the stand-in service answer 101 at that path and then speak the little
// line protocol these rows pass frames with: one line in, one line out. It is airlocktest's
// own upgrade stand, the one cmd/dev-airlock-stands installs on S, so these rows and a check
// script drive the one answer.
func upgradeStandIn(t *testing.T, r *rig) {
	t.Helper()
	r.answerWith(airlocktest.NewUpgradeStand(airlocktest.UpgradeOptions{Path: upgradePath}).Answer)
}

// upgradeThrough opens a connection to the bound host through the proxy, sends the upgrade
// request on it, and answers the 101 and the connection the frames pass on.
func upgradeThrough(t *testing.T, r *rig) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	answer, conn, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the CONNECT answered %d; want 200", answer.StatusCode)
	}
	// The proxy's own certificate, which this command trusts as a released one does.
	decrypted := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
	if err := decrypted.Handshake(); err != nil {
		t.Fatalf("the TLS handshake with the proxy: %v", err)
	}
	t.Cleanup(func() { decrypted.Close() })
	request := "GET " + upgradePath + " HTTP/1.1\r\n" +
		"Host: " + boundHost + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Bearer " + workerPlaceholder + "\r\n\r\n"
	if _, err := decrypted.Write([]byte(request)); err != nil {
		t.Fatalf("write the upgrade request: %v", err)
	}
	reader := bufio.NewReader(decrypted)
	upgraded, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read the answer to the upgrade: %v", err)
	}
	return upgraded, decrypted, reader
}

// TestAWebSocketUpgradeIsSwappedAndThenJoined: an upgrade is a request like any other until the
// service answers 101 - so the value reaches the service in the header the entry names, and the
// Upgrade and Connection headers survive the swap - and after the 101 the two connections are
// one and the frames cross it both ways.
func TestAWebSocketUpgradeIsSwappedAndThenJoined(t *testing.T) {
	r := newRig(t, Options{})
	upgradeStandIn(t, r)

	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	if got := upgraded.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("the answer's Upgrade = %q; want websocket", got)
	}

	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want the upgrade", len(seen))
	}
	if got, want := seen[0].Header.Get("Authorization"), "Bearer "+workerValue; got != want {
		t.Fatalf("the service saw Authorization %q; want %q: the swap reaches an upgrade's own headers", got, want)
	}
	if got := seen[0].Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("the service saw Upgrade %q; want websocket: the header has to survive the rewrite", got)
	}
	if got := seen[0].Header.Get("Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
		t.Errorf("the service saw Connection %q; want it to name Upgrade", got)
	}

	if _, err := conn.Write([]byte("a frame from the command\n")); err != nil {
		t.Fatalf("write a frame to the joined connection: %v", err)
	}
	frame, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the service's frame: %v", err)
	}
	if want := "the service read a frame from the command\n"; frame != want {
		t.Fatalf("the service's frame is %q; want %q: the bytes pass both ways", frame, want)
	}
}

// TestTheIdleLimitEndsAQuietUpgradedConnection: the joined connection is the command's own, so
// the limit that ends a quiet tunnel ends a quiet upgrade too - a WebSocket nobody is speaking
// on is state the broker holds for nobody.
func TestTheIdleLimitEndsAQuietUpgradedConnection(t *testing.T) {
	const idle = 400 * time.Millisecond
	r := newRig(t, Options{Idle: idle})
	upgradeStandIn(t, r)

	began := time.Now()
	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	// The read ends when the proxy closes the joined connection, which is the end of the
	// stream and no error of its own; a connection the limit missed would sit here until the
	// deadline above and answer a timeout instead.
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the joined connection was not closed by the idle limit: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the joined connection carried %q after the upgrade; want nothing", rest)
	}
	if quiet := time.Since(began); quiet < idle {
		t.Fatalf("the joined connection closed after %v; want it to outlive the idle limit %v", quiet, idle)
	}
}

// watchFast is Options.Watch for the rows below: short enough that a row does not wait out the
// real interval, long enough that a handful of them are a handful of reads and not a busy loop.
const watchFast = 20 * time.Millisecond

// frameStand is the stand-in service the rows below pass frames to. airlocktest's own stand
// answers one frame and lets its connection go, which is the join ending by itself; a row about
// a grant that ends under an open join needs a service that keeps reading, and it needs to read
// back which frames reached that service, since the frame a closed connection did not carry is
// what says it was closed.
type frameStand struct {
	mu     sync.Mutex
	frames []string

	// done is closed when the answer below has returned, which is after the proxy's own
	// ServeHTTP returned and closed the connection to this service: what the stand holds
	// then is all it will ever hold, so a row reads the frames once this says so rather
	// than sleeping for a frame that may still be on its way.
	done     chan struct{}
	doneOnce sync.Once
}

// finish says the hijacked answer has returned. The once is there because one service may
// answer more than one upgrade in a row's life, and a channel is closed once.
func (f *frameStand) finish() {
	f.doneOnce.Do(func() { close(f.done) })
}

// waitDone waits for that answer to return, woken by the stand's own goroutine.
func (f *frameStand) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(cutWait):
		t.Fatalf("the service's answer held %q and had not returned; want it to have returned once the join was over", f.read())
	}
}

// Answer is airlocktest.Answer: 101 at upgradePath, and then one line back for each line in, as
// airlocktest's own stand answers its one.
func (f *frameStand) Answer(w http.ResponseWriter, _ *http.Request, seen airlocktest.Request) bool {
	if seen.Path != upgradePath {
		return false
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the stand-in service cannot be taken over", http.StatusInternalServerError)
		return true
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "the stand-in service could not hijack: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	// Registered before the close of the connection, so that it runs after it: a row woken
	// by this is woken by an answer that has let its connection go and read its last frame.
	defer f.finish()
	defer conn.Close()
	if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return true
	}
	if err := buffered.Flush(); err != nil {
		return true
	}
	for {
		line, err := buffered.ReadString('\n')
		if err != nil {
			return true
		}
		f.mu.Lock()
		f.frames = append(f.frames, strings.TrimSpace(line))
		f.mu.Unlock()
		if _, err := buffered.WriteString("the service read " + strings.TrimSpace(line) + "\n"); err != nil {
			return true
		}
		if err := buffered.Flush(); err != nil {
			return true
		}
	}
}

// read is every frame the service has read, as a copy of its own: a row reads them while the
// service is still being sent frames.
func (f *frameStand) read() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.frames)
}

// joinedThrough upgrades through the proxy to a frameStand of its own and answers that stand
// and the joined connection, with the 101 already read.
func joinedThrough(t *testing.T, r *rig) (*frameStand, net.Conn, *bufio.Reader) {
	t.Helper()
	stand := &frameStand{done: make(chan struct{})}
	r.answerWith(stand.Answer)
	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	return stand, conn, reader
}

// frame writes one frame on a joined connection and reads the service's answer to it, which is
// what says the join is carrying bytes.
func frame(t *testing.T, conn net.Conn, reader *bufio.Reader, line string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write %q to the joined connection: %v", line, err)
	}
	answer, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the service's answer to %q: %v", line, err)
	}
	if want := "the service read " + line + "\n"; answer != want {
		t.Fatalf("the service answered %q; want %q", answer, want)
	}
}

// waitClosed reads the joined connection until it ends, which is what the proxy's close looks
// like from the command's side. A connection the close missed sits here until the deadline and
// answers a timeout instead, which fails the row.
func waitClosed(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the joined connection was not closed: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the joined connection carried %q before it closed; want nothing", rest)
	}
}

// TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt: a joined connection is the one
// connection no later request is read on, so the read that stops every other call of a grant
// that has ended never runs for it, and the idle limit bounds nothing on a socket a service
// pings. The grant is read for the connection itself instead, and a grant that no longer
// carries it closes it: the frames stop, and a frame written after that reaches no service.
func TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt(t *testing.T) {
	// The grant as it reads once its operator has withheld the name this upgrade was decided
	// for, holding one other name so that the read answers a grant rather than refusing.
	withheld := testConnection()
	withheld.Names = map[string]requests.AirlockGrantName{
		"OTHER_TOKEN": {Slots: map[string]string{requests.SlotValue: requests.PlaceholderPrefix + "otherotherotherotherotherother01"}},
	}
	for _, row := range []struct {
		name   string
		end    func(f *fakeGrants)
		reason string
	}{
		{"the grant has ended", func(f *fakeGrants) {
			f.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
		}, requests.ErrGrantNotLive.Error()},
		{"every name of the grant is withheld", func(f *fakeGrants) {
			f.refuseConnection(workerPlaceholder, requests.ErrAirlockNoNames)
		}, requests.ErrAirlockNoNames.Error()},
		{"the name this upgrade was decided for is withheld", func(f *fakeGrants) {
			f.setConnection(workerPlaceholder, withheld)
		}, workerName + " is no longer a name of this grant"},
	} {
		t.Run(row.name, func(t *testing.T) {
			const carried = "a frame while the grant carries this connection"
			r := newRig(t, Options{Watch: watchFast})
			stand, conn, reader := joinedThrough(t, r)
			frame(t, conn, reader, carried)

			row.end(r.grants)
			waitClosed(t, conn, reader)
			// The frame the round-1 probe sent after the refusal, which the service read
			// then: a closed connection carries it nowhere.
			_, _ = conn.Write([]byte("a frame after the grant stopped carrying it\n"))
			time.Sleep(5 * watchFast)
			if frames := stand.read(); !slices.Equal(frames, []string{carried}) {
				t.Fatalf("the service read %q; want the one frame from before the grant stopped carrying the connection", frames)
			}

			lines := r.logs.joins()
			if len(lines) != 1 {
				t.Fatalf("the proxy wrote %d lines for the close; want the one", len(lines))
			}
			if got := lines[0].fields["reason"]; got != row.reason {
				t.Errorf("the close logged the reason %v; want %q", got, row.reason)
			}
			if got := lines[0].fields["grant"]; got != testGrantID {
				t.Errorf("the close logged the grant %v; want %s", got, testGrantID)
			}
			for key, value := range lines[0].fields {
				if text, ok := value.(string); ok && (strings.Contains(text, workerPlaceholder) || strings.Contains(text, workerValue)) {
					t.Errorf("the close logged %s = %q; want neither the placeholder nor the value", key, text)
				}
			}
		})
	}
}

// TestAJoinedConnectionIsClosedWhenTheCurrentPolicyRefusesItsGrant: the connection read says
// nothing about the policy - it checks the grant's liveness, its enrollment's lease and the
// names its operator withheld, and no more - so a grant the current policy refuses is found by
// the read of the values, the one every other request of that grant makes for itself. A joined
// connection makes it too, and is closed by what it answers: the frames stop, and a frame
// written after that reaches no service, where a new call of that grant is already answered 407.
func TestAJoinedConnectionIsClosedWhenTheCurrentPolicyRefusesItsGrant(t *testing.T) {
	// The refusal the release checks make for a name the policy no longer allows this session
	// (requests.stillAllowed): a grant that is not live, which ends no grant of its own, so the
	// connection read goes on answering this grant while the values read refuses it.
	refusal := fmt.Errorf("%w: the current policy no longer allows %s", requests.ErrGrantNotLive, workerName)
	const carried = "a frame while the policy allows this grant"
	r := newRig(t, Options{Watch: watchFast})
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, carried)

	// A policy change the broker took by its periodic reload: the version moves, so the values
	// this pod holds for the grant are read again rather than answered from, and that read is
	// what the current policy refuses.
	r.grants.refuseValues(refusal)
	r.grants.setPolicyVersion("policy-v2")
	waitClosed(t, conn, reader)
	_, _ = conn.Write([]byte("a frame after the policy refused the grant\n"))
	time.Sleep(5 * watchFast)
	if frames := stand.read(); !slices.Equal(frames, []string{carried}) {
		t.Fatalf("the service read %q; want the one frame from before the policy refused the grant", frames)
	}

	lines := r.logs.joins()
	if len(lines) != 1 {
		t.Fatalf("the proxy wrote %d lines for the close; want the one", len(lines))
	}
	if got := lines[0].fields["reason"]; got != refusal.Error() {
		t.Errorf("the close logged the reason %v; want %q, the read's own words", got, refusal)
	}
}

// TestAJoinedConnectionIsClosedAtItsGrantsExpiry: when a grant ends is a time the broker already
// holds, so a joined connection is closed at it without reading anything.
func TestAJoinedConnectionIsClosedAtItsGrantsExpiry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	expires := clock.read().Add(time.Hour)
	bounded := testConnection()
	bounded.Expires = &expires
	r := newRig(t, Options{Now: clock.read, Watch: watchFast})
	r.grants.setConnection(workerPlaceholder, bounded)
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, "a frame before the grant expires")

	clock.set(expires)
	waitClosed(t, conn, reader)
	if frames := stand.read(); len(frames) != 1 {
		t.Fatalf("the service read %q; want the one frame from before the expiry", frames)
	}
}

// TestAGrantReadThatFailedLeavesAJoinedConnectionAlone: a read that did not happen says nothing
// about the grant, so the connection stays open and the error goes to the log. A store down for
// a minute cutting every WebSocket the airlock carries would be an outage of its own.
func TestAGrantReadThatFailedLeavesAJoinedConnectionAlone(t *testing.T) {
	r := newRig(t, Options{Watch: watchFast})
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, "a frame while the store answers")

	before := r.grants.connectionReads()
	r.grants.refuseConnection(workerPlaceholder, databaseDown)
	// Three reads that failed, so that what this row reads back is what the watcher did with
	// them rather than a watcher that has not run yet.
	deadline := time.Now().Add(10 * time.Second)
	for r.grants.connectionReads() < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("the grant was read %d times in ten seconds; want three more than the %d before the store went down", r.grants.connectionReads(), before)
		}
		time.Sleep(watchFast)
	}
	frame(t, conn, reader, "a frame while the store is down")

	if frames := stand.read(); len(frames) != 2 {
		t.Fatalf("the service read %q; want both frames: a read that failed closes no connection", frames)
	}
	if lines := r.logs.joins(); len(lines) != 0 {
		t.Fatalf("the proxy closed the joined connection and logged %d lines for it; want none", len(lines))
	}
}

// TestAJoinedConnectionSurvivesAMintWhoseUpstreamCouldNotBeAsked: the values read of a grant
// holding a minted name re-mints it, and a minter that could not ask its upstream - no answer,
// a 5xx, a rate limit - refuses with ErrMintUpstream inside an ErrMintFailed. That is a
// refusal of the requests package and no answer about this grant: the grant is live and the
// command's placeholder still opens it. GCLOUD_SAMI, AZURE_ADMIN and GWS_THEOREM_ADMIN_OAUTH
// are minted names this deployment puts behind the airlock, so a bad minute of Google's or
// GitHub's would otherwise cut every WebSocket the airlock carries for them.
func TestAJoinedConnectionSurvivesAMintWhoseUpstreamCouldNotBeAsked(t *testing.T) {
	upstream := fmt.Errorf("%w: %w: github app 42: installation 7: 502 Bad Gateway", requests.ErrMintFailed, secrets.ErrMintUpstream)
	r := newRig(t, Options{Watch: watchFast})
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, "a frame while the minter answers")

	// The version moves, so the values this pod holds are read again rather than answered
	// from, and that read is the one the minter's upstream refuses.
	before := r.grants.valueReads(testGrantID)
	r.grants.refuseValues(upstream)
	r.grants.setPolicyVersion("policy-v2")
	// Three reads the upstream refused, so that what this row reads back is what the watcher
	// did with them rather than a watcher that has not run yet.
	deadline := time.Now().Add(10 * time.Second)
	for r.grants.valueReads(testGrantID) < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("the grant's values were read %d times in ten seconds; want three more than the %d before the upstream went down",
				r.grants.valueReads(testGrantID), before)
		}
		time.Sleep(watchFast)
	}
	frame(t, conn, reader, "a frame while the minter's upstream is down")

	if frames := stand.read(); len(frames) != 2 {
		t.Fatalf("the service read %q; want both frames: a mint whose upstream could not be asked closes no connection", frames)
	}
	if lines := r.logs.joins(); len(lines) != 0 {
		t.Fatalf("the proxy closed the joined connection and logged %d lines for it; want none", len(lines))
	}
}

// cutWait bounds every wait the row below makes on an event it expects. It is no part of what
// the row proves: each of those events is one another goroutine makes happen, so the deadline
// is there to fail the row with what the service does hold rather than to wait an event out.
const cutWait = 5 * time.Second

// blockedClose is the socket under a joined connection's TLS, with a Close that waits to be
// released and nothing else of it changed. tls.Conn.Close writes close_notify and closes the
// socket under it only after that write, so a Close held here holds open the exact window
// between the two, which is the window the row below is about.
type blockedClose struct {
	net.Conn
	release chan struct{}
}

func (c *blockedClose) Close() error {
	<-c.release
	return c.Conn.Close()
}

// heldConn is that socket with two more things of the row's own, for the one row that holds
// the window open over the whole proxy rather than over a join of its own. It composes the
// held close rather than repeating it: the connection the proxy's listener accepted, held
// shut, watched, and under everything the proxy then builds on it - the hijack, the decrypted
// connection's TLS, the cut and the join's own copy.
//
// SetReadDeadline does nothing once the row has armed it, and passes through before that.
// That is the poller's own answer made certain rather than waited for: the read the cut has
// to stop is parked in the poller, and a deadline in the past does not end a read whose socket
// became readable before the deadline was set, which is what happens about one run in two
// hundred on a loaded machine. Armed, it is what happens in every run.
//
// Read says when a read of the connection under it is in flight and when it has returned, each
// change signalled, so the row waits on those two events rather than sleeping for them. The
// counts are monotone, which is what lets a wait be for one named read rather than for a
// glance at a state the next read may already have left behind.
type heldConn struct {
	net.Conn
	armed atomic.Bool

	mu       sync.Mutex
	started  int
	returned int
	changes  chan struct{}
}

// arm makes every read deadline set from here on do nothing.
func (c *heldConn) arm() { c.armed.Store(true) }

func (c *heldConn) SetReadDeadline(deadline time.Time) error {
	if c.armed.Load() {
		return nil
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *heldConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	c.started++
	c.mu.Unlock()
	c.signal()
	n, err := c.Conn.Read(b)
	c.mu.Lock()
	c.returned++
	c.mu.Unlock()
	c.signal()
	return n, err
}

// signal wakes whatever waits on the reads of this socket. A signal nobody is waiting for is
// dropped, as copySink's is: the waiters below read the counts before they wait at all, and a
// drop happens only while signals are already queued, each of which wakes that read again.
func (c *heldConn) signal() {
	select {
	case c.changes <- struct{}{}:
	default:
	}
}

// waitReading waits until a read of the socket is in flight - the join's copy of the command's
// side, parked with nothing to read, which is the one read the cut has to stop - and answers
// which read that is.
func (c *heldConn) waitReading(t *testing.T) int {
	t.Helper()
	deadline := time.After(cutWait)
	for {
		c.mu.Lock()
		started, returned := c.started, c.returned
		c.mu.Unlock()
		if started > returned {
			return started
		}
		select {
		case <-c.changes:
		case <-deadline:
			t.Fatalf("%d reads of the socket started and %d returned; want one of them still in flight", started, returned)
		}
	}
}

// waitReturned waits for the read waitReading answered to return. It waits for that one read
// rather than for a moment with none in flight because a proxy that carried the late frame
// starts its next read of the socket at once: a waiter that read the counts after that would
// be waiting for a moment that has already passed, and a row must not turn on which goroutine
// the scheduler ran first.
func (c *heldConn) waitReturned(t *testing.T, read int) {
	t.Helper()
	deadline := time.After(cutWait)
	for {
		c.mu.Lock()
		returned := c.returned
		c.mu.Unlock()
		if returned >= read {
			return
		}
		select {
		case <-c.changes:
		case <-deadline:
			t.Fatalf("%d reads of the socket returned; want read %d, the one in flight when the cut was decided, to have returned", returned, read)
		}
	}
}

// copySink stands for the service at the far end of a join: it holds every byte the copy from
// the command carried to it, and signals each write, so a row waits for a frame to arrive
// rather than sleeping for the time one takes.
type copySink struct {
	mu      sync.Mutex
	carried []byte
	writes  chan struct{}
}

func (s *copySink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.carried = append(s.carried, p...)
	s.mu.Unlock()
	// A signal nobody is waiting for is dropped, since the waiter below reads what the
	// service holds before it waits at all: a drop happens only while signals are already
	// queued, and each of those wakes that read again.
	select {
	case s.writes <- struct{}{}:
	default:
	}
	return len(p), nil
}

// read is every byte the service has been carried so far, which a row reads while the copy to
// it is still running.
func (s *copySink) read() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.carried)
}

// waitFor waits until the service holds want, woken by the copy's own writes.
func (s *copySink) waitFor(t *testing.T, want, when string) {
	t.Helper()
	deadline := time.After(cutWait)
	for {
		if strings.Contains(s.read(), want) {
			return
		}
		select {
		case <-s.writes:
		case <-deadline:
			t.Fatalf("%s the service read %q; want it to hold %q", when, s.read(), want)
		}
	}
}

// joinedStandIn is one joined connection and nothing else of the proxy: the command's side,
// the side the proxy holds and closes, the copy to the service that httputil.ReverseProxy
// runs for a join, and the socket's own Close held until the row lets it go. The rows above
// drive the whole proxy because what they prove is what it decides; this one is about what a
// close does to the bytes in flight under it, which is decided by those two halves alone.
type joinedStandIn struct {
	command *tls.Conn     // the command's side, which writes the frames
	joined  *tls.Conn     // the side watchJoined closes
	cut     *cutConn      // the connection under that side's TLS, which the cut stops
	sink    *copySink     // the service the join copies the command's side to
	copied  chan struct{} // closed when that copy returns
	release func()        // lets the socket under joined close
}

// newJoinedStandIn builds that connection over a real TCP socket, since the frame written in
// the window has to sit somewhere the kernel holds it while the close is in the middle of it,
// which an in-memory pipe has nowhere to do.
func newJoinedStandIn(t *testing.T) *joinedStandIn {
	t.Helper()
	authority := newTestAuthority(t)
	certificate, err := authority.leaf(boundHost)
	if err != nil {
		t.Fatalf("the joined connection's certificate: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the joined connection: %v", err)
	}
	defer func() { _ = listener.Close() }()

	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	// A row that fails inside the window would otherwise leave the close waiting for a
	// release nobody writes.
	t.Cleanup(release)

	type served struct {
		conn *tls.Conn
		cut  *cutConn
		err  error
	}
	serving := make(chan served, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serving <- served{err: err}
			return
		}
		cut := &cutConn{Conn: &blockedClose{Conn: raw, release: released}}
		joined := tls.Server(cut, &tls.Config{Certificates: []tls.Certificate{*certificate}})
		if err := joined.Handshake(); err != nil {
			serving <- served{err: err}
			return
		}
		serving <- served{conn: joined, cut: cut}
	}()

	trust := x509.NewCertPool()
	trust.AddCert(authority.Certificate())
	command, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: trust, ServerName: boundHost})
	if err != nil {
		t.Fatalf("dial the joined connection: %v", err)
	}
	t.Cleanup(func() { _ = command.Close() })
	answered := <-serving
	if answered.err != nil {
		t.Fatalf("the side the proxy holds: %v", answered.err)
	}

	join := &joinedStandIn{
		command: command,
		joined:  answered.conn,
		cut:     answered.cut,
		sink:    &copySink{writes: make(chan struct{}, 8)},
		copied:  make(chan struct{}),
		release: release,
	}
	go func() {
		defer close(join.copied)
		_, _ = io.Copy(join.sink, join.joined)
	}()
	return join
}

// waitCopied waits for the copy to the service to return. After the cut that return is the
// row's own event, since the cut is what ends the copy and nothing is timed to make it
// happen; at the end of either half it says the copy is over, so what the service holds then
// is all it will ever hold.
func (j *joinedStandIn) waitCopied(t *testing.T, when string) {
	t.Helper()
	select {
	case <-j.copied:
	case <-time.After(cutWait):
		t.Fatalf("the copy to the service did not return %s", when)
	}
}

// readEnd reads the command's side to the end of the stream, which is the close_notify the
// close wrote: the command is told the stream has ended, and the socket under it is still
// open while it is told, which is the window itself.
func (j *joinedStandIn) readEnd(t *testing.T) {
	t.Helper()
	if err := j.command.SetReadDeadline(time.Now().Add(cutWait)); err != nil {
		t.Fatalf("set the command's read deadline: %v", err)
	}
	rest, err := io.ReadAll(j.command)
	if err != nil {
		t.Fatalf("read the command's side to the end of the stream: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the command read %q before the end of the stream; want nothing", rest)
	}
}

// TestTheCloseOfAJoinedConnectionCutsTheCommandsSideFirst: the connection the close ends is a
// *tls.Conn, and tls.Conn.Close writes close_notify first and closes the socket under it only
// after that write, which crypto/tls gives five seconds of its own. The command reads that
// close_notify as the end of the stream while the socket is still open and the join's copy of
// the command's side is still reading it, so a command that answers the end of a stream with
// one more frame - which is what a framed protocol does - has that frame carried to the
// service after the airlock decided the grant no longer carries the connection. cutJoined
// ends the reads of the command's side before the close, which shuts that window.
//
// The plain close is the row's negative control, and it belongs in it because it is what says
// the window is real: the late frame crosses it, so the cut above is what shut it rather than
// a window that had closed itself. The socket's own Close waits for the row to let it go, so
// the window is held open rather than raced for, and every wait is on an event another
// goroutine makes happen rather than on a sleep.
func TestTheCloseOfAJoinedConnectionCutsTheCommandsSideFirst(t *testing.T) {
	const (
		carried = "a frame while the grant carries this connection\n"
		late    = "a frame after the grant stopped carrying it\n"
	)
	for _, row := range []struct {
		name  string
		close func(*cutConn, net.Conn)
		// crosses is whether the late frame reaches the service, which is what the
		// control shows and what the cut must not.
		crosses bool
	}{
		{"the cut before the close carries no frame written after it", cutJoined, false},
		{"a plain close carries one, which is the window the cut shuts", func(_ *cutConn, c net.Conn) { _ = c.Close() }, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			join := newJoinedStandIn(t)
			if _, err := join.command.Write([]byte(carried)); err != nil {
				t.Fatalf("write the frame from before the close: %v", err)
			}
			join.sink.waitFor(t, carried, "before the close")

			// In a goroutine because the socket under the close is held: the close sits
			// in the window this row is about until the release below.
			go row.close(join.cut, join.joined)
			if row.crosses {
				join.readEnd(t)
			} else {
				join.waitCopied(t, "after the cut")
			}
			if _, err := join.command.Write([]byte(late)); err != nil {
				t.Fatalf("write the late frame: %v", err)
			}
			if row.crosses {
				join.sink.waitFor(t, late, "while the socket under the close was still open")
			}

			join.release()
			join.waitCopied(t, "after the socket under the close was let go")
			want := carried
			if row.crosses {
				want = carried + late
			}
			if got := join.sink.read(); got != want {
				t.Fatalf("the service read %q; want %q", got, want)
			}
		})
	}
}

// TestAJoinedConnectionIsClosedCarryingNoFrameWrittenAfterTheCut holds the cut's own window
// open over the whole proxy: serveBound, the hijack, and the join httputil.ReverseProxy runs,
// which is where the frame that must not cross would cross. The three rows above drive the
// same proxy but let the close finish, so the window between the close_notify the command
// reads as the end of the stream and the close of the socket under it is never held open:
// they read that the frames stop, which a connection whose socket has already gone also
// reads. The row below holds that socket open with a Close of its own, and makes the read
// deadline cutJoined sets the nothing it is for a read the poller has already found readable,
// so that what is left is the defect itself - one read of the command's side in flight when
// the grant stopped carrying the connection, and one frame written after the command was told
// the stream had ended. The cut decides that frame is carried nowhere, and the service reads
// only what crossed before it.
//
// Every wait here is for something another goroutine makes happen: a read parked on the
// socket, that read returning, the close_notify arriving, the call's line being written, the
// service's answer returning. Nothing is waited out.
func TestAJoinedConnectionIsClosedCarryingNoFrameWrittenAfterTheCut(t *testing.T) {
	const (
		carried = "a frame while the grant carries this connection"
		late    = "a frame after the grant stopped carrying it"
	)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }

	held := make(chan *heldConn, 1)
	r := newRig(t, Options{Watch: watchFast}, func(c net.Conn) net.Conn {
		socket := &heldConn{
			Conn:    &blockedClose{Conn: c, release: released},
			changes: make(chan struct{}, 8),
		}
		// The send cannot wait, since it runs inside the listener's Accept: the row
		// opens the one connection and takes the first, and a second would be a
		// connection this row never meant.
		select {
		case held <- socket:
		default:
		}
		return socket
	})
	// After newRig and never before it: cleanups run last registered first, and newRig
	// registers the proxy's own drain. A release registered before that drain would run
	// after it, and the drain closes the connections it holds - so a row that failed inside
	// the window would sit in that close with nobody left to let it go.
	t.Cleanup(release)

	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, carried)
	socket := <-held

	// The join's copy of the command's side, parked in a read of the socket with nothing to
	// read: the one read that is in flight when the cut is decided.
	reading := socket.waitReading(t)
	socket.arm()

	r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
	// The watcher refused on its next tick and cut: this is the close_notify the cut wrote,
	// read as the end of the stream while the socket under it is still open, which is the
	// window itself.
	waitClosed(t, conn, reader)
	if _, err := conn.Write([]byte(late + "\n")); err != nil {
		t.Fatalf("write the frame after the cut: %v", err)
	}
	// Nothing else can wake the parked read - the deadline does nothing and the socket is
	// held open - so what wakes it is that frame, and its return is the proxy having read
	// the frame and decided what to do with it.
	socket.waitReturned(t, reading)
	release()

	// The proxy writes a call's line after proxy.ServeHTTP has returned, so the join is over
	// and nothing more can cross it, and the service's own answer has returned with it: what
	// the service holds now is all it will ever hold.
	r.logs.callsAtLeast(t, 1)
	stand.waitDone(t)
	if frames := stand.read(); !slices.Equal(frames, []string{carried}) {
		t.Fatalf("the service read %q; want only %q, the frame from before the grant stopped carrying the connection", frames, []string{carried})
	}
}
