// Package sessionproxy carries one generation's Oh My Pi session database traffic to Cloud SQL
// with cloud credentials, so that no database password is ever written into a pod. The proxy
// signs in as the pod's own identity, or as a Google service account that the deployment names
// (Config.ImpersonateServiceAccount). The pod's own credentials impersonate that account, or, with
// Config.WriterAudience, a token that only the launcher reads does, once Google's security token
// service has exchanged it. Config.Admit can refuse a connection by the process that made it
// (Peer) before the proxy dials anything for it.
// The agent's client (Bun's SQL client over a unix socket) sends a plain Postgres startup packet
// with a user and a database and no password; this proxy dials Cloud SQL for every accepted
// connection with automatic IAM database authentication and copies bytes both ways.
//
// Nothing here reads the Postgres wire protocol, and nothing here belongs to it: with automatic
// IAM database authentication the login token travels in the connector's ephemeral-certificate
// exchange, not in the startup packet, which is why Google's own cloud-sql-proxy is likewise a
// plain bidirectional copy over the connector's net.Conn.
package sessionproxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/google/externalaccount"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// Google's security token service exchanges a projected ServiceAccount token for a federated
// token. Two exchanges use it: the proxy's writer token (connectorOptions), and the pod's
// Workload Identity token in the launcher's pod-local metadata endpoint. STSTokenURL is the
// service's token endpoint. JWTTokenType is the subject token type that both exchanges name,
// because a projected ServiceAccount token is a JWT.
const (
	STSTokenURL  = "https://sts.googleapis.com/v1/token"
	JWTTokenType = "urn:ietf:params:oauth:token-type:jwt"
)

// Dialer is the upstream the proxy dials for each accepted connection: the Cloud SQL connector in
// a pod, a fake over a local listener in a test. Close releases what the dialer holds, the
// connector's certificate cache above all.
type Dialer interface {
	Dial(ctx context.Context, instance string) (net.Conn, error)
	Close() error
}

// Config is one proxy's whole configuration. A proxy serves exactly one generation, so every field
// is fixed for that generation's lifetime.
type Config struct {
	// Socket is the absolute path of the unix socket the agent and the launcher's own client
	// connect to. It must not exist yet: the generation's private directory is fresh, so a socket
	// already sitting there means something else holds the address.
	Socket string
	// Instance is the Cloud SQL instance connection name, project:region:instance, or
	// domain:project:region:instance for a domain-scoped project (ompsessions.ValidInstance).
	Instance string
	// ImpersonateServiceAccount is the email of the Google service account the proxy impersonates
	// and signs in as (ompsessions.ServiceAccountLogin says which emails are one). The pod's own
	// credentials impersonate it, or the writer token does when WriterAudience is set. Empty signs
	// in as the pod's own identity.
	ImpersonateServiceAccount string
	// WriterAudience is the audience of the writer token, the projected ServiceAccount token that
	// only the launcher reads: a workload identity provider's full resource name
	// (ompsessions.ValidWriterAudience). With it, the proxy exchanges that token at Google's
	// security token service and impersonates ImpersonateServiceAccount with the federated token,
	// so it never uses the pod's own credentials. It needs WriterTokenFile and
	// ImpersonateServiceAccount. Empty impersonates with the pod's own credentials.
	WriterAudience string
	// WriterTokenFile is the absolute path of the writer token, which only the launcher reads.
	// The proxy reads the file again at each exchange, so it sends the token that the kubelet
	// last rotated. It is set exactly when WriterAudience is.
	WriterTokenFile string
	// NewDialer builds the upstream dialer for signer, which carries ImpersonateServiceAccount,
	// WriterAudience and WriterTokenFile. Nil builds the Cloud SQL connector's (connectorOptions):
	// it dials the instance's private IP with automatic IAM database authentication, and signs in
	// as the signer's account, or as the pod itself when the signer names no account.
	NewDialer func(ctx context.Context, signer Signer) (Dialer, error)
	// DialTimeout bounds every upstream dial: Start's proof dial, the one upstream connection that
	// proves the credential path before Start returns, and the dial serve makes for each accepted
	// connection after it. It must be positive: an instance that never answers, an unreachable
	// address or a peer that stalls its handshake, would otherwise hold Start for as long as its
	// caller's context lives, and hold a connection's dial, and the connector's certificate cache
	// with it, for as long as the proxy serves. A later dial does no more than the proof dial did
	// (a refresh of the certificate and its tokens, then the connect and the handshake), so the
	// bound the proof needs holds every dial.
	DialTimeout time.Duration
	// Drain is how long Close lets established connections finish on their own before it closes
	// them. Zero closes them at once; a negative Drain is refused. Close is bounded by it so long as
	// every per-connection dial honours cancellation, as the Cloud SQL connector's does: a client
	// that never hangs up then holds Close for Drain and no longer.
	Drain time.Duration
	// Log takes one line per event: the proxy's start, a connection the proxy refused (Admit), a
	// connection the upstream refused, an accept loop that stopped on its own, connections Close
	// closed once the drain was over. Nil logs nothing.
	Log io.Writer
	// Admit decides if the proxy serves an accepted connection, from the process that made it
	// (Peer). The proxy reads the peer and asks Admit before it dials anything for the
	// connection. An error refuses the connection, and the log line gives it as the reason. The
	// proxy closes a connection that Admit refuses, or whose peer it cannot read, and logs it in
	// one line. That connection costs no dial and no credential work. Nil admits every
	// connection. The proxy reads a peer on Linux alone, so on another system a set Admit refuses
	// every connection.
	Admit func(Peer) error
}

// Signer is the identity a proxy's dialer signs in to Cloud SQL as, and the credential that signs
// it in: the Config fields ImpersonateServiceAccount, WriterAudience and WriterTokenFile, which
// Start has checked. It is comparable, so a test can compare what its dialer factory got.
type Signer struct {
	// Account is the service account the dialer impersonates and signs in as. "" signs in as the
	// pod itself.
	Account string
	// WriterAudience and WriterTokenFile name the writer token, which impersonates Account in
	// place of the pod's own credentials. Both are "", or both are set.
	WriterAudience, WriterTokenFile string
}

// Peer is the process at the other end of a connection to the proxy's socket, as the kernel
// recorded it when that process connected (SO_PEERCRED). PID is in the proxy's own PID
// namespace, and a connection made from any thread of a process names that process's pid. UID is
// the process's user id.
type Peer struct {
	PID, UID int
}

// Proxy is a started session database proxy: a bound socket, a dialer whose credential path has
// been proven once, and an accept loop that runs until Close and nothing else. The context Start
// was given bounds only the start; its owner decides when the proxy goes by calling Close. The
// proxy keeps every connection it is copying for, the client and its upstream, so that Close can
// end those still open once the drain is over rather than wait on whoever holds them.
type Proxy struct {
	instance string
	listener net.Listener
	dialer   Dialer
	// admit is Config.Admit: nil serves every connection.
	admit func(Peer) error

	// cancel ends the proxy's own serving context, which the dialer was built with and every
	// per-connection dial uses, so the dialer's background work lives exactly as long as the proxy
	// and Close never waits on a dial that has nobody left to serve. Close is its only caller.
	cancel context.CancelFunc
	// inFlight counts the accept loop and every connection it is still copying for.
	inFlight sync.WaitGroup
	// drain is Config.Drain: how long Close waits for established connections before closing them.
	drain time.Duration
	// dialTimeout is Config.DialTimeout: the bound of each connection's dial (serve).
	dialTimeout time.Duration

	// connsMu guards closing and conns. closing is set once Close has begun, after which a pair
	// serve would register is closed at once instead; conns maps every client the proxy is copying
	// for to its upstream.
	connsMu sync.Mutex
	closing bool
	conns   map[net.Conn]net.Conn

	closeOnce sync.Once
	closeErr  error

	logMu sync.Mutex
	log   io.Writer
}

// Start binds the socket, builds the dialer, proves the credential path with one upstream
// connection and then serves. The order matters in both directions: the socket is bound before
// anything is dialed, so a start that loses the address to something else costs no credential
// work, and the proof dial happens before Start returns, so the launcher never starts an agent
// against a proxy that cannot reach the database.
//
// ctx bounds the start alone: a start whose ctx ends before the proof dial succeeds fails and
// leaves nothing bound. The proof dial is bounded besides by cfg.DialTimeout, so a start against
// an instance that never answers fails within that bound however long ctx lives, with an error
// naming the instance and the bound that wraps the dial's own (context.DeadlineExceeded). Once
// Start returns, ctx no longer matters: the proxy serves until Close, so its owner can keep it
// serving after ctx ends, as the launcher does through a child's stop grace when the container is
// told to stop.
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	if !filepath.IsAbs(cfg.Socket) {
		return nil, fmt.Errorf("sessionproxy: Socket %q is not an absolute path", cfg.Socket)
	}
	if !ompsessions.ValidInstance(cfg.Instance) {
		return nil, fmt.Errorf("sessionproxy: Instance %q is not the Cloud SQL instance's connection name, project:region:instance, or domain:project:region:instance for a domain-scoped project", cfg.Instance)
	}
	if cfg.ImpersonateServiceAccount != "" {
		if _, ok := ompsessions.ServiceAccountLogin(cfg.ImpersonateServiceAccount); !ok {
			return nil, fmt.Errorf("sessionproxy: ImpersonateServiceAccount %q is not a Google service account's email", cfg.ImpersonateServiceAccount)
		}
	}
	// The writer token is named whole or not at all, and only with the account it impersonates.
	switch {
	case cfg.WriterAudience != "" && cfg.WriterTokenFile == "":
		return nil, errors.New("sessionproxy: WriterAudience needs WriterTokenFile, the file of the token it is the audience of")
	case cfg.WriterTokenFile != "" && cfg.WriterAudience == "":
		return nil, errors.New("sessionproxy: WriterTokenFile needs WriterAudience, the audience of the token it holds")
	case cfg.WriterAudience != "" && cfg.ImpersonateServiceAccount == "":
		return nil, errors.New("sessionproxy: WriterAudience and WriterTokenFile need ImpersonateServiceAccount: the writer token signs in only by impersonating that service account")
	case cfg.WriterAudience != "" && !ompsessions.ValidWriterAudience(cfg.WriterAudience):
		return nil, fmt.Errorf("sessionproxy: WriterAudience %q is not a workload identity provider's full resource name, %s",
			cfg.WriterAudience, ompsessions.WriterAudienceForm)
	case cfg.WriterTokenFile != "" && !filepath.IsAbs(cfg.WriterTokenFile):
		return nil, fmt.Errorf("sessionproxy: WriterTokenFile %q is not an absolute path", cfg.WriterTokenFile)
	}
	if cfg.DialTimeout <= 0 {
		return nil, fmt.Errorf("sessionproxy: DialTimeout %s is not positive", cfg.DialTimeout)
	}
	if cfg.Drain < 0 {
		return nil, fmt.Errorf("sessionproxy: Drain %s is negative", cfg.Drain)
	}

	// An address another process holds is never taken from it: net.Listen refuses a path that
	// already exists, and that refusal is the whole start's failure.
	listener, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: listen on %s: %w", cfg.Socket, err)
	}

	newDialer := cfg.NewDialer
	if newDialer == nil {
		newDialer = newConnectorDialer
	}
	// The serving context outlives ctx: it carries ctx's values and nothing of its cancellation, and
	// only Close ends it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	dialer, err := newDialer(runCtx, Signer{
		Account: cfg.ImpersonateServiceAccount, WriterAudience: cfg.WriterAudience, WriterTokenFile: cfg.WriterTokenFile,
	})
	if err != nil {
		cancel()
		_ = listener.Close()
		return nil, fmt.Errorf("sessionproxy: build the dialer for %s: %w", cfg.Instance, err)
	}
	proof, timedOut, err := boundedDial(ctx, dialer, cfg.Instance, cfg.DialTimeout)
	if err != nil {
		cancel()
		_ = listener.Close()
		_ = dialer.Close()
		if timedOut {
			return nil, fmt.Errorf("sessionproxy: dial %s: no answer within %s: %w", cfg.Instance, cfg.DialTimeout, err)
		}
		return nil, fmt.Errorf("sessionproxy: dial %s: %w", cfg.Instance, err)
	}
	_ = proof.Close()
	// Once the start has succeeded, closing the listener never unlinks the socket. Go's unlink goes
	// by the socket's full path, and once the generation's agent runs it can replace a directory on
	// that path, its own g<n> among them, with a link to a sibling's, which would make the unlink
	// remove the sibling's socket. The socket's owner removes it with the directory instead, through
	// a walk that follows no link (the launcher's rolefile.RemoveTree). A refused start above still
	// unlinks: nothing of the generation runs yet, and the address must be free for the
	// generation's next start.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)

	p := &Proxy{
		instance:    cfg.Instance,
		listener:    listener,
		dialer:      dialer,
		admit:       cfg.Admit,
		cancel:      cancel,
		drain:       cfg.Drain,
		dialTimeout: cfg.DialTimeout,
		conns:       make(map[net.Conn]net.Conn),
		log:         cfg.Log,
	}
	// The start line names the identity and the credential that signs it in, and never a token.
	switch {
	case cfg.WriterAudience != "":
		p.logf("sessionproxy: serving %s on %s as %s with the launcher's writer token", cfg.Instance, cfg.Socket, cfg.ImpersonateServiceAccount)
	case cfg.ImpersonateServiceAccount != "":
		p.logf("sessionproxy: serving %s on %s as %s", cfg.Instance, cfg.Socket, cfg.ImpersonateServiceAccount)
	default:
		p.logf("sessionproxy: serving %s on %s", cfg.Instance, cfg.Socket)
	}
	p.inFlight.Add(1)
	go p.accept(runCtx)
	return p, nil
}

// Close stops accepting, releases the dialer, lets established connections finish for up to Drain,
// then closes those still open, and returns once every connection has ended. It is the only way a
// started proxy ends. It cancels every per-connection dial at once, so it waits on a client no
// longer than Drain so long as each dial honours that cancellation, as the Cloud SQL connector's
// does: a holder that never hangs up, inside the generation or outside it, cannot keep it from
// returning; a dial that ignored its cancelled context would hold Close until it returned. A second
// call is a no-op that returns nil. Close leaves the socket's file where it was, for its owner to
// remove with the directory that holds it (Start says why).
func (p *Proxy) Close() error {
	first := false
	p.closeOnce.Do(func() {
		first = true
		p.connsMu.Lock()
		p.closing = true
		p.connsMu.Unlock()
		err := p.listener.Close()
		if dialerErr := p.dialer.Close(); err == nil {
			err = dialerErr
		}
		p.cancel()

		drained := make(chan struct{})
		go func() {
			p.inFlight.Wait()
			close(drained)
		}()
		timer := time.NewTimer(p.drain)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			if n := p.closeConns(); n > 0 {
				p.logf("sessionproxy: closed %d connections to %s still open after %s", n, p.instance, p.drain)
			}
			<-drained
		}
		p.closeErr = err
	})
	if !first {
		return nil
	}
	return p.closeErr
}

// closeConns closes both ends of every connection the proxy is still copying for, which ends both
// of its copies and so its serve, and reports how many there were.
func (p *Proxy) closeConns() int {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	for client, upstream := range p.conns {
		_ = client.Close()
		_ = upstream.Close()
	}
	return len(p.conns)
}

// track registers a pair serve is about to copy for, so Close can close it. A pair that arrives
// once Close has begun, accepted or dialed just as it marked the proxy closing, is closed here
// instead and track reports false: nothing Close has stopped waiting to close can escape it.
func (p *Proxy) track(client, upstream net.Conn) bool {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	if p.closing {
		_ = client.Close()
		_ = upstream.Close()
		return false
	}
	p.conns[client] = upstream
	return true
}

// untrack forgets a pair whose copies have both ended.
func (p *Proxy) untrack(client net.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	delete(p.conns, client)
}

// accept serves one connection per accepted client until the listener is closed, which is the
// ordinary way this loop ends and so says nothing.
func (p *Proxy) accept(ctx context.Context) {
	defer p.inFlight.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.logf("sessionproxy: stopped accepting connections for %s: %v", p.instance, err)
			}
			return
		}
		p.inFlight.Add(1)
		go func() {
			defer p.inFlight.Done()
			p.serve(ctx, client)
		}()
	}
}

// serve gives one accepted connection an upstream of its own and copies between them. With an
// Admit, the connection's peer is checked first (admitted), so a refused connection costs no dial.
// The dial is per connection on purpose: the connector hands each connection a TLS session of its
// own under the instance's ephemeral certificate, which carries the login token. The connector
// refreshes that certificate inside a dial (lazy refresh, connectorOptions says why): the first
// dial, and the first one within four minutes of the certificate's expiry, mints a new one under
// its own context while it holds the connector's cache, so the dials queued behind it wait for
// that refresh; a refresh that fails fails the dial that ran it, with no fallback to the
// certificate it replaces, and the next dial tries again. Every other dial reuses the certificate.
// Each dial is bounded by DialTimeout, so a refresh that gets no answer ends with its dial, and a
// dial queued behind it ends by its own bound. serve already runs on a goroutine of its own, so
// only the other copy direction takes one. The pair is tracked from the dial's success until both
// copies end, so Close can close it once the drain is over; a pair dialed once Close has begun is
// closed at once and never copied for.
func (p *Proxy) serve(ctx context.Context, client net.Conn) {
	if p.admit != nil && !p.admitted(client) {
		return
	}
	upstream, timedOut, err := boundedDial(ctx, p.dialer, p.instance, p.dialTimeout)
	if err != nil {
		// Nothing is written back: a client that reads a Postgres error here would believe the
		// database answered it.
		_ = client.Close()
		if timedOut {
			p.logf("sessionproxy: could not dial %s for a connection: no answer within %s: %v", p.instance, p.dialTimeout, err)
			return
		}
		p.logf("sessionproxy: could not dial %s for a connection: %v", p.instance, err)
		return
	}
	if !p.track(client, upstream) {
		return
	}
	defer p.untrack(client)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	// Either direction ending closes both ends, which ends the other; serve returns only once both
	// have, so the connection is in flight for exactly as long as it is copied for.
	var copies sync.WaitGroup
	copies.Add(1)
	go func() {
		defer copies.Done()
		defer stop()
		_, _ = io.Copy(upstream, client)
	}()
	_, _ = io.Copy(client, upstream)
	stop()
	copies.Wait()
}

// boundedDial is dialer's Dial of instance under ctx, bounded by timeout, and whether that bound,
// and not ctx, ended it. The upstream a dial hands back outlives the bound: it bounds the dial
// alone.
func boundedDial(ctx context.Context, dialer Dialer, instance string, timeout time.Duration) (net.Conn, bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialer.Dial(dialCtx, instance)
	// dialCtx's own error is read before cancel, which would set it either way.
	timedOut := errors.Is(dialCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	return conn, timedOut, err
}

// admitted asks Admit about the process that made client. It closes a connection whose peer it
// cannot read, or that Admit refuses, and writes nothing back, as for a failed dial. It logs that
// connection in one line: the peer's pid, its command and the reason, or that the pid could not
// be read. The command is read before the close, while the peer most likely still runs. The line
// holds no token, because the proxy reads none from the peer.
func (p *Proxy) admitted(client net.Conn) bool {
	peer, err := readPeer(client)
	if err != nil {
		_ = client.Close()
		p.logf("sessionproxy: refused a connection to %s from a process whose pid could not be read: %v", p.instance, err)
		return false
	}
	if err := p.admit(peer); err != nil {
		command := peerCommand(peer.PID)
		_ = client.Close()
		p.logf("sessionproxy: refused a connection to %s from pid %d (%s): %v", p.instance, peer.PID, command, err)
		return false
	}
	return true
}

// peerCommand is the command name of the process pid as /proc/<pid>/comm holds it, quoted,
// because that process chooses the name and an unquoted name could break the log line. It is "?"
// when the name cannot be read, as when the process has already ended.
func peerCommand(pid int) string {
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%q", strings.TrimSuffix(string(comm), "\n"))
}

// logf writes one event line, under a lock because connections log concurrently.
func (p *Proxy) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.log, format+"\n", args...)
}

// connectorDialer adapts the Cloud SQL connector, whose Dial takes variadic dial options, to
// Dialer. Each Dial has its own scope: the connector hands back Admin API errors as text only, so
// the boundary reads them with the records of the exact requests that Dial sent.
type connectorDialer struct {
	dialer *cloudsqlconn.Dialer
}

func (c connectorDialer) Dial(ctx context.Context, instance string) (net.Conn, error) {
	s := oauth2call.Scope[net.Conn](ctx)
	return oauth2call.Call(s.Read(c.dialer.Dial(s.Context(), instance)))
}

func (c connectorDialer) Close() error {
	s := oauth2call.Scope[struct{}](context.Background())
	_, err := oauth2call.Call(struct{}{}, s.Err(c.dialer.Close()))
	return err
}

// newConnectorDialer builds the dialer a pod uses, with connectorOptions and Google's own
// endpoints. It signs in as the signer's account, or as the pod itself when the signer names none.
func newConnectorDialer(ctx context.Context, signer Signer) (Dialer, error) {
	opts, err := connectorOptions(ctx, signer, googleEndpoints{})
	if err != nil {
		return nil, err
	}
	s := oauth2call.Scope[*cloudsqlconn.Dialer](ctx)
	dialer, err := oauth2call.Call(s.Read(cloudsqlconn.NewDialer(s.Context(), opts...)))
	if err != nil {
		return nil, err
	}
	return connectorDialer{dialer: dialer}, nil
}

// The scopes of the two token sources the connector takes for an impersonated account. The
// documentation of cloudsqlconn.WithIAMAuthNTokenSources names them: the API client's source
// should have sqlservice.admin and cloud-platform, and the IAM login's source should only have
// sqlservice.login.
const (
	sqlAdminScope      = "https://www.googleapis.com/auth/sqlservice.admin"
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	sqlLoginScope      = "https://www.googleapis.com/auth/sqlservice.login"
)

// writerTokenEarlyRefresh is how long before its expiry the federated token is exchanged again.
// The token signs each generateAccessToken call, so with this margin no call that a slow network
// or a retry holds carries a token that runs out on the way.
const writerTokenEarlyRefresh = 5 * time.Minute

// googleCallTimeout bounds one request the proxy sends to Google: an exchange at the security
// token service, a generateAccessToken call at IAM Credentials, a token read of the pod's own
// credentials over x/oauth2, and a Cloud SQL Admin API call. A token source holds its lock through
// such a request and reads no dial's context: the library builds each source from the serving
// context (connectorOptions), so a dial's bound cannot end its request. One that never answered
// would hold the source, and every later dial's refresh behind it, for as long as the proxy
// serves, each of those dials failing at its own bound. An Admin API call runs under its dial's
// context (lazy refresh), and the client's bound holds it as well. The metadata endpoint bounds
// its exchange the same way (internal/launcher's exchangeTimeout); the metadata server a pod's own
// credentials read is bounded by its own client (cloud.google.com/go/compute/metadata's five
// seconds).
const googleCallTimeout = 30 * time.Second

// googleEndpoints is where connectorOptions reaches Google's token services, and how long one
// request to Google may take. The zero value is what a pod uses: the IAM Service Account
// Credentials API over http.DefaultTransport, the security token service at STSTokenURL, and
// googleCallTimeout. Only this package's tests set a field, to reach fakes.
type googleEndpoints struct {
	// iam carries every IAM Credentials API request, beneath the credential that signs it: the
	// federated token with a writer token, and the pod's own credentials (Application Default
	// Credentials) with an account alone (impersonationClient). Nil is http.DefaultTransport.
	iam http.RoundTripper
	// stsURL is the token URL of the writer token's exchange. "" is STSTokenURL.
	stsURL string
	// callTimeout bounds one request to Google (googleCallTimeout says which). 0 is
	// googleCallTimeout.
	callTimeout time.Duration
}

// timeout is the bound of one request to Google: callTimeout, or googleCallTimeout where it is 0.
func (e googleEndpoints) timeout() time.Duration { return cmp.Or(e.callTimeout, googleCallTimeout) }

// connectorOptions are the Cloud SQL connector's options for a pod. The connector uses automatic
// IAM database authentication, which puts a login token in the ephemeral-certificate exchange. It
// dials the instance's private IP, which is the only address a pod's network reaches. Its built-in
// client metrics, on by default since cloudsqlconn v1.26.0, are turned off: with them on,
// NewDialer reads the metadata server for the metrics' labels, every dialer exports to Cloud
// Monitoring each minute as the identity that signs the Admin API calls, which then needs
// roles/monitoring.metricWriter, and Close waits up to three seconds of the stop grace to flush.
//
// With no account, the connector signs in as the pod itself, with two scoped instances of the
// pod's credentials. With an account, a credential impersonates that account through the IAM
// Service Account Credentials API (impersonationClient). One impersonated source signs the Admin
// API calls, and the other gives the login token, so the proxy signs in as the account. Both
// sources go to the connector through cloudsqlconn.WithIAMAuthNTokenSources, so the connector
// itself reads no Application Default Credentials. The Admin API client is always a
// RefusalClient, and follows no redirect (redirects.Refused): the Admin API answers none a call
// must follow, and oauth2.Transport sets the call's bearer on every request it sends, so a
// followed one would carry the bearer to whatever host its Location names. A 3xx is googleapi's
// refusal by its status.
//
// The connector refreshes in the dial (cloudsqlconn.WithLazyRefresh), never in the background,
// because a refusal it hands back is googleapi's text alone: connectorDialer.Dial reads it under a
// scope of its own, whose record holds every request that dial sent, and the bearer of a refusal
// that echoes it is redacted by that record. A background refresh sends its Admin API calls under a
// context of the connector's own, which carries no scope, so the dial that hands back its error
// would print that error's words with no request's credential redacted
// (TestAnAdminAPIRefusalPrintsNoBearerItEchoes fails without this option). The cost, which serve
// describes, is that a refresh runs in a dial and holds the connector's cache while it does; so
// every dial is bounded (Config.DialTimeout), and every request to Google is bounded by
// endpoints.timeout() (googleCallTimeout): the Admin API client's, the IAM Credentials client's,
// the security token service's and the pod's own credentials' token reads.
//
// ctx is the proxy's serving context, which only Close ends. The library builds each source from
// the IAM Credentials client given it, and the federated source makes each exchange with it, so a
// credential that fetches its own token with that context keeps doing so for as long as the proxy
// serves; never Start's own context, which ends once the proof dial is over. Each source refreshes
// its one-hour token by itself when the connector next asks for one. endpoints is where Google's
// token services are and how long one request to Google may take: production passes the zero
// value, and a test its fakes.
func connectorOptions(ctx context.Context, signer Signer, endpoints googleEndpoints) ([]cloudsqlconn.Option, error) {
	opts := []cloudsqlconn.Option{
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
		cloudsqlconn.WithBuiltInMetrics(false),
		cloudsqlconn.WithLazyRefresh(),
	}
	account := signer.Account
	var api, login oauth2.TokenSource
	var err error
	if account == "" {
		api, err = podTokenSource(ctx, endpoints.timeout(), sqlAdminScope, cloudPlatformScope)
		if err != nil {
			return nil, err
		}
		login, err = podTokenSource(ctx, endpoints.timeout(), sqlLoginScope)
		if err != nil {
			return nil, err
		}
	} else {
		iam, err := impersonationClient(ctx, signer, endpoints)
		if err != nil {
			return nil, fmt.Errorf("impersonate %s: %w", account, err)
		}
		api, err = impersonatedSource(ctx, iam, account, sqlAdminScope, cloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("impersonate %s for the Admin API: %w", account, err)
		}
		login, err = impersonatedSource(ctx, iam, account, sqlLoginScope)
		if err != nil {
			return nil, fmt.Errorf("impersonate %s for the database login: %w", account, err)
		}
	}
	admin := oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, oauth2call.RefusalClient(redirects.Refused(&http.Client{Timeout: endpoints.timeout()}))), oauth2call.TokenSource(api))
	return append(opts,
		cloudsqlconn.WithHTTPClient(oauth2call.RefusalClient(admin)),
		cloudsqlconn.WithIAMAuthNTokenSources(oauth2call.TokenSource(api), oauth2call.TokenSource(login)),
	), nil
}

// podTokenSource is Application Default Credentials for scopes. FindDefaultCredentials is a
// text-only library call, so it runs under a scope; the returned source is bound to that scope and
// each Token reads the refusal it caused with the record of that Token's requests. A token read
// over x/oauth2 (a credential file's token endpoint) takes the context's client, which timeout
// bounds (googleCallTimeout) and which follows no redirect: a token endpoint answers none, and a
// 307 would send the read's form, with the credential in it (a signed assertion, a subject token),
// again to whatever host its Location names, so a 3xx is x/oauth2's refusal by its status. A read
// of the metadata server takes that library's own client.
func podTokenSource(ctx context.Context, timeout time.Duration, scopes ...string) (oauth2.TokenSource, error) {
	s := oauth2call.Scope[*google.Credentials](context.WithValue(ctx, oauth2.HTTPClient, oauth2call.RefusalClient(redirects.Refused(&http.Client{Timeout: timeout}))))
	credentials, err := oauth2call.Call(s.Read(google.FindDefaultCredentials(s.Context(), scopes...)))
	if err != nil {
		return nil, err
	}
	return credentials.TokenSource, nil
}

// impersonatedSource is a source of account's tokens for scopes, minted through iam, the IAM
// Credentials client (impersonationClient). CredentialsTokenSource is a text-only library call, so
// it runs under a scope of its own, which iam's requests are recorded in.
func impersonatedSource(ctx context.Context, iam *http.Client, account string, scopes ...string) (oauth2.TokenSource, error) {
	s := oauth2call.Scope[oauth2.TokenSource](ctx)
	return oauth2call.Call(s.Read(impersonate.CredentialsTokenSource(s.Context(), impersonate.CredentialsConfig{
		TargetPrincipal: account,
		Scopes:          scopes,
	}, option.WithHTTPClient(s.Client(iam)))))
}

// impersonationClient is the IAM Credentials client the impersonation calls generateAccessToken
// with: one client, whatever signs its calls (iamSource), each of its requests bounded by
// endpoints.timeout() (googleCallTimeout). The library calls its constant IAM Credentials
// endpoint and ignores option.WithEndpoint, so the client is the only way to give each call its
// credential. Its requests go out through a RefusalClient beneath the credential's transport, so
// each refused request's bearer is recorded in the source's scope and the boundary reads
// impersonate's text with it redacted. It follows no redirect (redirects.Refused): the service
// answers none a call must follow, and a followed one would carry the bearer, which
// oauth2.Transport sets on every request it sends, to whatever host its Location names. A 3xx is
// the library's refusal by its status. endpoints.iam is the transport beneath (nil is
// http.DefaultTransport).
func impersonationClient(ctx context.Context, signer Signer, endpoints googleEndpoints) (*http.Client, error) {
	source, err := iamSource(ctx, signer, endpoints)
	if err != nil {
		return nil, err
	}
	signed := &oauth2.Transport{
		Source: oauth2call.TokenSource(source),
		Base:   oauth2call.RefusalClient(&http.Client{Transport: endpoints.iam}).Transport,
	}
	return oauth2call.RefusalClient(redirects.Refused(&http.Client{Timeout: endpoints.timeout(), Transport: signed})), nil
}

// iamSource is the source that signs each IAM Credentials call, its token reused while it lives.
// Without a writer token it is the pod's own credentials (Application Default Credentials,
// cloud-platform). With a writer token, one federated source exchanges the token file's contents
// at the security token service for a federated token, refreshed writerTokenEarlyRefresh before
// it expires, and the pod's own credentials sign nothing; the federated source reads the file
// again at each exchange. The exchange takes its HTTP client from the context
// (oauth2.HTTPClient), as the metadata endpoint's does, so the bound and the record reach it
// there; it follows no redirect either, since a 307 would send the exchange's form, with the
// subject token in it, again. endpoints.stsURL is the exchange's token URL ("" is STSTokenURL).
func iamSource(ctx context.Context, signer Signer, endpoints googleEndpoints) (oauth2.TokenSource, error) {
	if signer.WriterAudience == "" {
		source, err := podTokenSource(ctx, endpoints.timeout(), cloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("read the pod's own credentials: %w", err)
		}
		return oauth2.ReuseTokenSource(nil, oauth2call.TokenSource(source)), nil
	}
	s := oauth2call.Scope[oauth2.TokenSource](ctx)
	exchange := context.WithValue(s.Context(), oauth2.HTTPClient, s.Client(redirects.Refused(&http.Client{Timeout: endpoints.timeout()})))
	federated, err := oauth2call.Call(s.Read(externalaccount.NewTokenSource(exchange, externalaccount.Config{
		Audience:         signer.WriterAudience,
		SubjectTokenType: JWTTokenType,
		TokenURL:         cmp.Or(endpoints.stsURL, STSTokenURL),
		Scopes:           []string{cloudPlatformScope},
		CredentialSource: &externalaccount.CredentialSource{File: signer.WriterTokenFile},
	})))
	if err != nil {
		return nil, fmt.Errorf("exchange the writer token: %w", err)
	}
	return oauth2.ReuseTokenSourceWithExpiry(nil, oauth2call.TokenSource(federated), writerTokenEarlyRefresh), nil
}
