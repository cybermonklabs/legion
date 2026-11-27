package natscred

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// ServiceUser is the CONNECT user a service presents beside its platform identity token as the
// password, which is how the auth service tells a service's credential from a session's.
const ServiceUser = "service"

// ServiceRenewBefore is how long before the token it connected with lapses a service's connection
// moves to a later one: the auth service answers a service's user with its token's expiry, and a
// projected Kubernetes token is rewritten by the kubelet at 80% of its life, a Google identity
// token minted fresh on each read, so a later one is there to read well before then.
const ServiceRenewBefore = 10 * time.Minute

// servicePoll is how often a service's Keeper, once its token is due, asks its reader for a later
// one and tries it, and checks that its reconnect has presented it.
const servicePoll = time.Second

// serviceProbeCutoff is how long before the presented token lapses a service's Keeper stops
// trying to move its connection to a later one: past it, the connection keeps the user it has,
// the server drops it when that user expires, and nats.go reconnects with whatever token the
// reader answers then, retrying a refusal (IgnoreAuthErrorAbort).
const serviceProbeCutoff = 30 * time.Second

// TokenReader reads a service's platform identity token as it is now, for each CONNECT.
type TokenReader interface {
	Read() (string, error)
}

// Service is the credential of a service: user ServiceUser and, as the password, the platform
// identity token reader answers at each CONNECT, the auth service verifying it and answering the
// grant BROKER_NATS_SERVICES lists its account for. The option sets IgnoreAuthErrorAbort: a
// refused token is retried, not given up on, so a service whose token the hub refused while the
// broker was down connects again once it answers. A token reader cannot read is sent as no
// password, which the auth service refuses and the client retries.
//
// The Keeper reads the expiry of the token the connection presented (unverified, its 'exp') and,
// ServiceRenewBefore ahead of it, waits for reader to answer a token that lapses later. It then
// dials a throwaway connection (a probe) with that token, and reconnects the connection only once
// a probe is admitted: while the auth service is down, a reconnect would be refused and the
// connection cut until the service answers again, though its user has minutes left to live. A
// probe refused or failing is tried again every servicePoll until serviceProbeCutoff before the
// presented token lapses; past that, the connection is left as it is until the server drops it
// at expiry. Once it has reconnected, the Keeper waits for that reconnect to present a token
// before it reads the next expiry.
func Service(reader TokenReader) (nats.Option, Keeper) {
	credential := &serviceCredential{reader: reader}
	return allOf(nats.UserInfoHandler(credential.userInfo), nats.IgnoreAuthErrorAbort()), credential
}

// serviceCredential is Service's state: the reader and the token the last CONNECT presented.
type serviceCredential struct {
	reader TokenReader

	mu        sync.Mutex
	presented string
}

// userInfo is the CONNECT's user and password: ServiceUser and the token reader answers now.
func (c *serviceCredential) userInfo() (string, string) {
	token, err := c.reader.Read()
	if err != nil {
		token = ""
	}
	c.mu.Lock()
	c.presented = token
	c.mu.Unlock()
	return ServiceUser, token
}

func (c *serviceCredential) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presented
}

// Keep moves nc to a later token ServiceRenewBefore ahead of each one it presented, reconnecting
// it only once a probe with that token is admitted, until ctx ends or nc closes. Past
// serviceProbeCutoff before the presented token lapses it stops trying for that token, and waits
// for the reconnect nats.go makes when the server drops the connection at expiry.
func (c *serviceCredential) Keep(ctx context.Context, nc *nats.Conn) {
	ctx, stop := untilClosed(ctx, nc)
	defer stop()
	for {
		presented := c.token()
		expiry, err := expiryOf(presented)
		wait := servicePoll
		if err == nil {
			wait = time.Until(expiry.Add(-ServiceRenewBefore))
		}
		if !sleep(ctx, wait) || nc.IsClosed() {
			return
		}
		if err != nil || c.token() != presented {
			// Nothing readable was presented yet, or a reconnect of nats.go's own presented
			// another token while this waited: read the expiry again.
			continue
		}
		admitted, live := c.admitLater(ctx, nc, presented, expiry)
		if !live {
			return
		}
		if admitted && nc.ForceReconnect() != nil {
			return
		}
		// Wait for the connection to present another token: the reconnect just made, or, past
		// the cutoff, the one nats.go makes once the server drops the connection at expiry.
		for c.token() == presented {
			if !sleep(ctx, servicePoll) || nc.IsClosed() {
				return
			}
		}
	}
}

// admitLater waits, asking the reader every servicePoll, for a token that lapses after expiry
// and that a probe is admitted with, and reports whether one was. It gives up, reporting false,
// at serviceProbeCutoff before expiry, and when the connection presents a token other than
// presented on a reconnect of nats.go's own. live is false when ctx ended or nc closed first.
func (c *serviceCredential) admitLater(ctx context.Context, nc *nats.Conn, presented string, expiry time.Time) (admitted, live bool) {
	cutoff := expiry.Add(-serviceProbeCutoff)
	for time.Now().Before(cutoff) && c.token() == presented {
		if token, err := c.reader.Read(); err == nil {
			if later, err := expiryOf(token); err == nil && later.After(expiry) && probe(nc, token) == nil {
				return true, true
			}
		}
		if !sleep(ctx, servicePoll) || nc.IsClosed() {
			return false, false
		}
	}
	return false, true
}

// probe dials a throwaway connection where nc connects, presenting token as the password of user
// ServiceUser, and closes it: nil when the server admitted it, which for a service means the auth
// service answers and admits token now.
//
// It reaches the server as nc does, through the fields of nc's options that say where and how:
// its servers, name, TLS, dialer, websocket proxy path and headers, and timeout. It reads them as
// they are when it runs, since by then they are final whatever applied them: internal/bus sets
// the SOCKS5 proxy's dialer, its host lookup and a bounded timeout after the credential's option
// (connect.go, connectWithContext), so a copy of the options taken when that option was applied
// would dial the hub around the proxy. It reads no other field, and never copies nc.Opts whole:
// nats.go writes a running connection's options only through its Set*Handler methods, under a
// lock this package cannot take, and internal/bus calls two of them on a replacement connection
// it discards (nats.go, ensureConnWithContext), so a whole copy would race with those writes and
// these field reads do not.
//
// Everything else is nats.go's default, so the probe presents no other credential, never
// reconnects, and has no callbacks: nc's ConnectedCB starts a Keeper beside the connection it
// makes (internal/bus, keepAlongside), and its handlers would report the probe's connect and close
// as nc's own.
func probe(nc *nats.Conn, token string) error {
	from := &nc.Opts
	opts := nats.GetDefaultOptions()
	opts.Url, opts.Servers, opts.NoRandomize, opts.IgnoreDiscoveredServers = from.Url, from.Servers, from.NoRandomize, from.IgnoreDiscoveredServers
	opts.InProcessServer = from.InProcessServer
	opts.Name = strings.TrimSpace(from.Name + " probe")
	opts.Secure, opts.TLSConfig, opts.TLSHandshakeFirst = from.Secure, from.TLSConfig, from.TLSHandshakeFirst
	opts.TLSCertCB, opts.RootCAsCB = from.TLSCertCB, from.RootCAsCB
	opts.Timeout, opts.Dialer, opts.CustomDialer, opts.SkipHostLookup = from.Timeout, from.Dialer, from.CustomDialer, from.SkipHostLookup
	opts.ProxyPath, opts.Compression = from.ProxyPath, from.Compression
	opts.WebSocketConnectionHeaders, opts.WebSocketConnectionHeadersHandler = from.WebSocketConnectionHeaders, from.WebSocketConnectionHeadersHandler
	opts.User, opts.Password = ServiceUser, token
	opts.AllowReconnect = false
	conn, err := opts.Connect()
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// sleep waits d, reporting false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ServiceFile is the token in the file at path, read afresh at every Read: a projected Kubernetes
// ServiceAccount token, which the kubelet rewrites in place. setting is what the caller read path
// from (a variable or a flag), which a failed Read names: a file that could not be read is named
// by setting and the system's reason alone (redact.File), never by path or by the error's own
// text, which repeats it, since path is where a person who pasted the token itself put it. A file
// that was read, and is empty, is named by its path too.
func ServiceFile(path, setting string) TokenReader {
	return serviceFile{path: path, setting: setting}
}

type serviceFile struct {
	path, setting string
}

func (f serviceFile) Read() (string, error) {
	contents, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("natscred: %s names a service token file that could not be read: %w", f.setting, redact.File(err))
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("natscred: the service token file %s, which %s names, is empty", f.path, f.setting)
	}
	return token, nil
}

// googleReadTimeout bounds one read of the metadata server.
const googleReadTimeout = 10 * time.Second

// ServiceGoogle is this VM's Google identity token for audience, minted by its metadata server at
// every Read (googleidentity.Token). setting is what the caller read audience from (a variable or
// a flag), which a failed Read names: the audience is a value a person can paste a credential
// into, so it prints whole only where internal/redact's allow-list prints it, a plain http(s) URL,
// and by setting alone otherwise.
func ServiceGoogle(audience, setting string) TokenReader {
	return serviceGoogle{audience: audience, setting: setting}
}

type serviceGoogle struct {
	audience, setting string
}

func (g serviceGoogle) Read() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), googleReadTimeout)
	defer cancel()
	token, err := googleidentity.Token(ctx, g.audience)
	if err != nil {
		return "", fmt.Errorf("natscred: read %s: %w", g.describe(), err)
	}
	return token, nil
}

// describe names the token Read reads by the setting its audience came from, never
// AGENT_SECRETS_MACHINE_AUDIENCE, the setting of agent-secrets' own Google identity.
func (g serviceGoogle) describe() string {
	audience := "the audience " + g.setting + " names"
	if redact.Clean(g.audience) {
		audience = g.audience + " (" + g.setting + ")"
	}
	return "this machine's Google identity token for " + audience + " (" + googleidentity.MetadataHostVar + " names the metadata server)"
}
