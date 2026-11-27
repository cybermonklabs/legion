package sessionproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testInstance is a placeholder connection name: this repository names no real instance.
const testInstance = "example-project:us-central1:example-instance"

// testDialTimeout bounds every dial of a test whose upstream answers at once, so it only has to be
// long enough never to end a dial a loaded machine is slow to complete.
const testDialTimeout = 10 * time.Second

// testWriterAudience is a workload identity provider's full resource name, with placeholder ids.
const testWriterAudience = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/legion-session-writer/providers/example-cluster"

// upstream is the database end of a test: a real local listener, so the bytes the proxy copies
// really cross a connection and a test can read them.
type upstream struct {
	listener net.Listener
	accepted chan net.Conn
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen as the upstream: %v", err)
	}
	up := &upstream{listener: listener, accepted: make(chan net.Conn, 8)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			up.accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for {
			select {
			case conn := <-up.accepted:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	return up
}

// next is the connection the upstream accepted next, which is the one the proxy just dialed.
func (u *upstream) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-u.accepted:
		return conn
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy did not dial the upstream")
		return nil
	}
}

// fakeDialer stands in for the Cloud SQL connector: it records what the proxy asked of it, and the
// signer each build was given, and hands back a real connection to the test's upstream listener.
type fakeDialer struct {
	addr string

	mu        sync.Mutex
	instances []string
	signers   []Signer
	err       error
	closed    int
}

func (f *fakeDialer) Dial(ctx context.Context, instance string) (net.Conn, error) {
	f.mu.Lock()
	f.instances = append(f.instances, instance)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", f.addr)
}

func (f *fakeDialer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeDialer) dials() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

func (f *fakeDialer) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeDialer) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// build is the Config.NewDialer a test passes: the proxy gets this very fake, and each build
// records the signer it was given.
func (f *fakeDialer) build(_ context.Context, signer Signer) (Dialer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signers = append(f.signers, signer)
	return f, nil
}

// built is the signer of every build so far, in the order they were made.
func (f *fakeDialer) built() []Signer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.signers)
}

// socketPath is this package's socket path: a socket named session.sock in a directory of the
// test's own, placed by TMPDIR and falling back to /tmp for the socket path's length alone, so
// that the bind holds within the 107 bytes a unix socket path takes (docs/testing.md).
func socketPath(t *testing.T) string {
	t.Helper()
	return testtmp.Socket(t, "session.sock")
}

// startProxy starts a proxy and takes the connection Start's proof dial made, so that the next
// upstream connection a test reads is the one its own client caused.
func startProxy(ctx context.Context, t *testing.T, up *upstream, dialer *fakeDialer, socket string) *Proxy {
	t.Helper()
	return startConfigured(ctx, t, up, Config{Socket: socket, NewDialer: dialer.build})
}

// startConfigured is startProxy for a test that sets more of the Config: it fills in the test's
// instance and dial bound, starts the proxy and takes the connection the proof dial made.
func startConfigured(ctx context.Context, t *testing.T, up *upstream, cfg Config) *Proxy {
	t.Helper()
	cfg.Instance, cfg.DialTimeout = testInstance, testDialTimeout
	proxy, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = up.next(t).Close()
	return proxy
}

func writeAll(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a write deadline: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write %q: %v", payload, err)
	}
}

func readString(t *testing.T, conn net.Conn, n int) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

// expectNothing fails if anything arrives on conn within a short wait.
func expectNothing(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("read %q on a connection that should have carried nothing", buf[:n])
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("read on a connection that should have carried nothing: %v", err)
	}
}

func TestProxyCarriesBytesBothWays(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	writeAll(t, client, "startup packet")
	server := up.next(t)
	if got := readString(t, server, len("startup packet")); got != "startup packet" {
		t.Fatalf("the upstream read %q", got)
	}
	writeAll(t, server, "ready for query")
	if got := readString(t, client, len("ready for query")); got != "ready for query" {
		t.Fatalf("the client read %q", got)
	}

	_ = client.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartRefusesWhenTheProofDialFails(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	dialer.failWith(errors.New("the instance refused the login"))
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: testDialTimeout,
	})
	if err == nil {
		t.Fatal("Start accepted a dialer that cannot reach the instance")
	}
	// A dial the instance refused keeps the dial's own words: it is no timeout.
	if want := "sessionproxy: dial " + testInstance + ": the instance refused the login"; err.Error() != want {
		t.Fatalf("Start = %v, want %q", err, want)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}

	// Nothing holds the address afterwards, so the generation can be started again.
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}

// silentUpstream is an instance that never answers: a real listener that accepts every connection
// and never writes to it, holding each one open until the test ends.
func silentUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen as the silent upstream: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// stallingDialer stands in for the Cloud SQL connector dialing an instance that never answers: it
// opens a real connection to a silent upstream and runs a real TLS handshake over it, which, like
// the connector's own, ends only when its context does.
type stallingDialer struct {
	addr string

	mu     sync.Mutex
	closed int
}

func (s *stallingDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "x"})
	if err := client.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (s *stallingDialer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *stallingDialer) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *stallingDialer) build(context.Context, Signer) (Dialer, error) { return s, nil }

// A proof dial the instance never answers ends at DialTimeout, however long the start's own
// context lives: Start refuses with the instance and the bound named, the dial's deadline still
// readable in the error, the socket path free and the dialer closed, as for a dial the instance
// refused.
func TestStartRefusesWhenTheProofDialGetsNoAnswerWithinItsBound(t *testing.T) {
	dialer := &stallingDialer{addr: silentUpstream(t)}
	socket := socketPath(t)

	began := time.Now()
	_, err := Start(context.Background(), Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: 200 * time.Millisecond,
	})
	waited := time.Since(began)
	if err == nil {
		t.Fatal("Start served although its proof dial was never answered")
	}
	if waited >= 2*time.Second {
		t.Fatalf("Start refused %s after it began, want within its 200ms proof bound", waited)
	}
	if want := "sessionproxy: dial " + testInstance + ": no answer within 200ms"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Start = %v, want it to begin %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}

// A proof dial is never unbounded: a Start given no DialTimeout is refused, naming the field,
// before it binds or dials anything.
func TestStartRefusesAnUnboundedProofDial(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err == nil || !strings.Contains(err.Error(), "DialTimeout") {
		t.Fatalf("Start = %v, want a refusal naming DialTimeout", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start with no proof bound dialed the upstream %d times", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Start with no proof bound left something at the socket path: %v", err)
	}
}

// An account that is no service account's email is refused, naming it, before Start binds
// anything or builds a dialer. The login form, the account's email without .gserviceaccount.com, is
// the likeliest slip, because the session database's user is that login.
func TestStartRefusesAnAccountThatIsNoServiceAccountEmail(t *testing.T) {
	const login = "legion-sessions@example-project.iam"
	socket := socketPath(t)
	built := 0

	_, err := Start(context.Background(), Config{
		Socket:                    socket,
		Instance:                  testInstance,
		ImpersonateServiceAccount: login,
		DialTimeout:               testDialTimeout,
		NewDialer: func(context.Context, Signer) (Dialer, error) {
			built++
			return nil, errors.New("the dialer factory was called")
		},
	})
	want := fmt.Sprintf("sessionproxy: ImpersonateServiceAccount %q is not a Google service account's email", login)
	if err == nil || err.Error() != want {
		t.Fatalf("Start = %v, want %q", err, want)
	}
	if built != 0 {
		t.Fatalf("a Start with no service account's email called its dialer factory %d times", built)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Start with no service account's email left something at the socket path: %v", err)
	}
}

// A writer token is named whole, with its audience and its file, and only with the service account
// it impersonates: a half-named one, or one without the account, would sign in with no credential
// or with the pod's own where the deployment named the launcher's. An audience that is no
// provider's full resource name, and a token file that is no absolute path, are refused too. Each
// refusal names the fields, before Start binds anything or builds a dialer.
func TestStartRefusesAWriterTokenNamedByHalfOrWithoutItsAccount(t *testing.T) {
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	const needsTokenFile = "sessionproxy: WriterAudience needs WriterTokenFile, the file of the token it is the audience of"
	const needsAudience = "sessionproxy: WriterTokenFile needs WriterAudience, the audience of the token it holds"
	tokenFile := filepath.Join(t.TempDir(), "token")
	byID := strings.Replace(testWriterAudience, "/projects/123456789012/", "/projects/example-project/", 1)
	for _, tc := range []struct {
		name                         string
		account, audience, tokenFile string
		want                         string
	}{
		{"an audience without its token file", account, testWriterAudience, "", needsTokenFile},
		{"a token file without its audience", account, "", tokenFile, needsAudience},
		{"an audience alone", "", testWriterAudience, "", needsTokenFile},
		{"a token file alone", "", "", tokenFile, needsAudience},
		{
			"a writer token without the account", "", testWriterAudience, tokenFile,
			"sessionproxy: WriterAudience and WriterTokenFile need ImpersonateServiceAccount: the writer token signs in only by impersonating that service account",
		},
		{
			"the project's id in place of its number", account, byID, tokenFile,
			fmt.Sprintf("sessionproxy: WriterAudience %q is not a workload identity provider's full resource name, %s", byID, ompsessions.WriterAudienceForm),
		},
		{
			"a token file by a relative path", account, testWriterAudience, "session-writer/token",
			`sessionproxy: WriterTokenFile "session-writer/token" is not an absolute path`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := socketPath(t)
			built := 0
			_, err := Start(context.Background(), Config{
				Socket:                    socket,
				Instance:                  testInstance,
				ImpersonateServiceAccount: tc.account,
				WriterAudience:            tc.audience,
				WriterTokenFile:           tc.tokenFile,
				DialTimeout:               testDialTimeout,
				NewDialer: func(context.Context, Signer) (Dialer, error) {
					built++
					return nil, errors.New("the dialer factory was called")
				},
			})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Start = %v, want %q", err, tc.want)
			}
			if built != 0 {
				t.Fatalf("a refused Start called its dialer factory %d times", built)
			}
			if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused Start left something at the socket path: %v", err)
			}
		})
	}
}

// A proxy that the writer token signs in gives its dialer factory the account, the audience and
// the token file together: a factory given the account alone would impersonate it with the pod's
// own credentials. Its start line names the account and says that the launcher's writer token
// signs in. The line names no token, and not the token file either.
func TestStartGivesItsDialerTheAccountAndTheWriterToken(t *testing.T) {
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	tokenFile := filepath.Join(t.TempDir(), "token")
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	var logged strings.Builder
	proxy := startConfigured(context.Background(), t, up, Config{
		Socket: socket, NewDialer: dialer.build, Log: &logged,
		ImpersonateServiceAccount: account, WriterAudience: testWriterAudience, WriterTokenFile: tokenFile,
	})
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := Signer{Account: account, WriterAudience: testWriterAudience, WriterTokenFile: tokenFile}
	if got := dialer.built(); !slices.Equal(got, []Signer{want}) {
		t.Fatalf("the dialer factory was given %+v, want %+v once", got, want)
	}
	line := fmt.Sprintf("sessionproxy: serving %s on %s as %s with the launcher's writer token\n", testInstance, socket, account)
	if got := logged.String(); got != line {
		t.Fatalf("the proxy logged %q, want only %q", got, line)
	}
}

// A negative drain is refused, naming the field, before Start binds or dials anything.
func TestStartRefusesANegativeDrain(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: testDialTimeout,
		Drain:       -time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "Drain") {
		t.Fatalf("Start = %v, want a refusal naming Drain", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start with a negative drain dialed the upstream %d times", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Start with a negative drain left something at the socket path: %v", err)
	}
}

func TestStartRefusesWhenSomethingHoldsTheSocket(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	held, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen as the address's holder: %v", err)
	}
	defer held.Close()

	_, err = Start(context.Background(), Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: testDialTimeout,
	})
	if err == nil {
		t.Fatal("Start took an address another listener holds")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("the error does not name the socket path: %v", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start that could not bind still dialed the upstream %d times", got)
	}

	// The holder keeps its address: nothing removed the socket under it.
	go func() {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
		}
	}()
	if err := held.(*net.UnixListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set an accept deadline: %v", err)
	}
	conn, err := held.Accept()
	if err != nil {
		t.Fatalf("the holder lost its address: %v", err)
	}
	_ = conn.Close()
}

func TestConnectionIsClosedWhenItsUpstreamDialFails(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)
	// The credential path worked at Start and stops working afterwards, as a revoked identity
	// would.
	dialer.failWith(errors.New("the instance refused the login"))

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if n != 0 {
		t.Fatalf("the proxy wrote %q to a connection it could not serve", buf[:n])
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the client's read: %v, want EOF", err)
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestEachConnectionDialsItsOwnUpstream(t *testing.T) {
	// Every connection is dialed on its own, which is how each one gets a TLS session of its own
	// under the instance's ephemeral certificate, which carries the login: the Cloud SQL connector
	// refreshes that certificate inside a dial (serve). The dial count is the observable here; no
	// token source is faked.
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	proxy, err := Start(context.Background(), Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: testDialTimeout,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := dialer.dials(); got != 1 {
		t.Fatalf("Start made %d dials, want the one that proves the credential path", got)
	}
	_ = up.next(t).Close()

	first, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	writeAll(t, first, "one")
	firstUpstream := up.next(t)
	if got := readString(t, firstUpstream, 3); got != "one" {
		t.Fatalf("the first connection's upstream read %q", got)
	}

	second, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy a second time: %v", err)
	}
	writeAll(t, second, "two")
	secondUpstream := up.next(t)
	if got := readString(t, secondUpstream, 3); got != "two" {
		t.Fatalf("the second connection's upstream read %q", got)
	}
	if got := dialer.dials(); got != 3 {
		t.Fatalf("the proxy made %d dials, want the proof dial plus one per connection", got)
	}

	// The two connections share no upstream: nothing the second wrote reached the first's.
	expectNothing(t, firstUpstream)

	// And the first connection still carries bytes after the second arrived.
	writeAll(t, first, "still here")
	if got := readString(t, firstUpstream, len("still here")); got != "still here" {
		t.Fatalf("the first connection's upstream read %q", got)
	}

	_ = first.Close()
	_ = second.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// requirePeerReads skips a test on a system where the proxy reads no connection's peer.
func requirePeerReads(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the proxy reads a connection's peer (SO_PEERCRED) on Linux alone")
	}
}

// A connection that Admit refuses gets nothing: the proxy closes it with nothing written back and
// dials no upstream for it, so it costs no credential work. The proxy logs one line for it, which
// names the instance, the pid and the command of the process that connected, and Admit's reason.
func TestAConnectionAdmitRefusesIsClosedWithoutADial(t *testing.T) {
	requirePeerReads(t)
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	var logged strings.Builder
	asked := make(chan Peer, 1)
	proxy := startConfigured(context.Background(), t, up, Config{
		Socket: socket, NewDialer: dialer.build, Log: &logged,
		Admit: func(peer Peer) error {
			asked <- peer
			return errors.New("not the generation's agent")
		},
	})

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if n != 0 {
		t.Fatalf("the proxy wrote %q to a connection Admit refused", buf[:n])
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the refused client's read: %v, want EOF", err)
	}
	// Close returns once every connection's serve has ended, so the log is whole after it.
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := dialer.dials(); got != 1 {
		t.Fatalf("the proxy dialed the upstream %d times, want only Start's proof dial: a refused connection costs no dial", got)
	}
	if peer := admitted(t, asked); peer.PID != os.Getpid() {
		t.Fatalf("Admit was given the pid %d, want this test's own %d", peer.PID, os.Getpid())
	}
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatalf("read this test's command: %v", err)
	}
	want := fmt.Sprintf("sessionproxy: refused a connection to %s from pid %d (%q): not the generation's agent",
		testInstance, os.Getpid(), strings.TrimSuffix(string(comm), "\n"))
	var refusals []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "refused") {
			refusals = append(refusals, line)
		}
	}
	if !slices.Equal(refusals, []string{want}) {
		t.Fatalf("the proxy logged the refusals %q, want one line %q; the whole log is %q", refusals, want, logged.String())
	}
}

// peerHelperEnv names the socket that TestSessionProxyPeerHelper connects to, in a test's child
// process.
const peerHelperEnv = "LEGION_SESSIONPROXY_TEST_PEER_SOCKET"

// TestSessionProxyPeerHelper is a child process of a test. It connects to the socket that
// peerHelperEnv names and holds the connection until its stdin ends, so it is still the socket's
// peer while the proxy reads that peer. In a test run of its own it does nothing.
func TestSessionProxyPeerHelper(t *testing.T) {
	socket := os.Getenv(peerHelperEnv)
	if socket == "" {
		return
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", socket, err)
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = conn.Close()
	os.Exit(0)
}

// Admit is given the process that made the connection, as the kernel recorded it: the pid and
// user id of this test's own process when it connects, and those of a child process when the child
// connects. The launcher decides on that pid, so a proxy that gave Admit another pid, its own
// above all, would let Admit take a process for one it never was.
func TestAdmitIsGivenTheProcessThatConnects(t *testing.T) {
	requirePeerReads(t)
	// serve starts a proxy whose Admit records each peer it is given and admits it.
	serve := func(t *testing.T) (string, *upstream, <-chan Peer) {
		t.Helper()
		up := newUpstream(t)
		socket := socketPath(t)
		asked := make(chan Peer, 1)
		proxy := startConfigured(context.Background(), t, up, Config{
			Socket: socket, NewDialer: (&fakeDialer{addr: up.listener.Addr().String()}).build,
			Admit: func(peer Peer) error {
				asked <- peer
				return nil
			},
		})
		t.Cleanup(func() {
			if err := proxy.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return socket, up, asked
	}

	t.Run("this test's process", func(t *testing.T) {
		socket, up, asked := serve(t)
		client, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatalf("dial the proxy: %v", err)
		}
		defer client.Close()
		if peer, want := admitted(t, asked), (Peer{PID: os.Getpid(), UID: os.Getuid()}); peer != want {
			t.Fatalf("Admit was given %+v, want this test's own process %+v", peer, want)
		}
		// An admitted connection is served as any other.
		writeAll(t, client, "startup packet")
		server := up.next(t)
		defer server.Close()
		if got := readString(t, server, len("startup packet")); got != "startup packet" {
			t.Fatalf("the upstream read %q", got)
		}
	})

	t.Run("a child process", func(t *testing.T) {
		socket, up, asked := serve(t)
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(self, "-test.run=^TestSessionProxyPeerHelper$")
		child.Env = append(os.Environ(), peerHelperEnv+"="+socket)
		child.Stdout, child.Stderr = os.Stderr, os.Stderr
		hold, err := child.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Start(); err != nil {
			t.Fatalf("start the child: %v", err)
		}
		defer func() {
			_ = hold.Close()
			if err := child.Wait(); err != nil {
				t.Errorf("the child: %v", err)
			}
		}()
		if peer, want := admitted(t, asked), (Peer{PID: child.Process.Pid, UID: os.Getuid()}); peer != want {
			t.Fatalf("Admit was given %+v, want the child's process %+v (this test's own pid is %d)", peer, want, os.Getpid())
		}
		_ = up.next(t).Close()
	})
}

// admitted is the next peer that the proxy's Admit was given. Its one bound is the test binary's
// deadline (testwait.Receive), so a child process a loaded machine starts slowly is still waited
// for, and an Admit never asked fails the row by name.
func admitted(t *testing.T, asked <-chan Peer) Peer {
	t.Helper()
	return testwait.Receive(t, "the proxy's Admit to be asked about the connection", asked)
}

// expectClosed fails unless conn's far end has been closed: a read ends with no bytes, at once and
// not for want of time.
func expectClosed(t *testing.T, conn net.Conn, who string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("%s read %q on a connection that should have been closed", who, buf[:n])
	}
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("%s's connection is still open: %v", who, err)
	}
}

// Close ends the proxy however long its clients hold on: a client that keeps its connection open
// is given the drain and no more, after which Close closes both ends of that connection, says so
// and returns. The address is gone, the dialer is closed once, and a second Close is a no-op.
func TestCloseEndsTheProxy(t *testing.T) {
	for _, drain := range []time.Duration{0, 200 * time.Millisecond} {
		t.Run("drain "+drain.String(), func(t *testing.T) {
			up := newUpstream(t)
			dialer := &fakeDialer{addr: up.listener.Addr().String()}
			socket := socketPath(t)
			var logged strings.Builder
			proxy := startConfigured(context.Background(), t, up, Config{
				Socket: socket, NewDialer: dialer.build, Drain: drain, Log: &logged,
			})

			// Bytes cross both ways first, so the connection is established end to end, and the
			// client then holds it open and never hangs up.
			client, err := net.Dial("unix", socket)
			if err != nil {
				t.Fatalf("dial the proxy: %v", err)
			}
			defer client.Close()
			writeAll(t, client, "in flight")
			server := up.next(t)
			defer server.Close()
			if got := readString(t, server, len("in flight")); got != "in flight" {
				t.Fatalf("the upstream read %q", got)
			}
			writeAll(t, server, "answered")
			if got := readString(t, client, len("answered")); got != "answered" {
				t.Fatalf("the client read %q", got)
			}

			began := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- proxy.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Close did not return within 2s while a client held its connection past the %s drain", drain)
			}
			if waited := time.Since(began); waited < drain {
				t.Fatalf("Close returned after %s, inside the %s drain an open connection is owed", waited, drain)
			}

			expectClosed(t, client, "the client")
			expectClosed(t, server, "the upstream")
			if conn, err := net.Dial("unix", socket); err == nil {
				_ = conn.Close()
				t.Fatal("the proxy still accepts connections after Close")
			}
			if got := dialer.closeCount(); got != 1 {
				t.Fatalf("Close closed the dialer %d times", got)
			}
			want := fmt.Sprintf("sessionproxy: closed 1 connections to %s still open after %s", testInstance, drain)
			if !strings.Contains(logged.String(), want) {
				t.Fatalf("the proxy logged %q, want %q", logged.String(), want)
			}
			if err := proxy.Close(); err != nil {
				t.Fatalf("a second Close: %v", err)
			}
		})
	}
}

// A connection that finishes on its own inside the drain is not cut: its last write, made after
// Close began, reaches the upstream whole, the answer reaches the client, and Close returns as soon
// as the client hangs up, long before the drain is over, without closing anything itself.
func TestCloseLetsAConnectionFinishInsideTheDrain(t *testing.T) {
	const drain = 5 * time.Second
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	var logged strings.Builder
	proxy := startConfigured(context.Background(), t, up, Config{
		Socket: socket, NewDialer: dialer.build, Drain: drain, Log: &logged,
	})

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	writeAll(t, client, "first")
	server := up.next(t)
	defer server.Close()
	if got := readString(t, server, len("first")); got != "first" {
		t.Fatalf("the upstream read %q", got)
	}

	began := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()
	// Close closes the dialer before it waits on the connections, so a closed dialer means Close
	// is under way.
	for deadline := time.Now().Add(2 * time.Second); dialer.closeCount() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Close never closed the dialer")
		}
	}

	// Larger than a socket buffer holds, so the copy is still under way as the write is made.
	payload := strings.Repeat("last write ", 64<<10)
	wrote := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte(payload))
		wrote <- err
	}()
	if got := readString(t, server, len(payload)); got != payload {
		t.Fatalf("the upstream read %d bytes that are not the client's last write", len(got))
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the client's last write: %v", err)
	}
	writeAll(t, server, "done")
	if got := readString(t, client, len("done")); got != "done" {
		t.Fatalf("the client read %q", got)
	}
	_ = client.Close()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return once the connection had finished")
	}
	if waited := time.Since(began); waited >= drain {
		t.Fatalf("Close returned %s after it began, not before the %s drain was over", waited, drain)
	}
	expectClosed(t, server, "the upstream")
	if strings.Contains(logged.String(), "still open") {
		t.Fatalf("Close closed a connection that finished inside the drain: %q", logged.String())
	}
}

// The context a proxy was started with bounds its start and nothing after: once Start has returned,
// cancelling it leaves the proxy serving, with its dialer open and every connection still dialed
// upstream, and only Close ends it. The launcher relies on this to serve a child through its stop
// grace when the container is told to stop and the context it started the proxy with is gone.
func TestCancellingTheContextLeavesAStartedProxyServingUntilClose(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	proxy := startProxy(ctx, t, up, dialer, socket)

	cancel()
	// Long enough for anything that watched the context to have acted on it.
	time.Sleep(100 * time.Millisecond)

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy after its start context ended: %v", err)
	}
	defer client.Close()
	writeAll(t, client, "last write")
	server := up.next(t)
	if got := readString(t, server, len("last write")); got != "last write" {
		t.Fatalf("the upstream read %q", got)
	}
	writeAll(t, server, "written")
	if got := readString(t, client, len("written")); got != "written" {
		t.Fatalf("the client read %q", got)
	}
	if got := dialer.closeCount(); got != 0 {
		t.Fatalf("the dialer was closed %d times while the proxy still served", got)
	}

	_ = client.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		_ = conn.Close()
		t.Fatal("the proxy still accepts connections after Close")
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("Close closed the dialer %d times", got)
	}
}

// A start whose context has already ended is refused: its proof dial cannot succeed, so nothing is
// left bound at the socket path and the dialer it built is closed.
func TestStartRefusesWhenItsContextHasEnded(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Start(ctx, Config{
		Socket:      socket,
		Instance:    testInstance,
		NewDialer:   dialer.build,
		DialTimeout: testDialTimeout,
	})
	if err == nil {
		t.Fatal("Start served although its context had ended")
	}
	if !strings.Contains(err.Error(), testInstance) {
		t.Fatalf("the error does not name the instance: %v", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}

// A started proxy's Close unlinks nothing by the socket's path: a generation's agent can replace
// its own g<n> directory with a link to a sibling's, and an unlink by path would then remove the
// sibling's socket. The proxy's socket is left for its owner's removal of the directory, which
// follows no link.
func TestCloseUnlinksNoSocketThroughALinkedDirectory(t *testing.T) {
	// The sockets bound here are g1/.s.PGSQL.5432 and the sibling's g2/.s.PGSQL.5432, of the
	// same length, so that pair is the room the directory needs.
	base := testtmp.Dir(t, filepath.Join("g1", ompsessions.SocketName))
	own, sibling := filepath.Join(base, "g1"), filepath.Join(base, "g2")
	for _, dir := range []string{own, sibling} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	siblingListener, err := net.Listen("unix", filepath.Join(sibling, ompsessions.SocketName))
	if err != nil {
		t.Fatalf("listen as the sibling: %v", err)
	}
	siblingListener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer siblingListener.Close()

	up := newUpstream(t)
	proxy := startProxy(context.Background(), t, up, &fakeDialer{addr: up.listener.Addr().String()}, filepath.Join(own, ompsessions.SocketName))
	// The agent's swap: its own directory moved aside, a link to the sibling's in its place.
	moved := filepath.Join(base, "g1.moved")
	if err := os.Rename(own, moved); err != nil {
		t.Fatalf("move %s aside: %v", own, err)
	}
	if err := os.Symlink(sibling, own); err != nil {
		t.Fatalf("link %s to %s: %v", own, sibling, err)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sibling, ompsessions.SocketName)); err != nil {
		t.Fatalf("the sibling's socket is gone after the proxy's Close: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(moved, ompsessions.SocketName)); err != nil {
		t.Fatalf("the proxy's own socket is gone after Close, which should have left it to its owner: %v", err)
	}
	// The owner's removal of the generation's path takes the link, never what it points at.
	if err := os.RemoveAll(own); err != nil {
		t.Fatalf("remove %s: %v", own, err)
	}
	if _, err := os.Lstat(filepath.Join(sibling, ompsessions.SocketName)); err != nil {
		t.Fatalf("the sibling's socket is gone after the generation's directory was removed: %v", err)
	}
}
