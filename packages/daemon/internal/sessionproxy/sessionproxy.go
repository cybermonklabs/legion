// Package sessionproxy carries one generation's Oh My Pi session database traffic to Cloud SQL
// with the pod's own cloud credentials, so that no database password is ever written into a pod.
// The proxy signs in as the pod's own identity, or as a Google service account that the
// deployment names and that the pod's credentials impersonate (Config.ImpersonateServiceAccount).
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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// Dialer is the upstream the proxy dials for each accepted connection: the Cloud SQL connector in
// a pod, a fake over a local listener in a test. Close releases the dialer's own background work,
// the connector's refreshed certificate cache above all.
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
	// with the pod's own credentials and signs in as (ompsessions.ServiceAccountLogin says which
	// emails are one). Empty signs in as the pod's own identity.
	ImpersonateServiceAccount string
	// NewDialer builds the upstream dialer for account, which is ImpersonateServiceAccount. Nil
	// builds the Cloud SQL connector's: it dials the instance's private IP with automatic IAM
	// database authentication, and signs in as account, which the pod's own credentials
	// impersonate, or as the pod itself when account is "".
	NewDialer func(ctx context.Context, account string) (Dialer, error)
	// ProofTimeout bounds Start's proof dial, the one upstream connection that proves the
	// credential path before Start returns. It must be positive: an instance that never answers,
	// an unreachable address or a peer that stalls its handshake, would otherwise hold Start for as
	// long as its caller's context lives. The per-connection dials after Start are not bounded by it.
	ProofTimeout time.Duration
	// Drain is how long Close lets established connections finish on their own before it closes
	// them. Zero closes them at once; a negative Drain is refused. Close is bounded by it so long as
	// every per-connection dial honours cancellation, as the Cloud SQL connector's does: a client
	// that never hangs up then holds Close for Drain and no longer.
	Drain time.Duration
	// Log takes one line per event: the proxy's start, a connection the upstream refused, an
	// accept loop that stopped on its own, connections Close closed once the drain was over. Nil
	// logs nothing.
	Log io.Writer
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

	// cancel ends the proxy's own serving context, which the dialer was built with and every
	// per-connection dial uses, so the dialer's background work lives exactly as long as the proxy
	// and Close never waits on a dial that has nobody left to serve. Close is its only caller.
	cancel context.CancelFunc
	// inFlight counts the accept loop and every connection it is still copying for.
	inFlight sync.WaitGroup
	// drain is Config.Drain: how long Close waits for established connections before closing them.
	drain time.Duration

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
// leaves nothing bound. The proof dial is bounded besides by cfg.ProofTimeout, so a start against
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
	if cfg.ProofTimeout <= 0 {
		return nil, fmt.Errorf("sessionproxy: ProofTimeout %s is not positive", cfg.ProofTimeout)
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
	dialer, err := newDialer(runCtx, cfg.ImpersonateServiceAccount)
	if err != nil {
		cancel()
		_ = listener.Close()
		return nil, fmt.Errorf("sessionproxy: build the dialer for %s: %w", cfg.Instance, err)
	}
	proofCtx, cancelProof := context.WithTimeout(ctx, cfg.ProofTimeout)
	proof, err := dialer.Dial(proofCtx, cfg.Instance)
	// proofCtx's own error is read before cancelProof, which would set it either way.
	timedOut := errors.Is(proofCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelProof()
	if err != nil {
		cancel()
		_ = listener.Close()
		_ = dialer.Close()
		if timedOut {
			return nil, fmt.Errorf("sessionproxy: dial %s: no answer within %s: %w", cfg.Instance, cfg.ProofTimeout, err)
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
		instance: cfg.Instance,
		listener: listener,
		dialer:   dialer,
		cancel:   cancel,
		drain:    cfg.Drain,
		conns:    make(map[net.Conn]net.Conn),
		log:      cfg.Log,
	}
	if cfg.ImpersonateServiceAccount != "" {
		p.logf("sessionproxy: serving %s on %s as %s", cfg.Instance, cfg.Socket, cfg.ImpersonateServiceAccount)
	} else {
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

// serve gives one accepted connection an upstream of its own and copies between them. The dial is
// per connection on purpose: the connector mints the connection's credential as it dials, from a
// certificate cache it keeps refreshed, so every connection carries a freshly minted one. serve
// already runs on a goroutine of its own, so only the other copy direction takes one. The pair is
// tracked from the dial's success until both copies end, so Close can close it once the drain is
// over; a pair dialed once Close has begun is closed at once and never copied for.
func (p *Proxy) serve(ctx context.Context, client net.Conn) {
	upstream, err := p.dialer.Dial(ctx, p.instance)
	if err != nil {
		// Nothing is written back: a client that reads a Postgres error here would believe the
		// database answered it.
		_ = client.Close()
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

// newConnectorDialer builds the dialer a pod uses, with connectorOptions. It signs in as account,
// which the pod's own credentials impersonate, or as the pod itself when account is "".
func newConnectorDialer(ctx context.Context, account string) (Dialer, error) {
	opts, err := connectorOptions(ctx, account, nil)
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

// connectorOptions are the Cloud SQL connector's options for a pod. The connector uses automatic
// IAM database authentication, which puts a login token in the ephemeral-certificate exchange. It
// dials the instance's private IP, which is the only address a pod's network reaches.
//
// With account "", the connector signs in as the pod itself, with two scoped instances of the
// pod's credentials. With an account, the pod's credentials impersonate that account through the
// IAM Service Account Credentials API. One source signs the Admin API calls, and the other gives
// the login token, so the proxy signs in as the account. The Admin API client is always a
// RefusalClient, and lazy refresh makes its requests run under the Dial context that reads their
// error, instead of a background context with no record.
//
// ctx is the proxy's serving context, which only Close ends. The library builds each source from
// the IAM Credentials client given it, so a credential that fetches its own token with that
// context keeps doing so for as long as the proxy serves; never Start's own context, which ends
// once the proof dial is over. Each source refreshes its one-hour token by itself when the
// connector next asks for one. iamBase is the transport the IAM Credentials client's requests go
// out on: production passes nil (http.DefaultTransport), and a test a fake endpoint.
func connectorOptions(ctx context.Context, account string, iamBase http.RoundTripper) ([]cloudsqlconn.Option, error) {
	opts := []cloudsqlconn.Option{
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
		cloudsqlconn.WithLazyRefresh(),
	}
	var api, login oauth2.TokenSource
	var err error
	if account == "" {
		api, err = podTokenSource(ctx, sqlAdminScope, cloudPlatformScope)
		if err != nil {
			return nil, err
		}
		login, err = podTokenSource(ctx, sqlLoginScope)
		if err != nil {
			return nil, err
		}
	} else {
		iam, err := impersonationClient(ctx, iamBase)
		if err != nil {
			return nil, fmt.Errorf("impersonate %s: read the pod's own credentials: %w", account, err)
		}
		adminScope := oauth2call.Scope[oauth2.TokenSource](ctx)
		api, err = oauth2call.Call(adminScope.Read(impersonate.CredentialsTokenSource(adminScope.Context(), impersonate.CredentialsConfig{
			TargetPrincipal: account,
			Scopes:          []string{sqlAdminScope, cloudPlatformScope},
		}, option.WithHTTPClient(adminScope.Client(iam)))))
		if err != nil {
			return nil, fmt.Errorf("impersonate %s for the Admin API: %w", account, err)
		}
		loginScope := oauth2call.Scope[oauth2.TokenSource](ctx)
		login, err = oauth2call.Call(loginScope.Read(impersonate.CredentialsTokenSource(loginScope.Context(), impersonate.CredentialsConfig{
			TargetPrincipal: account,
			Scopes:          []string{sqlLoginScope},
		}, option.WithHTTPClient(loginScope.Client(iam)))))
		if err != nil {
			return nil, fmt.Errorf("impersonate %s for the database login: %w", account, err)
		}
	}
	admin := oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, oauth2call.RefusalClient(nil)), oauth2call.TokenSource(api))
	return append(opts,
		cloudsqlconn.WithHTTPClient(oauth2call.RefusalClient(admin)),
		cloudsqlconn.WithIAMAuthNTokenSources(oauth2call.TokenSource(api), oauth2call.TokenSource(login)),
	), nil
}

// podTokenSource is Application Default Credentials for scopes. FindDefaultCredentials is a
// text-only library call, so it runs under a scope; the returned source is bound to that scope and
// each Token reads the refusal it caused with the record of that Token's requests.
func podTokenSource(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
	s := oauth2call.Scope[*google.Credentials](ctx)
	credentials, err := oauth2call.Call(s.Read(google.FindDefaultCredentials(s.Context(), scopes...)))
	if err != nil {
		return nil, err
	}
	return credentials.TokenSource, nil
}

// impersonationClient is the IAM Credentials client the impersonation calls generateAccessToken
// with, signed with the pod's own credentials (Application Default Credentials, cloud-platform).
// Its requests go out through a RefusalClient beneath the credential's transport, so each refused
// request's bearer is recorded in the source's scope and the boundary reads impersonate's text
// with it redacted.
func impersonationClient(ctx context.Context, base http.RoundTripper) (*http.Client, error) {
	source, err := podTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return nil, err
	}
	return oauth2call.RefusalClient(oauth2.NewClient(
		context.WithValue(ctx, oauth2.HTTPClient, oauth2call.RefusalClient(&http.Client{Transport: base})),
		oauth2call.TokenSource(source),
	)), nil
}
