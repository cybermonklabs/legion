package bus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/natsconn"
)

// reconnectErrEvery is how often one connection reports a failed reconnect attempt. nats.go
// retries about once a second forever (ReconnectWait, MaxReconnect -1), so an outage of an hour
// would be thousands of identical lines; one a minute still names the outage in every log window
// that matters.
const reconnectErrEvery = time.Minute

// reconnectErrThrottle lets the first failed reconnect attempt of an outage through and then one
// per reconnectErrEvery of its clock. now is the clock so a test can pass a fake one: ./internal/bus
// runs under -race with parallel tests, so there is no package-level clock any test could swap.
type reconnectErrThrottle struct {
	mu   sync.Mutex
	now  func() time.Time
	last time.Time
}

// allow reports whether this failure is logged: the first one is, and after it only one per
// reconnectErrEvery.
func (t *reconnectErrThrottle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.now()
	if !t.last.IsZero() && at.Sub(t.last) < reconnectErrEvery {
		return false
	}
	t.last = at
	return true
}

// reset forgets the last allowed failure, so the first failure of the next outage is logged at
// once rather than swallowed by the previous outage's minute.
func (t *reconnectErrThrottle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = time.Time{}
}

// newReconnectErrThrottle is a throttle on now, time.Now when now is nil.
func newReconnectErrThrottle(now func() time.Time) *reconnectErrThrottle {
	if now == nil {
		now = time.Now
	}
	return &reconnectErrThrottle{now: now}
}

// silentCutPingInterval and silentCutMaxPingsOut are the pings of WithSilentCutDetection. nats.go
// counts a ping out at each interval and declares the connection stale on the interval that would
// put a third one out, and any PONG clears the count, so a server that stops answering is found two
// to three seconds later: inside the five seconds envoy-dispatch's /healthz has to report it in,
// while a PONG may still take two seconds before a loaded server is taken for a cut one. A
// connection that is cut reconnects as one whose server closed it does.
const (
	silentCutPingInterval = time.Second
	silentCutMaxPingsOut  = 2
)

// detectSilentCuts is the nats.Option WithSilentCutDetection adds to every connection of a client.
func detectSilentCuts(o *nats.Options) error {
	o.PingInterval = silentCutPingInterval
	o.MaxPingsOut = silentCutMaxPingsOut
	return nil
}

// The waits of WithPromptReconnect (promptRedialWait): the first redial of an outage is at once
// but for promptRedialJitter, which keeps two replicas from dialling in the same instant; each
// later wait is drawn anew between half and all of a bound that starts at promptRedialBase and
// doubles with each redial up to promptRedialCap.
const (
	promptRedialJitter = 50 * time.Millisecond
	promptRedialBase   = 250 * time.Millisecond
	promptRedialCap    = 2 * time.Second
)

// promptRedialWait is a connection's wait, made WithPromptReconnect, in the pass'th round of an
// outage's dials over the servers of its pool. It is the connection's CustomReconnectDelayCB:
// nats.go calls it once each round, with the round's number counted from 1 again in each outage,
// so the waits start over at every connect (nats.go v1.55.0 doReconnect, :3413 and :3480-3528).
// With one server it is the wait before each redial; a pool of n servers is dialled n times for
// each wait. draw returns a number in [0, n) uniformly: rand.Int64N, or a test's own.
func promptRedialWait(pass int, draw func(n int64) int64) time.Duration {
	if pass <= 1 {
		return time.Duration(draw(int64(promptRedialJitter)))
	}
	bound := promptRedialBase
	for round := 2; round < pass && bound < promptRedialCap; round++ {
		bound = min(bound*2, promptRedialCap)
	}
	return bound/2 + time.Duration(draw(int64(bound/2)))
}

// promptRedial is the nats.Option WithPromptReconnect adds to every connection of a client.
func promptRedial(o *nats.Options) error {
	o.CustomReconnectDelayCB = func(pass int) time.Duration { return promptRedialWait(pass, rand.Int64N) }
	return nil
}

// allOf applies each option that is not nil, in order.
func allOf(options ...nats.Option) nats.Option {
	return func(o *nats.Options) error {
		for _, option := range options {
			if option == nil {
				continue
			}
			if err := option(o); err != nil {
				return err
			}
		}
		return nil
	}
}

// connectInBackground is the nats.Option WithConnectInBackground adds to the first connection of a
// client, the one its constructor returns, and to no replacement ensureConnWithContext dials. nats.go
// returns that connection whether or not a server answered and connects it in place, retrying as
// it reconnects, forever (MaxReconnect -1), and never closes it over a refused credential: by
// default it closes a connection the same server refuses twice in a row, which here would leave the
// process with no bus until a recovery found it. connected runs on the first connection, after
// nats.go has sent the core subscriptions made before it.
//
// A refusal of the credential (authorizationRefusal) is logged at ERROR, at most once per
// reconnectErrEvery of refusals: a refusal that lasts is a grant someone has to fix, and nats.go
// retries it about once a second (every 1 to 2 s at WithPromptReconnect's cap, promptRedialWait).
// Once the connection has connected, nats.go reports each refusal to the async error callback,
// which takes the line here in place of its own; before then it reports none, and
// reportRefusalsUntilConnected logs them on the same throttle.
func connectInBackground(logger *slog.Logger, name string, urls []string, refusals *reconnectErrThrottle, connected func(*nats.Conn)) nats.Option {
	return func(o *nats.Options) error {
		o.RetryOnFailedConnect = true
		o.IgnoreAuthErrorAbort = true
		connectedCB, reconnectedCB, report := o.ConnectedCB, o.ReconnectedCB, o.AsyncErrorCB
		o.ConnectedCB = func(nc *nats.Conn) {
			// The connection's own report comes first (options' ConnectedCB, which records it for
			// the client's connected flag, Client.noteConnection), so a held first-connect callback
			// (HoldFirstConnectForTest) holds what the client does with the connection, not the
			// flag that says nats.go connected it.
			if connectedCB != nil {
				connectedCB(nc)
			}
			if gate := firstConnectGate.Load(); gate != nil {
				<-*gate
			}
			refusals.reset()
			logger.Info("envoy nats connected", slog.String("name", name), slog.String("server", natsconn.ServerPosition(urls, nc.ConnectedUrl())))
			if connected != nil {
				connected(nc)
			}
		}
		o.ReconnectedCB = func(nc *nats.Conn) {
			refusals.reset()
			if reconnectedCB != nil {
				reconnectedCB(nc)
			}
		}
		o.AsyncErrorCB = func(nc *nats.Conn, sub *nats.Subscription, err error) {
			if authorizationRefusal(err) {
				if refusals.allow() {
					logRefusal(logger, name, urls, err)
				}
				return
			}
			if report != nil {
				report(nc, sub, err)
			}
		}
		return nil
	}
}

// reportRefusalsUntilConnected logs each refusal of the credential while nc makes its first
// connection, at ERROR and on refusals' throttle, and returns once nc has connected or closed.
// nats.go tells no callback of a refusal before a connection has first connected (it reports only
// a dial that failed, through ReconnectErrCB), but keeps it as the connection's last error until
// its next attempt, a reconnect wait later, so a read each second sees every run of refusals.
func reportRefusalsUntilConnected(logger *slog.Logger, nc *nats.Conn, name string, urls []string, refusals *reconnectErrThrottle) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !nc.IsConnected() && !nc.IsClosed() {
		if err := nc.LastError(); authorizationRefusal(err) && refusals.allow() {
			logRefusal(logger, name, urls, err)
		}
		<-ticker.C
	}
}

// connectFirstInBackground makes the connection a constructor returns with WithConnectInBackground.
// dial connects with the option it is handed added to the constructor's own settings, and here
// that option is connectInBackground, so nats.go dials alone until the connection first connects.
// The refusals of the credential before then are logged by reportRefusalsUntilConnected, on the
// throttle connectInBackground logs the later ones on, a throttle on now (newReconnectErrThrottle).
//
// install, when not nil, records the connection dial returned; an error from it closes the
// connection and is returned. connected, when not nil, runs on the first connection, and never
// before install has returned: nats.go can connect the connection as soon as dial returns, and
// connected may read what install records.
func connectFirstInBackground(logger *slog.Logger, name string, urls []string, now func() time.Time, dial func(nats.Option) (*nats.Conn, error), connected func(*nats.Conn), install func(*nats.Conn) error) (*nats.Conn, error) {
	refusals := newReconnectErrThrottle(now)
	installed := make(chan struct{})
	defer close(installed)
	first := func(nc *nats.Conn) {
		<-installed
		if connected != nil {
			connected(nc)
		}
	}
	nc, err := dial(connectInBackground(logger, name, urls, refusals, first))
	if err != nil {
		return nil, err
	}
	if install != nil {
		if err := install(nc); err != nil {
			nc.Close()
			return nil, err
		}
	}
	go reportRefusalsUntilConnected(logger, nc, name, urls, refusals)
	return nc, nil
}

// authorizationRefusals are the errors nats.go reports the server refusing a connection's
// credential with: a credential the server does not know (nats.ErrAuthorization), or one whose
// authorization expired or was revoked.
var authorizationRefusals = [...]error{nats.ErrAuthorization, nats.ErrAuthExpired, nats.ErrAuthRevoked, nats.ErrAccountAuthExpired}

// authorizationRefusal reports whether err is the server refusing the connection's credential:
// whether err or any error it wraps is one of authorizationRefusals (errors.Is).
func authorizationRefusal(err error) bool {
	for _, refusal := range authorizationRefusals {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// AuthorizationRefused reports whether err holds the server's refusal of the connection's
// credential anywhere in its chain (authorizationRefusal), an earlier refusal that a start's last
// error names beside the error its last try failed with (withLastRefusal) included. cmd/listener's
// start records it as a refusal the start met before the main client's connect ends the start, so
// the start's last line names the credential (connectListener, startFailureLine). Whether the
// connect's last try was refused, which decides whether a later connect ends the start, is
// FinalTryRefused's question.
func AuthorizationRefused(err error) bool { return authorizationRefusal(err) }

// FinalTryRefused reports whether err, the error a connect ended with, is the server refusing the
// connection's credential at the connect's last try: whether err or an error it wraps is one of
// authorizationRefusals, read along err's chain as errors.Is reads it but following only the first
// error of an error that wraps several (Unwrap() []error). In the form a start's last error takes
// when it names an earlier refusal beside the error its last try failed with (withLastRefusal),
// that first error is the last try's, so the earlier refusal does not count; a start that ended at
// its bound on a refusal (credentialWaitEnded) wraps that refusal alone, so it counts.
// cmd/listener's later connects end the start on it and try every other error again by the start
// rule (openStartConnection).
func FinalTryRefused(err error) bool {
	for err != nil {
		if refusalItself(err) {
			return true
		}
		switch wrapper := err.(type) {
		case interface{ Unwrap() error }:
			err = wrapper.Unwrap()
		case interface{ Unwrap() []error }:
			wrapped := wrapper.Unwrap()
			if len(wrapped) == 0 {
				return false
			}
			err = wrapped[0]
		default:
			return false
		}
	}
	return false
}

// refusalItself reports whether err itself, not an error it wraps, is one of
// authorizationRefusals, matched as errors.Is matches each error of a chain: equal to it, or
// saying so through an Is method (FinalTryRefused).
func refusalItself(err error) bool {
	is, hasIs := err.(interface{ Is(error) bool })
	for _, refusal := range authorizationRefusals {
		if err == refusal || hasIs && is.Is(refusal) {
			return true
		}
	}
	return false
}

// logRefusal writes the line a refused credential is reported with, naming the servers by their
// count and the refusal by its class (natsconn.ConnectionClass), as the reconnect failure's line does.
func logRefusal(logger *slog.Logger, name string, urls []string, err error) {
	logger.Error("envoy nats authorization refused",
		slog.String("name", name),
		slog.Int("servers", len(urls)),
		slog.String("error", natsconn.ConnectionClass(err)))
}

// handshaking says whether nc is in a reconnect's handshake (CONNECTING) or between its attempts
// (RECONNECTING), the two states a connection the server closed during that handshake is in when
// its async error callback runs.
func handshaking(nc *nats.Conn) bool {
	if nc == nil {
		return false
	}
	status := nc.Status()
	return status == nats.CONNECTING || status == nats.RECONNECTING
}

// handshakeReadFailed says whether err is how a read of a connection fails: the peer closed it
// (io.EOF, io.ErrUnexpectedEOF), or the read itself failed (a *net.OpError, which nats.go's read
// returns for a reset connection and for one that passed its deadline). An error the handshake
// builds itself, such as a credentials callback's (nats.go's connectProto), is neither.
func handshakeReadFailed(err error) bool {
	var readErr *net.OpError
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &readErr)
}

// connectCallbacks are what a connection tells its client of its own events. nats.go runs every
// callback of one connection on that connection's dispatcher goroutine, one at a time and in the
// order of the events, so none of them may block: a reconnect callback that waits holds the
// disconnect behind it (Client.queueReconnect runs the reconnect hooks off that goroutine for
// exactly that reason).
type connectCallbacks struct {
	// reconnected, when not nil, runs on each connect and reconnect of the connection, the first
	// connection of a client that connects in the background excepted: that one reports through
	// connectFirstInBackground's connected instead, once the client has installed it.
	reconnected func(*nats.Conn)
	// disconnected, when not nil, runs once the connection has disconnected, after the line this
	// package logs for it: a disconnect is a connection event the client reports to the callers
	// waiting on one (Client.connectionChanged).
	disconnected func()
	// closed, when not nil, runs once the connection is closed: by Close or Drain, or by nats.go
	// giving up on it (Client.onClosed). It is handed the connection that closed, so the client can
	// tell which of its connections' closes it has handled (Client.closeHandled).
	closed func(*nats.Conn)
	// asyncErr, when not nil, takes each error the connection reports on its own, after the line
	// this package logs for it (Client.onAsyncError).
	asyncErr func(*nats.Conn, error)
	// state, when not nil, is told at each of those events whether the connection is connected: a
	// connect or a reconnect reports it up, a disconnect or a close reports it down. It is what a
	// client keeps its lock-free connected flag by (Client.noteConnection), since nats.go holds
	// the connection's own lock across a reconnect dial that can hang for the connect timeout,
	// and nats.Conn.Status waits for it.
	state func(*nats.Conn, bool)
}
// options starts from nats.GetDefaultOptions: Connect fills in only some zero fields, and the
// defaults are what turn on reconnecting, the client's pings and the drain and flusher timeouts. A
// lost server is reconnected in place, forever, so JetStream handles taken from the connection
// keep working; ReconnectedCB calls callbacks.reconnected, which hands the re-subscribe and a run
// of the reconnect hooks to a goroutine of the client's, off nats.go's callback goroutine
// (Client.queueReconnect). DisconnectedErrCB logs the disconnect and then calls
// callbacks.disconnected: the reconnect loop keeps every subscription open through an outage, so a
// KV watcher never ends on its own and only this callback says its cache stopped following the
// bucket. ClosedCB fires on Close or Drain, and also when nats.go gives up on the connection
// itself: the same authorization error twice in a row, or a server -ERR it does not classify.
// Those two are why onClosed still starts a recovery that dials a replacement. It logs the close
// and then hands callbacks.closed the connection that closed, so the client can tell which of its
// connections' closes it has handled (closeHandled). The reconnect buffer is off, so nothing is
// held and sent after its caller saw an error: a publish while reconnecting waits for the
// reconnect instead (ensureConnWithContext), and a publish that fails was not sent. AsyncErrorCB
// logs each error the connection reports on its own and then hands it to callbacks.asyncErr, where
// the client reads the server's refusal of its core subscription (onAsyncError). Every line goes
// to logger.
func options(logger *slog.Logger, name string, urls []string, callbacks connectCallbacks, now func() time.Time) nats.Options {
	// The throttle is built here, one per call, so it is per connection and has its own mutex:
	// options is rebuilt for every dial attempt in connectTrying's loop and only the attempt
	// that connects leaves its closures on the *nats.Conn, so one live connection has exactly one
	// throttle and a second client's first failure is never swallowed by this one's minute.
	throttle := newReconnectErrThrottle(now)
	// note tells callbacks.state of each event, so a caller that reads the flag it keeps reads
	// what the last event of this connection said.
	note := func(nc *nats.Conn, up bool) {
		if callbacks.state != nil {
			callbacks.state(nc, up)
		}
	}
	opts := nats.GetDefaultOptions()
	opts.Servers = urls
	opts.Name = name
	opts.NoRandomize = true
	opts.Timeout = connectTimeout
	opts.MaxReconnect = -1
	// A publish while reconnecting waits for the reconnect (ensureConnWithContext), so the wait
	// between attempts is how long it waits after NATS is back.
	opts.ReconnectWait = time.Second
	opts.ReconnectBufSize = -1
	opts.DisconnectedErrCB = func(nc *nats.Conn, err error) {
		note(nc, false)
		if err != nil {
			// The cause by its class (natsconn.ConnectionClass): a dial or a read through the
			// SOCKS5 proxy names the server by the host its URL spells.
			logger.Info("envoy nats disconnected", slog.String("name", name), slog.String("error", natsconn.ConnectionClass(err)))
		} else {
			logger.Info("envoy nats disconnected", slog.String("name", name))
		}
		if callbacks.disconnected != nil {
			callbacks.disconnected()
		}
	}
	// A reconnect attempt that fails is otherwise silent: nats.go retries in place forever, so a
	// broker that is down leaves no line at all in the process's log. The line names how many
	// servers this client was configured with (urls) and the failure's class (natsconn.ConnectionClass),
	// never a server's URL or the error's text, which names the server it failed at by its host
	// (refuseRemoteNATS says why that host can be a piece of a credential). The line goes to
	// logger, the client's (WithLogger), which in a deployed process is followsDefault and so
	// writes where slog.Default() points, like every other bus line, because a listener process
	// test fails if a bus line stops being Go's text format (cmd/listener/startup_test.go,
	// TestListenerLogsFromOtherPackagesKeepTheirHandlers, names the deployed metric filters). The
	// NATS credential never reaches a line: options never sees it, since credential is applied to
	// the options value afterwards (connect.go, connectTrying) and holds a public key and a signer,
	// or a token reader, only.
	reconnectAttemptFailed := func(err error) {
		if !throttle.allow() {
			return
		}
		logger.Warn("envoy nats reconnect attempt failed",
			slog.String("name", name),
			slog.Int("servers", len(urls)),
			slog.String("error", natsconn.ConnectionClass(err)))
	}
	opts.ReconnectErrCB = func(_ *nats.Conn, err error) { reconnectAttemptFailed(err) }
	opts.ReconnectedCB = func(nc *nats.Conn) {
		note(nc, true)
		// Each outage reports its own first failed attempt at once, rather than staying silent for
		// what is left of the previous outage's minute.
		throttle.reset()
		// ConnectedUrl is the server's URL.String(), userinfo and all, and a server nats.go
		// discovered inherits the configured URL's (nats.go v1.55.0 ConnectedUrl), so the line names
		// the server by its position alone (natsconn.ServerPosition).
		logger.Info("envoy nats reconnected", slog.String("name", name), slog.String("server", natsconn.ServerPosition(urls, nc.ConnectedUrl())))
		if callbacks.reconnected != nil {
			callbacks.reconnected(nc)
		}
	}
	// A connection that connects only after failing to (connectInBackground) reports its first
	// connection here rather than as a reconnect, and that ends its outage as a reconnect does.
	opts.ConnectedCB = func(nc *nats.Conn) {
		note(nc, true)
		throttle.reset()
	}
	opts.ClosedCB = func(nc *nats.Conn) {
		note(nc, false)
		logger.Info("envoy nats connection closed", slog.String("name", name))
		if callbacks.closed != nil {
			callbacks.closed(nc)
		}
	}
	opts.AsyncErrorCB = func(nc *nats.Conn, sub *nats.Subscription, err error) {
		if sub == nil && handshakeReadFailed(err) && handshaking(nc) {
			// A reconnect's CONNECT and PING handshake whose reply the client cannot read - a
			// server that closes the connection (a server restarting, or one that accepted the dial
			// as it shut down), resets it, or does not answer within the handshake's deadline -
			// makes nats.go's sendConnect report the read's error here, and nowhere else: the
			// reconnect loop then tries again without calling ReconnectErrCB (nats.go v1.55.0
			// sendConnect, processConnectInit and doReconnect). It is a failed reconnect attempt,
			// so it is logged as one, on the same throttle. Only that report is: the connection is
			// still in that handshake (CONNECTING) or waiting for the next attempt (RECONNECTING)
			// when the callback runs, and every other async error, the same read error on a
			// connected connection included, stays an ERROR.
			reconnectAttemptFailed(err)
			if callbacks.asyncErr != nil {
				callbacks.asyncErr(nc, err)
			}
			return
		}
		level := slog.LevelError
		if errors.Is(err, nats.ErrConsumerNotActive) {
			// Every consumer Envoy runs with idle heartbeats is a KV watcher's ordered consumer,
			// which reports missed heartbeats only while the connection is not connected; once it
			// is connected again, nats.go resets the consumer instead. The report restates the
			// disconnect logged above, once per watcher. The listener's durable carries no heartbeat:
			// cmd/listener's ensureListenerDurable refuses one that does before the lease holder
			// subscribes to it.
			level = slog.LevelWarn
		}
		// The error by its class (natsconn.ConnectionClass), as every bus line names one: nats.go's
		// own fixed words and a server's refusal print as they are, and an error nats.go formatted
		// from what the server sent, or any other, by a fixed class.
		if sub != nil {
			logger.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("subject", sub.Subject), slog.String("error", natsconn.ConnectionClass(err)))
		} else {
			logger.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("error", natsconn.ConnectionClass(err)))
		}
		if callbacks.asyncErr != nil {
			callbacks.asyncErr(nc, err)
		}
	}
	return opts
}

// connectTimeout bounds each dial of a NATS server and its handshake: the bus's direct dialer's for
// a direct dial, the bus's SOCKS5 dialer's for one through a proxy (socks5Proxy), and nats.go's own
// for the handshake (options). A connect with a deadline cuts each to what remains of it.
const connectTimeout = 5 * time.Second

// connectTries is how many times a connect dials before it ends with the last dial's error, and
// connectRetryWait the wait between two of its dials: a server that is restarting, or a network
// that dropped a dial, answers within them, and a start that ends after them is restarted by
// whatever runs the process. A start with WithCredentialWait holds the tries of its own that fail
// other than by a refusal of the credential to the same two (waitOutCredentialRefusals).
const (
	connectTries     = 10
	connectRetryWait = time.Second
)

func connect(logger *slog.Logger, name string, urls []string, settings nats.Option, dialer *socks5Dialer, callbacks connectCallbacks, now func() time.Time) (*nats.Conn, error) {
	return connectWithContext(context.Background(), logger, name, urls, settings, dialer, callbacks, now)
}

// connectWithContext dials urls with envoy's options, then settings when it is not nil: the NATS
// credential the environment names (credential), and what the client's options add (connectOpts.connectionSettings,
// connectInBackground). Every connection dials through a severableDialer of its own, so Sever can
// end it. When dialer is not nil it dials through the SOCKS5 proxy it names (socks5Proxy): every
// connection then, the first and each one nats.go dials to reconnect, goes through the proxy,
// handed the server's host name as the URL spells it. It dials up to connectTries times, waiting
// connectRetryWait after each dial that fails, the last one included, whatever each dial failed
// on, a refusal of the credential included, and returns the first connection that connects
// (connectTrying).
//
// A connect through a proxy that fails names the setting and the proxy, whatever failed: its ten
// attempts ran out, the credential was refused, or ctx ended first (a publish's own deadline). A
// dial error comes back with the proxy's address but not the setting, and a refused dial as
// nats.go's ErrNoServers alone, which names neither, so the proxy's own dial error stands in for
// it. The address carries no credentials: CheckSOCKS5Proxy admits only an IP address with no zone
// or a DNS name. A failed connect prints as its class alone (natsconn.ConnectionError): a
// dial's error names the server by the host its URL spells, the proxy's among them, and that host
// can be a piece of a credential (refuseRemoteNATS).
func connectWithContext(ctx context.Context, logger *slog.Logger, name string, urls []string, settings nats.Option, dialer *socks5Dialer, callbacks connectCallbacks, now func() time.Time) (*nats.Conn, error) {
	return connectTrying(ctx, connectTries, connectRetryWait, logger, name, urls, settings, dialer, callbacks, now)
}

// connectTrying is connectWithContext dialling up to tries times in place of connectTries, and
// waiting wait after each dial that fails, the last one included, in place of connectRetryWait:
// every connect but one dials connectTries times connectRetryWait apart and ends that long after
// its last failed dial, as it always has. The one is a try of a start that waits out refusals of
// its credential, which dials once with no wait (tries 1, wait 0) and waits between its tries
// itself (waitOutCredentialRefusals).
func connectTrying(ctx context.Context, tries int, wait time.Duration, logger *slog.Logger, name string, urls []string, settings nats.Option, dialer *socks5Dialer, callbacks connectCallbacks, now func() time.Time) (_ *nats.Conn, err error) {
	if dialer != nil {
		defer func() {
			if err != nil {
				err = fmt.Errorf("bus: NATS connect through the SOCKS5 proxy %s=%s failed: %w", SOCKS5ProxyEnvVar, dialer.Address(), err)
			}
		}()
	}
	var lastErr error
	for range tries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := options(logger, name, urls, callbacks, now)
		if settings != nil {
			if err := settings(&next); err != nil {
				return nil, err
			}
		}
		attemptDialer := dialer
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Not ctx.Err(): the deadline can pass before the context's timer marks it done,
				// and a nil error here would hand the caller a nil connection to use.
				return nil, context.DeadlineExceeded
			}
			if remaining < next.Timeout {
				next.Timeout = remaining
			}
			if dialer != nil {
				attemptDialer = dialer.bounded(remaining)
			}
		}
		// Every connection dials through a severable dialer, so Client.Sever can end it without
		// nats.go's lock: through the SOCKS5 proxy when one is set, else directly, bounded by the
		// attempt's timeout as nats.go's own dialer is. nats.go otherwise resolves the host itself
		// and hands the dialer an address, which the proxy cannot reach when only its resolver knows
		// the name; a direct dial resolves it in Go's dialer, which spreads the timeout over the
		// addresses as nats.go spreads it over the hosts it resolved.
		dial := (&net.Dialer{Timeout: next.Timeout}).DialContext
		if attemptDialer != nil {
			dial = attemptDialer.DialContext
		}
		next.CustomDialer = newSeverableDialer(dial)
		next.SkipHostLookup = true
		type connectionResult struct {
			conn *nats.Conn
			err  error
		}
		result := make(chan connectionResult, 1)
		go func() {
			conn, err := next.Connect()
			result <- connectionResult{conn: conn, err: err}
		}()
		select {
		case connected := <-result:
			if connected.err == nil {
				return connected.conn, nil
			}
			lastErr = connected.err
		case <-ctx.Done():
			go func() {
				connected := <-result
				if connected.conn != nil {
					connected.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if dialer != nil && errors.Is(lastErr, nats.ErrNoServers) {
		lastErr = cmp.Or(dialer.LastError(), lastErr)
	}
	return nil, natsconn.ConnectionError(lastErr)
}
