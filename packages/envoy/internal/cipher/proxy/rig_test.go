// packages/envoy/internal/cipher/proxy/rig_test.go
//
// The world a row runs in, and nothing that asserts anything about it: the proxy on a loopback
// listener, the fake GrantSource behind it, the stand-in service behind the bound host, the two
// tunnelled services, the certificate authority the rows sign with, and the log they read back.
// proxy_test.go, aws_test.go, azure_test.go, upgrade_test.go and h2_test.go each build one with
// newRig, and ca_test.go takes the authority helpers alone. Why the rig is built out of real
// listeners and real TLS rather than mocks is in proxy_test.go's own comment.
//
// TestMain runs the whole suite twice: once with every rig's client speaking HTTP/1.1 to a bound
// host, and once speaking HTTP/2 alone, as a gRPC client does (rig.protocol).
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The grant every row but the two-name one is served by: one secret, bound to one host by its
// kind, whose value may travel in Authorization alone and only on /v1/.
const (
	boundHost    = "example.com"
	tunnelHost   = "tunnel.example.net"
	echoHost     = "echo.example.net"
	workerName   = "WORKER_TOKEN"
	workerValue  = "ghp-the-real-worker-token"
	testGrantID  = "grant-00000000"
	testEnrollID = "enrollment-00000000"
	testRequest  = "request-00000000"
	testSession  = "session-00000000"
	testRuntime  = "runtime-00000000"
	testIssue    = "INFRA-1352"
)

// workerPlaceholder is a placeholder in the shape the broker mints them, so a row that asserts a
// body never names the placeholder is asserting about the real thing.
var workerPlaceholder = placeholder.Prefix + "AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPp"

// The two protocols a command's client may speak to a bound host, by their ALPN names:
// HTTP/1.1, which every client that offers it is answered in, and HTTP/2, which a client that
// offers nothing else - every gRPC client - is answered in (connectBound).
const (
	protocolHTTP1 = "http/1.1"
	protocolH2    = "h2"
)

// suiteProtocol is the protocol every rig's client speaks unless its row names one: TestMain sets
// it before each of its two runs of the suite.
var suiteProtocol = protocolHTTP1

// TestMain runs every row of this package under each protocol a client may speak to a bound host,
// so each row whose call goes through r.client is a row of HTTP/1.1 and a row of HTTP/2: the swap,
// the refusals, the AWS signature, the Azure exchange and the lines and counts are the same
// request handling whichever framing carried the request. A row that speaks to the proxy on a
// socket of its own - a CONNECT written by hand, an upgrade, a frame written by hand - runs the
// same bytes in both runs. The banner before each run names the protocol of the run below it.
// Both runs go through storetest.Run, which drops the test binary's own database the rows'
// schemas are made in once both have ended.
func TestMain(m *testing.M) {
	os.Exit(storetest.Run(func() int {
		code := 0
		for _, protocol := range []string{protocolHTTP1, protocolH2} {
			suiteProtocol = protocol
			fmt.Printf("proxy: every rig's client speaks %s to a bound host in the run below\n", protocol)
			if run := m.Run(); run != 0 {
				code = run
			}
		}
		return code
	}))
}

// workerKind is the kind of the profiles file the rig's one name is served behind: its proxy tag
// names it.
const workerKind = "worker"

// testRules is the airlock's rules every row but the two-name one runs under: one kind on the
// bound host, whose credential travels in Authorization on GET and POST under /v1/, and the rig's
// one name bound to it, a stored value swapped in with no allow of a profile's own.
func testRules() proxyrules.Rules {
	return proxyrules.Rules{
		Kinds: []proxyrules.Kind{{
			Name:    workerKind,
			Hosts:   []string{boundHost},
			Headers: []string{"authorization"},
			Ways:    []string{proxyrules.WaySwap},
			Allow:   []proxyrules.Allow{{Method: http.MethodGet, PathPrefix: "/v1/"}, {Method: http.MethodPost, PathPrefix: "/v1/"}},
		}},
		Names: []proxyrules.Binding{{Name: workerName, Kind: workerKind, Way: proxyrules.WaySwap}},
	}
}

// swapKind is a kind of its own for a row's name: the hosts and headers it names, every method and
// path, and a stored value swapped in. A row with two names on one host gives each a kind of its
// own, since the headers a credential travels in are its kind's.
func swapKind(name string, hosts []string, headers ...string) proxyrules.Kind {
	return proxyrules.Kind{Name: name, Hosts: hosts, Headers: headers, Ways: []string{proxyrules.WaySwap}}
}

// swapBinding binds name to kind as a stored value swapped in, with no allow of a profile's own.
func swapBinding(name, kind string) proxyrules.Binding {
	return proxyrules.Binding{Name: name, Kind: kind, Way: proxyrules.WaySwap}
}

// twoNameRules is the airlock's rules of a grant of two stored values on the one bound host: alpha
// travels in Authorization under the kind alpha, and beta in X-Beta under the kind beta, each kind
// allowing every method and path, so a row reads which name a request was handled for by the
// header its value went into.
func twoNameRules(alpha, beta string) proxyrules.Rules {
	return proxyrules.Rules{
		Kinds: []proxyrules.Kind{swapKind("alpha", []string{boundHost}, "authorization"), swapKind("beta", []string{boundHost}, "x-beta")},
		Names: []proxyrules.Binding{swapBinding(alpha, "alpha"), swapBinding(beta, "beta")},
	}
}

// testConnection is what the fake answers for the proxy's password: one live grant holding the
// one name above.
func testConnection() requests.AirlockConnection {
	return requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		SessionID: testSession, RuntimeID: testRuntime,
		Names: map[string]requests.AirlockGrantName{
			workerName: {Slots: map[string]string{requests.SlotValue: workerPlaceholder}},
		},
	}
}

// storedValues is one grant's values as the fake answers them: one name, one value slot, no
// expiry, which is what a stored secret's release looks like.
func storedValues(name, value string) map[string]requests.AirlockSecret {
	return map[string]requests.AirlockSecret{name: {Slots: map[string]string{requests.SlotValue: value}}}
}

// The github-app shape of the rig's one name, for the rows about a slot the broker stops
// releasing: one token per installation, acme's and zed's, and the worker placeholder is acme's.
const (
	githubAcmeSlot  = requests.SlotGitHubPrefix + "acme"
	githubZedSlot   = requests.SlotGitHubPrefix + "zed"
	githubZedHolder = placeholder.Prefix + "zedzedzedzedzedzedzedzedzedzed02"
	githubAcmeToken = "ghs_the-real-acme-installation-token"
	githubZedToken  = "ghs_the-real-zed-installation-token"
)

// useGitHubAppGrant makes the rig's one name a github-app name holding acme's and zed's tokens,
// with the worker placeholder as acme's, so a call that carries it is handled for (workerName,
// acme).
func (r *rig) useGitHubAppGrant() {
	conn := testConnection()
	conn.Names = map[string]requests.AirlockGrantName{
		workerName: {Type: "github-app", Slots: map[string]string{githubAcmeSlot: workerPlaceholder, githubZedSlot: githubZedHolder}},
	}
	r.grants.setConnection(workerPlaceholder, conn)
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Type: "github-app", Slots: map[string]string{githubAcmeSlot: githubAcmeToken, githubZedSlot: githubZedToken}},
	})
}

// removeAcmeInstallation is the broker's read of that name once acme's installation has gone:
// zed's token alone, and the name's reread, which drops what this pod holds of it (DropName). The
// grant still holds acme's placeholder, as a release made before the installation went does.
func (r *rig) removeAcmeInstallation() {
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Type: "github-app", Slots: map[string]string{githubZedSlot: githubZedToken}},
	})
	r.proxy.DropName(workerName)
}

// noAcmeValue is the words a call for acme's slot is refused or ended with once the broker no
// longer releases it.
const noAcmeValue = "the broker released no " + githubAcmeSlot + " value for " + workerName

// databaseDown is a grant read that failed in the shape the store fails in: pgx puts the
// database's user, its name and its address in the text of a connect error, which is why a row
// about such a failure asserts that no answer of this port carries a word of it.
var databaseDown = errors.New("failed to connect to `user=broker database=broker`: 10.20.0.7:5432 (db.internal): dial error: timeout")

// fakeGrants is the proxy's GrantSource: the reads of requests.Machine, answered from maps a
// row writes, and a count of how often the values were read.
type fakeGrants struct {
	mu          sync.Mutex
	connections map[string]requests.AirlockConnection
	refusals    map[string]error
	values      map[string]map[string]requests.AirlockSecret
	valuesErr   error
	valueCalls  map[string]int
	connCalls   int
	// version is the policy version the broker holds now, which every read answers as the one
	// it ran under. A row moves it to play a policy change the broker took by its periodic
	// reload, which reaches a pod's held values no other way.
	version string
	// access is the placeholder each (grant, name, slot) of an exchange was minted, as the
	// broker's own table holds it: minted once and answered again after.
	access      map[string]string
	accessCalls int
	// rules is the airlock's rules the fake answers (AirlockRules), as the broker's live policy
	// holds them: testRules unless the row's rig was built with others (newRuledRig), or the row
	// changes them (setRules) to play a retag the broker read.
	rules proxyrules.Rules
}

func newFakeGrants() *fakeGrants {
	return &fakeGrants{
		connections: map[string]requests.AirlockConnection{workerPlaceholder: testConnection()},
		refusals:    map[string]error{},
		values:      map[string]map[string]requests.AirlockSecret{testGrantID: storedValues(workerName, workerValue)},
		valueCalls:  map[string]int{},
		version:     "policy-v1",
		access:      map[string]string{},
		rules:       testRules(),
	}
}

// AirlockRules is the rules the fake holds now, read under the lock as every other read is, since
// a row can change them while a connection's request reads them.
func (f *fakeGrants) AirlockRules() proxyrules.Rules {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules
}

// setRules replaces the rules every later request and CONNECT reads.
func (f *fakeGrants) setRules(rules proxyrules.Rules) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = rules
}

func (f *fakeGrants) AirlockConnection(_ context.Context, placeholder string) (requests.AirlockConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connCalls++
	if err, ok := f.refusals[placeholder]; ok {
		return requests.AirlockConnection{}, err
	}
	conn, ok := f.connections[placeholder]
	if !ok {
		return requests.AirlockConnection{}, requests.ErrGrantNotLive
	}
	return conn, nil
}

func (f *fakeGrants) AirlockValues(_ context.Context, grantID string) (map[string]requests.AirlockSecret, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCalls[grantID]++
	if f.valuesErr != nil {
		return nil, "", f.valuesErr
	}
	// A copy, as the broker's own read answers a map of its own: a row that changes what the
	// next read answers must not change the map a held answer is still being read from.
	held := map[string]requests.AirlockSecret{}
	maps.Copy(held, f.values[grantID])
	return held, f.version, nil
}

func (f *fakeGrants) PolicyVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version
}

// setPolicyVersion is a policy change the broker took: the version every later read answers,
// and the one the proxy compares what it holds against.
func (f *fakeGrants) setPolicyVersion(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
}

// setValues replaces what the next read of grantID answers.
func (f *fakeGrants) setValues(grantID string, values map[string]requests.AirlockSecret) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[grantID] = values
}

// refuseValues is what the release checks answer for every grant from here on: the refusal a
// grant the current policy no longer allows gets, among the others. It is taken under the lock
// because a row that writes it while a joined connection's watcher is reading would otherwise
// be a race of its own.
func (f *fakeGrants) refuseValues(why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valuesErr = why
}

func (f *fakeGrants) valueReads(grantID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valueCalls[grantID]
}

// refuseConnection is what the broker answers for a placeholder from here on: the refusal a
// grant that has ended, been revoked or had its enrollment's lease lapse gets.
func (f *fakeGrants) refuseConnection(placeholder string, why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals[placeholder] = why
}

// setConnection replaces the grant a placeholder opens from here on, which is how a row withholds
// one name of a grant and leaves the rest.
func (f *fakeGrants) setConnection(placeholder string, conn requests.AirlockConnection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connections[placeholder] = conn
}

// connectionReads is how often a password has been read as a grant: once per CONNECT and once
// per request on a connection the proxy decrypted.
func (f *fakeGrants) connectionReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connCalls
}

// AirlockAccessPlaceholder is the broker's mint of one access token's placeholder: the same
// string for one (grant, name, slot) however often it is asked for, and written into the
// grant's names, as the broker's own row is, so a connection opened after it carries it and one
// opened before does not.
func (f *fakeGrants) AirlockAccessPlaceholder(_ context.Context, grantID, name, slot string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessCalls++
	key := grantID + "\x00" + name + "\x00" + slot
	mint, minted := f.access[key]
	if !minted {
		mint = fmt.Sprintf("%saccessToken%021d", placeholder.Prefix, len(f.access))
		f.access[key] = mint
	}
	for password, conn := range f.connections {
		held, carries := conn.Names[name]
		if conn.GrantID != grantID || !carries {
			continue
		}
		// Copied rather than written to: a connection already open is reading the map it was
		// handed, and the row this mints reaches it only on its next connection.
		slots := maps.Clone(held.Slots)
		slots[slot] = mint
		names := maps.Clone(conn.Names)
		names[name] = requests.AirlockGrantName{Slots: slots, Type: held.Type}
		conn.Names = names
		f.connections[password] = conn
	}
	return mint, nil
}

// accessMints is how many exchanges asked the broker for a placeholder.
func (f *fakeGrants) accessMints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessCalls
}

// seenRequest is one request the stand-in service received, as the rows read it: the record the
// stand-in package keeps, since the service behind the bound host is that package's.
type seenRequest = proxyrulestest.Request

// logLine is one line the proxy logged, as the rows read it.
type logLine struct {
	message string
	level   slog.Level
	fields  map[string]any
}

// logCapture is the slog handler the rows install: logtest.Recorder keeps every record with the
// attributes the proxy wrote it with, and the methods here are this package's own reading of
// those records. Each record is also written as the broker writes it - the broker's JSON handler
// (logging.NewCloudHandler) through its placeholder mask (logmask.Writer) - into written, the
// bytes a row that asks what reaches the broker's log reads (writtenLines).
type logCapture struct {
	logtest.Recorder
	written *logtest.Buffer
	broker  slog.Handler
}

// newLogCapture is a logCapture with nothing written yet.
func newLogCapture() *logCapture {
	written := &logtest.Buffer{}
	return &logCapture{written: written, broker: logging.NewCloudHandler(logmask.Writer(written))}
}

// Handle writes the line the broker writes before it keeps the record: a row waits on the records
// (linesAtLeast) and then reads written, and a record kept first could be seen by that wait while
// the proxy's goroutine, held off the CPU on a loaded runner, had not yet written its line.
func (c *logCapture) Handle(ctx context.Context, record slog.Record) error {
	if err := c.broker.Handle(ctx, record); err != nil {
		return err
	}
	return c.Recorder.Handle(ctx, record)
}

// The proxy logs with no attribute or group of a logger's own, and Recorder keeps none.
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// writtenLines is every line written so far as the broker writes it, each decoded as the JSON
// object it is.
func (c *logCapture) writtenLines(t *testing.T) []map[string]any {
	t.Helper()
	return logtest.JSONLines(t, c.written.String())
}

// calls is every line one call to a bound host logged that the airlock carried or failed. Records
// answers a slice of its own, so a row reads the lines written so far while the proxy goes on
// writing.
func (c *logCapture) calls() []logLine { return c.lines(callMessage) }

// refusals is every refusal record the proxy wrote (refuse), read the same way: a refused call to
// a bound host writes one in place of its call line, and a refusal of the outer server writes
// one too. refuse writes it before the answer, so a row that has its answer reads it at once.
func (c *logCapture) refusals() []logLine { return c.lines(answer.RefusalMessage) }

// joins is every line the close of a joined connection logged, read the same way.
func (c *logCapture) joins() []logLine { return c.lines(joinMessage) }

// cuts is every line the end of a carried call its grant stopped carrying logged, read the same
// way.
func (c *logCapture) cuts() []logLine { return c.lines(cutMessage) }

// tunnels is every line one socket the proxy opened for a tunnel or a forwarded plain-HTTP
// request logged, and dialFailures every line one dial it could not make logged, read the same
// way.
func (c *logCapture) tunnels() []logLine { return c.lines(tunnelMessage) }

func (c *logCapture) dialFailures() []logLine { return c.lines(dialFailedMessage) }

// tunnelsAtLeast and dialFailuresAtLeast are those lines once there are n of them, as
// callsAtLeast is for a call's. The proxy writes either line before the command is answered, so
// a row that has its answer could read them at once; a row whose command went away has no answer
// to read, and a row that drives several requests reads all their lines at the end, so these
// wait.
func (c *logCapture) tunnelsAtLeast(t *testing.T, n int) []logLine {
	t.Helper()
	return c.linesAtLeast(t, tunnelMessage, n)
}

func (c *logCapture) dialFailuresAtLeast(t *testing.T, n int) []logLine {
	t.Helper()
	return c.linesAtLeast(t, dialFailedMessage, n)
}

// linesAtLeast waits for n lines of one message, and fails t rather than hanging when they never
// come.
func (c *logCapture) linesAtLeast(t *testing.T, message string, n int) []logLine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := c.lines(message)
		if len(lines) >= n {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy wrote %d %q lines; want at least %d", len(lines), message, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// lines is every line the proxy wrote under one message, with the attributes it wrote them with.
func (c *logCapture) lines(message string) []logLine {
	var lines []logLine
	for _, record := range c.Records() {
		if record.Message != message {
			continue
		}
		fields := map[string]any{}
		record.Attrs(func(a slog.Attr) bool {
			fields[a.Key] = a.Value.Any()
			return true
		})
		lines = append(lines, logLine{message: record.Message, level: record.Level, fields: fields})
	}
	return lines
}

// callsAtLeast is the same, once there are n of them. The proxy writes a call's line after its
// answer has been copied to the client, so a client whose own read of the body has returned can
// still be ahead of that write by a few instructions: a row that reads calls()[0] straight after
// its request passes on an idle machine and panics on a loaded one, which is how this wait came
// to be written. It fails the row rather than hanging when the line never comes.
func (c *logCapture) callsAtLeast(t *testing.T, n int) []logLine {
	t.Helper()
	return c.linesAtLeast(t, callMessage, n)
}

// rig is one row's whole world: the proxy, what it dials, what it may resolve, the stand-in
// service behind the bound host, two tunnel services, and the log.
type rig struct {
	t         *testing.T
	proxy     *Server
	addr      string
	grants    *fakeGrants
	logs      *logCapture
	authority *Authority

	// trust is what the command's own client trusts: the proxy's authority, so a bound host
	// verifies, and the service certificates of the two tunnel hosts.
	trust *x509.CertPool
	// addresses is what resolve answers, which is how a row says a name is public or private.
	addresses map[string][]netip.Addr
	// dialTo is where the dial seam sends each authority, by the name a row wrote it under.
	// The proxy dials the address it checked rather than the name (address.go), so the seam
	// reads the name back out of addresses: a row names its services and the rig does the rest.
	dialTo map[string]string
	dials  atomic.Int64
	// answers is what resolve answers for a name, one list per call and the last one again
	// for every call after: how a row drives a name that answers one address to the check and
	// another to a lookup after it, which is DNS rebinding.
	answersMu sync.Mutex
	answers   map[string][][]netip.Addr
	// dialed is every address the proxy opened a socket to, in order, which is how a row reads
	// where a dial went.
	dialedMu sync.Mutex
	dialed   []string
	// holdResolve and holdDial, where a row sets one, run at the start of each lookup and each
	// dial with its own context: how a row holds a lookup or a dial until the command behind it
	// has gone. A lookup a hold ended fails with its context's error, and a dial is made under
	// the context holdDial answers, so a row has it fail as a dial its command abandoned does or
	// open as one that won that race does.
	holdResolve func(ctx context.Context)
	holdDial    func(ctx context.Context) context.Context
	// dialTimeout is the dialer's own Timeout for each address a row names it for, as the
	// production dialer has one (New): how a row has a dial time out on its own, with the error
	// net.Dialer itself answers, rather than end because its command went away.
	dialTimeout map[string]time.Duration
	// dialledFor is the grant each socket the proxy opened was dialled for, as the lines of that
	// dial name it (callerOfContext), by the socket's own local address, which is the remote
	// address the service it reached reads each request on it as: how a row reads which caller a
	// socket was opened for beside the request it carried.
	dialledForMu sync.Mutex
	dialledFor   map[string]string

	service *proxyrulestest.Service

	// protocol is what the command's own client speaks to a bound host (clientAt): protocolHTTP1,
	// or protocolH2 alone, as a gRPC client offers it. newRig sets the suite's, and a row about
	// one of the two sets its own after newRig.
	protocol string
}

// wrapListener is the seam a row puts a connection of its own under the proxy with: every
// connection this listener accepts is handed to each wrap in turn, and what the last one
// answers is what the proxy hijacks, decrypts, cuts and joins. A row that has to hold one of
// those sockets - or watch what is read of it - reaches it here, under everything the proxy
// builds on top of it, rather than sniffing a wrapper out of what the proxy holds.
type wrapListener struct {
	net.Listener
	wrap []func(net.Conn) net.Conn
}

func (l *wrapListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	for _, wrap := range l.wrap {
		conn = wrap(conn)
	}
	return conn, nil
}

// newRig builds the whole world under testRules. Everything of o the caller leaves zero is filled
// in, and each wrap, where a row gives one, is applied in order to every connection the proxy
// port's listener accepts.
func newRig(t *testing.T, o Options, wrap ...func(net.Conn) net.Conn) *rig {
	t.Helper()
	return newRuledRig(t, testRules(), o, wrap...)
}

// newRuledRig is newRig with rules as the fake grants' airlock rules, the rules the proxy reads on
// each request (GrantSource.AirlockRules). A row that gives a grant source of its own (o.Grants)
// has that source's rules read instead, and rules go to a fake nothing reads.
func newRuledRig(t *testing.T, rules proxyrules.Rules, o Options, wrap ...func(net.Conn) net.Conn) *rig {
	t.Helper()
	return newServedRig(t, nil, rules, o, wrap...)
}

// newServedRig is newRuledRig with the stand-in service answering as each host of also, beside
// the bound host: each resolves to a public address of its own, wired to that one service, so a
// row whose rules bind a real service's name (api.anthropic.com) reaches the same stand-in.
func newServedRig(t *testing.T, also []string, rules proxyrules.Rules, o Options, wrap ...func(net.Conn) net.Conn) *rig {
	t.Helper()
	r := &rig{t: t, grants: newFakeGrants(), logs: newLogCapture(), trust: x509.NewCertPool(), protocol: suiteProtocol}
	r.grants.rules = rules

	// The stand-in service behind the bound host, with a certificate of its own naming that
	// host and an authority of the rig's own behind it, which is the only thing the proxy
	// trusts upstream.
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Hosts: append([]string{boundHost}, also...)})
	if err != nil {
		t.Fatalf("the stand-in service: %v", err)
	}
	r.service = service
	t.Cleanup(r.service.Close)

	// The two tunnelled services: one that speaks TLS with a certificate of its own, and one
	// plain echo the idle and drain rows run bytes through.
	tunnelCA := newTestAuthority(t)
	tunnelCertificate, err := tunnelCA.leaf(tunnelHost)
	if err != nil {
		t.Fatalf("the tunnel service's certificate: %v", err)
	}
	tunnelAddr := startTLSEcho(t, tunnelCertificate)
	echoAddr := startEcho(t)
	r.trust.AddCert(tunnelCA.Certificate())

	r.authority = newTestAuthority(t)
	r.trust.AddCert(r.authority.Certificate())

	r.addresses = map[string][]netip.Addr{
		boundHost:  {netip.MustParseAddr("93.184.216.34")},
		tunnelHost: {netip.MustParseAddr("93.184.216.35")},
		echoHost:   {netip.MustParseAddr("93.184.216.36")},
	}
	r.answers = map[string][][]netip.Addr{}
	r.dialTo = map[string]string{
		net.JoinHostPort(boundHost, "443"):  r.service.Addr(),
		net.JoinHostPort(tunnelHost, "443"): tunnelAddr,
		net.JoinHostPort(echoHost, "443"):   echoAddr,
	}
	for i, host := range also {
		r.addresses[host] = []netip.Addr{netip.AddrFrom4([4]byte{93, 184, 216, byte(50 + i)})}
		r.dialTo[net.JoinHostPort(host, "443")] = r.service.Addr()
	}

	// The fake grants, unless the row gives a grant source of its own: a real broker's
	// requests.Machine (profile_test.go, and slack_test.go's newSlackBroker), whose reads no map a
	// row writes stands in for. r.grants is then a fake nothing reads.
	if o.Grants == nil {
		o.Grants = r.grants
	}
	o.CA = r.authority
	o.Log = slog.New(r.logs)
	o.Dial = r.dial
	o.Resolve = r.resolve
	// What the proxy trusts upstream: the stand-in service's own authority, in place of the
	// host's roots. Nothing else about the upstream side is replaced.
	o.UpstreamRoots = r.service.Authority().Pool()
	r.proxy = New(o)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the proxy port: %v", err)
	}
	// The address is the real listener's, which is what a row dials; the wrapper is only what
	// the proxy serves on.
	r.addr = listener.Addr().String()
	served := &wrapListener{Listener: listener, wrap: wrap}
	go func() {
		if err := r.proxy.Serve(served); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the proxy stopped serving: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.proxy.Shutdown(ctx)
	})
	return r
}

// dial is Options.Dial: every upstream connection of the proxy, counted, recorded and routed to
// whichever stand-in service the row means. What it is handed is an address and a port, the
// address the proxy resolved and checked, so the name it was resolved from is read back out of
// the rig's own answers.
func (r *rig) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.dials.Add(1)
	r.dialedMu.Lock()
	r.dialed = append(r.dialed, addr)
	r.dialedMu.Unlock()
	if r.holdDial != nil {
		ctx = r.holdDial(ctx)
	}
	to, ok := r.dialTo[addr]
	if !ok {
		to, ok = r.dialTo[r.nameOf(addr)]
	}
	if !ok {
		return nil, fmt.Errorf("this test dials no %s", addr)
	}
	dialer := net.Dialer{Timeout: r.dialTimeout[addr]}
	conn, err := dialer.DialContext(ctx, network, to)
	if err != nil {
		return nil, err
	}
	r.dialledForMu.Lock()
	defer r.dialledForMu.Unlock()
	if r.dialledFor == nil {
		r.dialledFor = map[string]string{}
	}
	r.dialledFor[conn.LocalAddr().String()] = callerOfContext(ctx).GrantID
	return conn, nil
}

// socketCaller is the grant the socket whose local address is local was dialled for, and "" for
// an address no socket of the proxy's had.
func (r *rig) socketCaller(local string) string {
	r.dialledForMu.Lock()
	defer r.dialledForMu.Unlock()
	return r.dialledFor[local]
}

// nameOf is the authority a row wrote dialTo under for an address and port: the name whose
// addresses hold that address, with that port. Names are walked in order, so two names sharing
// one address answer the same way in every run.
func (r *rig) nameOf(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	for _, name := range slices.Sorted(maps.Keys(r.addresses)) {
		for _, held := range r.addresses[name] {
			if held.String() == host {
				return net.JoinHostPort(name, port)
			}
		}
	}
	return addr
}

// dialedAddresses is every address the proxy dialled so far.
func (r *rig) dialedAddresses() []string {
	r.dialedMu.Lock()
	defer r.dialedMu.Unlock()
	return slices.Clone(r.dialed)
}

// resolve is Server.resolve: an address literal is itself, a name a row queued answers for has
// them one per call, a name the row named has the addresses it was given, and any other name
// resolves to nothing.
func (r *rig) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if r.holdResolve != nil {
		r.holdResolve(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	r.answersMu.Lock()
	queued, ok := r.answers[host]
	if ok && len(queued) > 0 {
		next := queued[0]
		if len(queued) > 1 {
			// The last answer stands for every lookup after it, so a row names one answer
			// for the check and one for everything the name is looked up for afterwards.
			r.answers[host] = queued[1:]
		}
		r.answersMu.Unlock()
		return next, nil
	}
	r.answersMu.Unlock()
	if addrs, ok := r.addresses[host]; ok {
		return addrs, nil
	}
	return nil, fmt.Errorf("no such host")
}

// requests is what the stand-in service has been sent.
func (r *rig) requests() []seenRequest { return r.service.Requests() }

// answerWith is what the stand-in service answers from here on, in place of its one line: a row
// whose point is what the service received - the signature it verifies, the token it is given,
// the upgrade it answers - writes that answer here.
func (r *rig) answerWith(answer proxyrulestest.Answer) {
	r.service.AnswerWith(answer)
}

// client is a command's own HTTP client, pointed at the rig's proxy with password as its proxy
// password and trusting the proxy's authority. Each call makes a client of its own, so two of
// them are two connections, which is what a command's second call after a pod minted something
// for it is.
func (r *rig) client(password string) *http.Client {
	r.t.Helper()
	return r.clientAt(r.addr, password)
}

// clientAt is that client pointed at one proxy address, which a row with a second pod of its
// own needs. It speaks r.protocol to a bound host: HTTP/1.1 as net/http's client does with a TLS
// configuration of its own, or HTTP/2 alone, whose handshake offers h2 and nothing else, as a
// gRPC client's does, so a proxy that does not answer h2 fails the handshake rather than falling
// back to HTTP/1.1.
func (r *rig) clientAt(addr, password string) *http.Client {
	r.t.Helper()
	proxyURL, err := url.Parse("http://agent:" + password + "@" + addr)
	if err != nil {
		r.t.Fatalf("the proxy URL: %v", err)
	}
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: r.trust},
	}
	switch r.protocol {
	case protocolHTTP1:
	case protocolH2:
		var protocols http.Protocols
		protocols.SetHTTP2(true)
		transport.Protocols = &protocols
	default:
		r.t.Fatalf("the rig's protocol is %q, want %q or %q", r.protocol, protocolHTTP1, protocolH2)
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport}
}

// get is one GET on a bound host through the proxy, with the headers given as name/value pairs.
func (r *rig) get(client *http.Client, target string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	return r.do(client, http.MethodGet, target, "", headers...)
}

// do is one request through the proxy. target is a whole URL; a body is sent when one is given.
func (r *rig) do(client *http.Client, method, target, body string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		r.t.Fatalf("build the request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			request.Host = headers[i+1]
			continue
		}
		request.Header.Set(headers[i], headers[i+1])
	}
	answer, err := client.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s through the proxy: %v", method, target, err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		r.t.Fatalf("read the answer: %v", err)
	}
	return answer, string(read)
}

// connectRaw writes one CONNECT on a connection of its own and answers what came back, with the
// connection still open: the rows about the credential, the tunnel and the idle limit each need
// the bytes rather than an HTTP client's reading of them. Each header must end in CRLF.
func (r *rig) connectRaw(target string, headers ...string) (*http.Response, net.Conn, *bufio.Reader) {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatalf("dial the proxy: %v", err)
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + strings.Join(headers, "") + "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		r.t.Fatalf("write the CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		r.t.Fatalf("read the answer to the CONNECT: %v", err)
	}
	r.t.Cleanup(func() { conn.Close() })
	return answer, conn, reader
}

// plainRaw writes one absolute-URL GET of http://<authority><path> on a connection of its own,
// with authority as its Host header too, and answers what came back with its body read. A row
// about a zoned literal needs it: net/http's client drops the zone from the URL it writes to a
// proxy, so through that client the zone never reaches the proxy, while any client that writes
// its own bytes can send it. Each header must end in CRLF.
func (r *rig) plainRaw(authority, path string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	return r.requestRaw(http.MethodGet, "http://"+authority+path, authority, headers...)
}

// requestRaw writes one request of the row's own bytes on a connection of its own - its method,
// its request target and a Host header that need not agree with it, as no HTTP client writes
// them but any command that writes its own bytes can - and answers what came back with its body
// read. A row sends a CONNECT through it only when the CONNECT is refused, since the body of a
// tunnel that opened never ends. Each header must end in CRLF.
func (r *rig) requestRaw(method, target, host string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	request := method + " " + target + " HTTP/1.1\r\nHost: " + host + "\r\n" + strings.Join(headers, "") + "Connection: close\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		r.t.Fatalf("write the %s: %v", method, err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		r.t.Fatalf("read the answer to %s %s: %v", method, target, err)
	}
	return answer, bodyOf(r.t, answer)
}

// basic is one Proxy-Authorization header line, as a command's client writes it.
func basic(password string) string {
	return "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("agent:"+password)) + "\r\n"
}

// bodyOf reads and closes an answer's body.
func bodyOf(t *testing.T, answer *http.Response) string {
	t.Helper()
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return string(body)
}

// newTestAuthority is a certificate authority of its own: a key and certificate written to PEM
// files and read back through the production reader, so every row signs with the same code a pod
// does.
func newTestAuthority(t *testing.T) *Authority {
	t.Helper()
	return newTestAuthorityOn(t, nil)
}

// newTestAuthorityOn is newTestAuthority signing by clock, and nil is the system's.
func newTestAuthorityOn(t *testing.T, clock Clock) *Authority {
	t.Helper()
	certFile, keyFile := writeCAFiles(t)
	authority, err := NewAuthority(certFile, keyFile, AuthorityOptions{Clock: clock})
	if err != nil {
		t.Fatalf("the test certificate authority: %v", err)
	}
	return authority
}

// writeCAFiles makes one certificate authority through the stand-in package and writes its PEM
// certificate and key into a directory of its own, which is the pair a pod mounts.
func writeCAFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	authority, err := proxyrulestest.NewAuthority("airlock test authority")
	if err != nil {
		t.Fatalf("the authority: %v", err)
	}
	certFile, keyFile, err = authority.WriteFiles(t.TempDir())
	if err != nil {
		t.Fatalf("write the authority's files: %v", err)
	}
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startEcho is a plain TCP service that writes back whatever it is sent.
func startEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// startTLSEcho is the same service behind a TLS certificate of its own, which is how the tunnel
// row proves the proxy opened nothing.
func startTLSEcho(t *testing.T, certificate *tls.Certificate) string {
	t.Helper()
	certificates := &tls.Config{Certificates: []tls.Certificate{*certificate}}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", certificates)
	if err != nil {
		t.Fatalf("bind the TLS echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}
