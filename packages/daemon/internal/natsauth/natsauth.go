// Package natsauth resolves the NATS credential a Legion process connects to Envoy's NATS with (an
// nkey user's seed, or the daemon's platform identity token), and connects with it.
package natsauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/tlsalert"
)

// ServiceTokenFileKey is the configuration key naming the file of the platform identity token the
// daemon's own connection presents to a hub whose connections the NATS auth service admits.
const ServiceTokenFileKey = "nats_daemon_service_token_file"

// SeedFileKey is the configuration key naming the seed's file, in `legion.yaml` and the operator's
// controller configuration alike. SeedFileVariable and SeedVariable are the two variables a process
// names it with otherwise: a file holding the seed, or the seed itself. The Envoy clients read the
// same two (packages/envoy/internal/bus/nkey.go), and SeedVariable is also the name of the launch
// secret every pane receives the seed as, behind a SeedFileVariable pointer.
const (
	SeedFileKey      = "nats_nkey_seed_file"
	SeedFileVariable = "NATS_NKEY_SEED_FILE"
	SeedVariable     = "NATS_NKEY_SEED"
)

// DaemonSeedFileKey, DaemonSeedFileVariable and DaemonSeedVariable name the seed the daemon's own
// connection authenticates with, the same three ways: a `legion.yaml` key, a variable naming a file,
// and a variable holding the seed. No pane is ever handed it; unset, the daemon connects as the pane
// seed.
const (
	DaemonSeedFileKey      = "nats_daemon_nkey_seed_file"
	DaemonSeedFileVariable = "NATS_DAEMON_NKEY_SEED_FILE"
	DaemonSeedVariable     = "NATS_DAEMON_NKEY_SEED"
)

// names are the three names one seed is read under: the configuration key naming its file, the
// variable naming its file, and the variable holding it.
type names struct{ fileKey, fileVariable, variable string }

var (
	paneSeed   = names{SeedFileKey, SeedFileVariable, SeedVariable}
	daemonSeed = names{DaemonSeedFileKey, DaemonSeedFileVariable, DaemonSeedVariable}
)

// Configured reports whether Seed reads a seed at all: file (the SeedFileKey's, "" when the
// configuration names none) is set, or either variable is, whatever it holds.
func Configured(file string, lookup func(string) (string, bool)) bool {
	_, ok := resolve(paneSeed, file, lookup, nil)
	return ok
}

// Seed is the nkey seed of the NATS user a process connects as: the trimmed contents of file, the
// file the configuration's SeedFileKey names ("" when it names none); else those of the file
// NATS_NKEY_SEED_FILE names; else NATS_NKEY_SEED. The first source set is authoritative: an empty
// NATS_NKEY_SEED_FILE, or a missing, unreadable, or blank file, or one whose mode lets more than
// its owner read it, is an error naming the key or variable and the path, never a fallback to the
// next source or to no credential; so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey
// seed. The file's group may read it only when root owns it and the reader is not root
// (config.ReadGroupSecretPointer): a non-root process in a pod — the daemon, or `legion
// probe-image` in the probe pod — reads a kubelet-mounted Secret file, root's, through the pod's
// fsGroup. None set is "": the connection carries no credential, as every connection did before
// servers required one. No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func Seed(file string, lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve(paneSeed, file, lookup, config.ReadGroupSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// DaemonSeed is Seed of the daemon's own seed, under its own names: the file the configuration's
// DaemonSeedFileKey names ("" when it names none), else the file NATS_DAEMON_NKEY_SEED_FILE names,
// else NATS_DAEMON_NKEY_SEED, with Seed's refusals and file mode rule. None set is "": the daemon
// then connects as the pane seed. It never falls back to the pane seed's names, nor they to it.
func DaemonSeed(file string, lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve(daemonSeed, file, lookup, config.ReadGroupSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// SeedFile is Seed of the file the configuration's SeedFileKey names, alone, refused when its group
// or others can read it (config.ReadPrivateSecretPointer): what `legion controller start` checks,
// on the operator's own machine, before handing its Oh My Pi that file.
func SeedFile(file string) (string, error) {
	return fileSource(paneSeed.fileKey, file, config.ReadPrivateSecretPointer).seed()
}

// source is one place a seed is read from: its name, which every refusal carries, and read, which
// answers the seed or why the source holds none.
type source struct {
	name string
	read func() (string, error)
}

// resolve is the one precedence every reader of a seed shares, under n's names: file when the
// configuration names one, else the file variable when set, else the seed variable when set,
// whatever each holds; ok is false when none is. read reads a file source (the key or variable
// naming it, and its path).
func resolve(n names, file string, lookup func(string) (string, bool), read func(key, path string) (string, error)) (source, bool) {
	if file != "" {
		return fileSource(n.fileKey, file, read), true
	}
	if path, set := lookup(n.fileVariable); set {
		if path == "" {
			return source{n.fileVariable, func() (string, error) {
				return "", fmt.Errorf("%s is set but empty", n.fileVariable)
			}}, true
		}
		return fileSource(n.fileVariable, path, read), true
	}
	if value, set := lookup(n.variable); set {
		return source{n.variable, func() (string, error) {
			if seed := strings.TrimSpace(value); seed != "" {
				return seed, nil
			}
			return "", fmt.Errorf("%s is set but empty", n.variable)
		}}, true
	}
	return source{}, false
}

// fileSource is the file path that key (a configuration key or a variable) names, which read reads.
func fileSource(key, path string, read func(key, path string) (string, error)) source {
	return source{fmt.Sprintf("%s (%s)", key, path), func() (string, error) { return read(key, path) }}
}

// seed is the source's seed, refused unless it is a user's nkey seed.
func (s source) seed() (string, error) {
	seed, err := s.read()
	if err != nil {
		return "", err
	}
	if _, err := userKey(seed, s.name); err != nil {
		return "", err
	}
	return seed, nil
}

// Connect dials urls as the nkey user seed is the seed of, a seed Seed answered; "" connects
// without a credential. Every connection it opens pings as detectSilentCuts sets, because the
// daemon dials NATS here and nowhere else: the pings are this package's, not a caller's to pass.
//
// name is what the connection calls itself to the server, and it is a parameter rather than an
// option because an option can be forgotten and an unnamed connection is one nobody can place:
// an operator reading /connz on the Envoy NATS sees which process holds each connection, and a
// test fixture reporting a connection a test left open can say which client it was. It is
// appended after the caller's own options, beside detectSilentCuts, so a caller's own nats.Name
// does not win here.
//
// It leaves nats.go's IgnoreAuthErrorAbort at its default: a seed's user is one a hub admits by its
// own configuration, never asking the NATS auth service, and the auth service admits no connection
// with no credential, so the same refusal twice is a refusal for good and the connection closes
// (TestADaemonSeedRefusedTwiceCloses). The credential the auth service decides is
// ConnectService's, which retries a refusal.
func Connect(urls []string, seed string, name string, options ...nats.Option) (*nats.Conn, error) {
	if seed != "" {
		user, err := userKey(seed, "the NATS nkey seed")
		if err != nil {
			return nil, err
		}
		public, err := user.PublicKey()
		if err != nil {
			return nil, err
		}
		options = append(options, nats.Nkey(public, user.Sign))
	}
	// Last, so these pings and this name hold whatever a caller passed before them.
	options = append(options, detectSilentCuts, nats.Name(name))
	return nats.Connect(strings.Join(urls, ","), options...)
}

// ServiceIdentity is the account the platform identity token in the file at tokenFile names (its
// subject, which for a projected Kubernetes token is system:serviceaccount:<namespace>:<name>),
// read unverified, as natscred.Identity reads it: what the daemon says it connects as. The file is
// refused, naming ServiceTokenFileKey and the path, when it cannot be read, is empty, or holds no
// token that names an account. No error carries the token.
func ServiceIdentity(tokenFile string) (string, error) {
	token, err := natscred.ServiceFile(tokenFile).Read()
	if err != nil {
		return "", fmt.Errorf("%s names %s, whose token could not be read: %w", ServiceTokenFileKey, tokenFile, err)
	}
	identity, err := natscred.Identity(token)
	if err != nil {
		return "", fmt.Errorf("%s names %s, whose token names no account: %w", ServiceTokenFileKey, tokenFile, err)
	}
	return identity, nil
}

// ConnectService dials urls as the service the platform identity token in the file at tokenFile
// names: user natscred.ServiceUser and that token, read afresh at each CONNECT (natscred.Service),
// which the NATS auth service verifies and answers with the grant its BROKER_NATS_SERVICES entry
// lists for the account (kind daemon). It pings and names itself as Connect does.
//
// Three things a seed's connection does not do:
//   - It sets nats.go's IgnoreAuthErrorAbort (natscred.Service does): a hub refuses every client the
//     auth service decides while that service is away or does not yet hold an answer the decision
//     needs, so this connection retries a refusal rather than closing after the same one twice (as
//     a seed's connection does, TestADaemonSeedRefusedTwiceCloses).
//   - Its reply inboxes are natsgrant.InboxPrefix of the account the token names, the one reply
//     prefix the auth service grants this account in place of _INBOX.>, so no other client's
//     replies reach it.
//   - It moves to a later token natscred.ServiceRenewBefore before the one it presented lapses
//     (natscred.Keeper), since the user the auth service answers expires with the token; the keeper
//     runs until the connection closes.
func ConnectService(urls []string, tokenFile, name string, options ...nats.Option) (*nats.Conn, error) {
	identity, err := ServiceIdentity(tokenFile)
	if err != nil {
		return nil, err
	}
	credential, keeper := natscred.Service(natscred.ServiceFile(tokenFile))
	// Last, as Connect's, so this credential, prefix, pings and name hold whatever a caller passed.
	options = append(options, credential, nats.CustomInboxPrefix(natsgrant.InboxPrefix(identity)), detectSilentCuts, nats.Name(name))
	conn, err := nats.Connect(strings.Join(urls, ","), options...)
	if err != nil {
		return nil, err
	}
	// The keeper ends when the connection closes, the daemon's own Close included. nats.go sends
	// CLOSED to a status listener registered before the close, into a buffered channel, so the
	// event is kept even when it comes before the goroutine waits; a close before the listener is
	// registered sends nothing, which the IsClosed read after the registration catches.
	closed := conn.StatusChanged(nats.CLOSED)
	keep, stop := context.WithCancel(context.Background())
	go func() {
		defer stop()
		if conn.IsClosed() {
			return
		}
		<-closed
	}()
	go keeper.Keep(keep, conn)
	return conn, nil
}

// ReconnectForever is the connection option that never gives up reconnecting. nats.go's own
// default policy (60 attempts, nats.DefaultMaxReconnect, about two minutes at
// nats.DefaultReconnectWait) closes the connection for good once it gives up; with MaxReconnect
// negative, nats.go's own server-pool logic never drops a server for having failed too many times
// (selectNextServer) and doReconnect loops until Close, so a NATS outage mid-run costs degraded
// minutes of nats.go's own default 2 s reconnect wait rather than a closed connection (LEGION-580).
func ReconnectForever() nats.Option {
	return nats.MaxReconnects(-1)
}

// silentCutPingInterval and silentCutMaxPingsOut are the pings of detectSilentCuts. nats.go counts
// a ping out at each interval and declares the connection stale on the interval that would put a
// third one out, and any PONG clears the count, so a server that stops answering is found two to
// three seconds later, while a PONG may still take two seconds before a loaded server is taken for
// a cut one. The daemon's ReconnectForever makes that mistake cheap: a false cut costs a redial,
// never a closed connection.
const (
	silentCutPingInterval = time.Second
	silentCutMaxPingsOut  = 2
)

// detectSilentCuts is the connection option, set by Connect itself, that finds a server which
// stops answering without closing the connection: packets that stop, with no reset. nats.go's own
// defaults (a ping every two minutes, two outstanding) leave the daemon reading connected for
// minutes into such a cut, and the daemon reads nothing in those minutes. A connection declared
// lost here reconnects as one whose server closed it does.
func detectSilentCuts(o *nats.Options) error {
	o.PingInterval = silentCutPingInterval
	o.MaxPingsOut = silentCutMaxPingsOut
	return nil
}

// permissionRefusal is the operation and subject a server's permissions violation names
// (`Permissions Violation for Subscription to "<subject>"`, or `… Publish to …`).
var permissionRefusal = regexp.MustCompile(`(?i)(publish|subscription) to "([^"]+)"`)

// LogEvents is the connection option that logs what the server reports about the connection
// asynchronously: at error, every permission the server refuses it, a subscription or a publish,
// the JetStream API requests a consumer makes included, and a terminal close (a fatal server -ERR,
// or — for a caller that has not also set ReconnectForever — reconnects exhausted), once, with
// its cause; at warn, every refusal of the connection's credential on a reconnect (Refused), with
// its reason, every other asynchronous error, with the subject of the subscription it names, and
// every disconnect, with its cause; at info, every reconnect, with the server. nats.go's
// default handler writes a refusal to stderr unlabelled and nothing for the rest. It owns the
// connection's AsyncErrorCB, DisconnectedErrCB, ReconnectedCB and ClosedCB: an option after it
// that sets one replaces its handler. It also sets NoCallbacksAfterClientClose, so the daemon's
// own Close runs neither of the last two.
func LogEvents(log *slog.Logger) nats.Option {
	return func(o *nats.Options) error {
		o.AsyncErrorCB = func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if Refused(err) {
				// nats.go reports each refusal of a reconnect's CONNECT here: a seed's connection
				// closes after the same one twice, the service token's tries again (ConnectService).
				log.Warn("NATS refused the connection's credential", "error", err)
				return
			}
			if !errors.Is(err, nats.ErrPermissionViolation) {
				subject := ""
				if sub != nil {
					subject = sub.Subject
				}
				log.Warn("NATS reported an asynchronous error", "subject", subject, "error", err)
				return
			}
			operation, subject := "", ""
			if m := permissionRefusal.FindStringSubmatch(err.Error()); m != nil {
				operation, subject = strings.ToLower(m[1]), m[2]
			}
			log.Error("NATS refused the daemon a permission: its NATS user lacks that grant", "operation", operation, "subject", subject, "error", err)
		}
		// A nil cause is a terminal close, which ClosedCB logs with its cause.
		o.DisconnectedErrCB = func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("NATS connection lost", "error", err)
			}
		}
		o.ReconnectedCB = func(c *nats.Conn) {
			log.Info("NATS connection restored", "server", c.ConnectedUrlRedacted())
		}
		// The daemon's own Close runs no callback (NoCallbacksAfterClientClose), so a close here is
		// terminal and its cause is the connection's LastError. A permission refusal or a slow
		// consumer also sets LastError and leaves it set, but only while the connection is up.
		o.NoCallbacksAfterClientClose = true
		o.ClosedCB = func(c *nats.Conn) {
			if err := c.LastError(); err != nil {
				log.Error("NATS connection closed", "error", err)
			}
		}
		return nil
	}
}

// WithLastError is err naming the last error of conn, when conn is closed: the cause of a terminal
// close (a fatal server -ERR, or — for a caller that has not also set ReconnectForever —
// reconnects exhausted) that nats.go hands no handler. It is err itself while conn is up, whose
// LastError may be an earlier asynchronous error (a permission refusal, a slow consumer) that
// caused nothing, and when a closed conn has none. The join is %w, not %v: a caller that closes
// conn right after a failed call and then wraps with WithLastError can still match the refusal
// with errors.Is (nats.ErrPermissionViolation, say), since Close does not clear LastError.
func WithLastError(err error, conn *nats.Conn) error {
	if !conn.IsClosed() {
		return err
	}
	if last := conn.LastError(); last != nil {
		return fmt.Errorf("%w (the NATS connection's last error: %w)", err, last)
	}
	return err
}

// Unreachable reports whether err means NATS is not reachable yet, as against a misconfiguration
// — a permission or an authorization violation the server itself refuses, a TLS alert, on the
// handshake or on a later record (tlsalert.In: a server that demands a client certificate, say, or
// speaks no TLS version this client does), or a malformed seed (userKey, before any dial is
// attempted) — that no wait fixes. The alert is read before the dial's shape because crypto/tls
// returns it as a *net.OpError too, which would otherwise read as a NATS not up yet and be waited
// out forever.
// Matched by concrete, named shapes only, never the net.Error interface: two unrelated
// standard library types satisfy that interface without being network failures at all —
// context.DeadlineExceeded and *url.Error — so a caller must never classify by the interface
// alone. A permission violation reports its cause asynchronously and never closes the
// connection, so the blocked call's own synchronous error is a bare context.DeadlineExceeded with
// no sign of it, until a caller folds the connection's own LastError in first
// (natsauth.WithLastError) — the errors.Is check above is what then recognizes it by name. A
// bare context.DeadlineExceeded with nothing joined is refused by the same default a genuinely
// unanswered call takes (a clustered JetStream that is itself unavailable, say): this package has
// no way to tell that shape from an unjoined permission violation, so both are refused rather
// than waited on.
func Unreachable(err error) bool {
	if errors.Is(err, nats.ErrPermissionViolation) || errors.Is(err, nats.ErrAuthorization) || tlsalert.In(err) {
		return false
	}
	if errors.Is(err, nats.ErrNoServers) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// Refused reports whether err is the server refusing the connection's credential: nats.go's
// authorization violation, which a hub whose connections the NATS auth service admits answers for
// every refusal of that service, whatever its reason (the service tells the server why, and the
// server tells the client only that it was refused). For a seed's connection that is a
// misconfiguration, which Unreachable refuses; for the daemon's service token it is waited out,
// at boot and while running alike (ConnectService, ReconnectBackoff).
func Refused(err error) bool {
	return errors.Is(err, nats.ErrAuthorization)
}

// ReconnectBackoff is the connection option that waits initial before the first reconnect of a
// lost connection, doubling each attempt up to max: the daemon's service-token connection's wait
// while running, the same schedule its boot waits a refusal out on (daemon.readinessRetry), so a
// hub whose auth service refuses every client while it is away is not met with a reconnect per
// client every two seconds.
func ReconnectBackoff(initial, max time.Duration) nats.Option {
	return nats.CustomReconnectDelay(func(attempts int) time.Duration {
		wait := initial
		for range attempts - 1 {
			if wait >= max/2 {
				return max
			}
			wait *= 2
		}
		return min(wait, max)
	})
}

// PublicKey is the public key of the nkey user seed is the seed of, a seed Seed answered: what a
// process may say about the seed it holds without the seed leaving it.
func PublicKey(seed string) (string, error) {
	user, err := userKey(seed, "the NATS nkey seed")
	if err != nil {
		return "", err
	}
	return user.PublicKey()
}

// userKey is seed's key pair, refused, naming source, when seed is no nkey seed or the seed of
// something other than a user.
func userKey(seed, source string) (nkeys.KeyPair, error) {
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	public, err := user.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	if !nkeys.IsValidPublicUserKey(public) {
		return nil, fmt.Errorf("%s holds an nkey seed that is not a user's (public key %s)", source, public)
	}
	return user, nil
}
