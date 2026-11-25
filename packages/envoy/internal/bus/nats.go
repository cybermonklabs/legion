package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ConnectOption configures a client: Connect and ConnectOwningStream take every one. Dial takes
// only the DialOptions among them.
type ConnectOption interface{ apply(*connectOpts) }

// DialOption is a ConnectOption Dial honours too: WithConnectInBackground, WithSilentCutDetection,
// WithPromptReconnect, WithInboxPrefix and WithReconnectErrClock. Dial's name and environment
// are its parameters, so WithName and WithEnvironment are not DialOptions, and neither are the
// options of a client's JetStream (WithReplicas, WithPublishAcknowledgementClock).
type DialOption func(*connectOpts)

func (o DialOption) apply(opts *connectOpts) { o(opts) }

// clientOption is a ConnectOption only Connect and ConnectOwningStream honour.
type clientOption func(*connectOpts)

func (o clientOption) apply(opts *connectOpts) { o(opts) }

type connectOpts struct {
	name                        string
	replicas                    int
	streamMaxBytes              int64
	keepInterval                time.Duration
	keptBuckets                 []string
	publishAcknowledgementClock AcknowledgementClock
	reconnectErrClock           func() time.Time
	environment                 func(string) (string, bool)
	streamSubjectsRefresh       time.Duration
	reconnectWaitTick           func()
	beforeRequestFlush          func()
	beforeRequestWait           func(loss <-chan struct{})
	beforeCoreSendFlush         func()
	afterCoreSendFlush          func()
	requestExpiry               func(deadline time.Time) (expired <-chan time.Time, flushSettled func())
	background                  bool
	silentCutDetection          bool
	promptReconnect             bool
	inboxPrefix                 string
	streamMessageReads          bool
	keeperStepGuard             time.Duration
	// credentialWait, credentialRefused and credentialAdmitted are WithCredentialWait's bound and
	// reports, and startCheck WithStartCheck's check.
	credentialWait     time.Duration
	credentialRefused  func(error)
	credentialAdmitted func()
	startCheck         func(*Client) error
	closedHook         func()
	closeSettleBound   time.Duration
}

// connectionSettings is what every connection of a client applies after envoy's own options
// (connectWithContext): the NATS credential names (credential, nil for none), the pings of
// WithSilentCutDetection, the redial waits of WithPromptReconnect and the inbox prefix of
// WithInboxPrefix, each when the client asked for it.
func (o connectOpts) connectionSettings(credential nats.Option) nats.Option {
	settings := []nats.Option{credential}
	if o.silentCutDetection {
		settings = append(settings, detectSilentCuts)
	}
	if o.promptReconnect {
		settings = append(settings, promptRedial)
	}
	if o.inboxPrefix != "" {
		settings = append(settings, nats.CustomInboxPrefix(o.inboxPrefix))
	}
	if len(settings) == 1 {
		return credential
	}
	return allOf(settings...)
}

// parseConnectOptions applies options over the defaults every connect starts from.
func parseConnectOptions[O ConnectOption](options []O) connectOpts {
	opts := connectOpts{name: "envoy", replicas: 1, keepInterval: defaultKeepInterval, publishAcknowledgementClock: wallClock{}, environment: os.LookupEnv, streamSubjectsRefresh: streamSubjectsRefresh}
	for _, o := range options {
		o.apply(&opts)
	}
	return opts
}

// connectionBuild is the build every connection of this binary names after its name
// (buildversion.ConnectionBuild): legion@<commit>, or "" for a build that holds no commit. A test of
// this package replaces it.
var connectionBuild = buildversion.ConnectionBuild

// PrivacyRequiredSuffix is natsgrant.PrivacyRequiredSuffix, spelled here because natsgrant imports
// this package and this one cannot import it back; natsgrant's tests hold the two equal. The auth
// service admits a listener on a hub with private projects only when its client name ends with
// it, so connectionName keeps it last.
const PrivacyRequiredSuffix = " privacy-required"

// connectionName is the name a connection presents: the client's name, then a space and this
// binary's build when it has one, before PrivacyRequiredSuffix when the client's name ends with it
// (`<name> legion@<commit> privacy-required`). legacy-subjects-gate.sh reads each connection's
// build from the end of its name in the hub's census of connections ($SYS.REQ.SERVER.PING.CONNZ),
// the last word or the one before a trailing privacy-required, and a connection that names none
// keeps the gate from opening.
func connectionName(name string) string {
	build := connectionBuild()
	if build == "" {
		return name
	}
	if client, required := strings.CutSuffix(name, PrivacyRequiredSuffix); required {
		return client + " " + build + PrivacyRequiredSuffix
	}
	return name + " " + build
}

// streamConfig is the stream ConnectOwningStream ensures: streamCfg at the deployment's replica
// count and byte limit.
func (o connectOpts) streamConfig() nats.StreamConfig {
	cfg := *streamCfg
	cfg.Replicas = o.replicas
	cfg.MaxBytes = o.streamMaxBytes
	return cfg
}

// AcknowledgementClock supplies deadline contexts for JetStream publish acknowledgements.
type AcknowledgementClock interface {
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

type wallClock struct{}

func (wallClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// WithPublishAcknowledgementClock overrides the clock that bounds JetStream publish acknowledgements.
func WithPublishAcknowledgementClock(clock AcknowledgementClock) ConnectOption {
	return clientOption(func(o *connectOpts) { o.publishAcknowledgementClock = clock })
}

// WithReconnectErrClock overrides the clock the bus reads for the time-based parts of its
// reconnect reporting: the throttle of the warning a failed reconnect attempt logs
// (reconnectErrThrottle) and the stall HookStall reports, so a test can see a minute pass without
// waiting one. nil, the default, is time.Now.
func WithReconnectErrClock(now func() time.Time) DialOption {
	return func(o *connectOpts) { o.reconnectErrClock = now }
}

// WithReplicas sets the deployment's replica count, ReplicasEnvVar, for the stream
// ConnectOwningStream ensures (default 1): the count the stream is created at, and the count a
// deployed stream with fewer copies is raised to, at the start (ensureStreamWithConfig) and by the
// client's keeper after it (keepStream). A start never writes fewer copies than it read, but a
// concurrent start with a lower count can put back the copies it read, until the keeper raises
// them again a period later: the count holds only while every program of the deployment carries it.
func WithReplicas(n int) ConnectOption {
	return clientOption(func(o *connectOpts) { o.replicas = n })
}

// WithStreamSubjectsRefresh replaces streamSubjectsRefresh for one client, so a test can see the
// client read the deployed subjects again without waiting a minute, or hold that read off for the
// whole of a window in which it counts what the client's connection sends. It is exported for the
// tests of the programs that connect through this package, as WithKeepInterval is: no deployment
// sets it, and no caller outside a test may.
func WithStreamSubjectsRefresh(every time.Duration) ConnectOption {
	return clientOption(func(o *connectOpts) { o.streamSubjectsRefresh = every })
}

// WithStreamMaxBytes sets the deployment's byte limit, StreamMaxBytesEnvVar, for the stream
// ConnectOwningStream ensures: the stream's MaxBytes at create, and the limit a deployed stream with
// a smaller one or none is raised to, at the start (ensureStreamWithConfig) and by the client's
// keeper after it (keepStream). Neither ever lowers a deployed limit, so an operator's raise of a
// full stream stands and programs with different limits leave the largest; lowering one is an
// operator step once every program's setting is lowered. A full stream refuses a new publish
// (ErrStreamFull) and keeps every stored message, which leaves only by age, so the limit must sit
// above what the stream takes in its age. 0, the default, sets none: the stream is created with no
// byte limit, and a deployed stream keeps the limit it has.
func WithStreamMaxBytes(n int64) ConnectOption {
	return clientOption(func(o *connectOpts) { o.streamMaxBytes = n })
}

// WithKeptBuckets names the KV buckets this program opens with EnsureKeyValue at the deployment's
// replica count, which the keeper of a ConnectOwningStream client raises back to WithReplicas'
// count each period (keep). Each name must be a bucket the program's NATS grant covers: the
// keeper reads and updates each one's stream. A named bucket that does not exist yet is skipped
// until the program creates it.
func WithKeptBuckets(buckets ...string) ConnectOption {
	return clientOption(func(o *connectOpts) { o.keptBuckets = buckets })
}

// WithEnvironment names where a connect reads ENVOY_ALLOW_REMOTE_NATS (AllowRemoteEnvVar),
// ENVOY_NATS_SOCKS5_PROXY (SOCKS5ProxyEnvVar), NATS_NKEY_SEED_FILE and NATS_NKEY_SEED, in place of
// the process environment (os.LookupEnv): envoy-dispatch hands it the lookup over its settings
// table, which lists all four. Dial takes the same lookup as its environment parameter, and not
// this option.
func WithEnvironment(lookup func(string) (string, bool)) ConnectOption {
	return clientOption(func(o *connectOpts) { o.environment = lookup })
}

// WithName names the client, in place of "envoy": the name, with this binary's build after it or
// before its trailing PrivacyRequiredSuffix (connectionName), that the NATS server's connection
// listings (its /connz endpoint, `nats server report connections`, $SYS.REQ.SERVER.PING.CONNZ)
// show for the connection the client dials first and for each one it dials to replace a closed
// one, and the name field of the client's lifecycle log lines (`envoy nats disconnected`, `envoy
// nats recovery attempt` and the rest). Dial takes its name as a parameter, and not this option.
func WithName(name string) ConnectOption {
	return clientOption(func(o *connectOpts) { o.name = name })
}

// WithConnectInBackground has the constructor (Connect, ConnectOwningStream, Dial) return without
// waiting for NATS. A configuration error is still returned before any dial: an unusable nkey seed,
// a server ENVOY_ALLOW_REMOTE_NATS does not allow, an unusable SOCKS5 setting. A server that does
// not answer, or refuses the credential, is not an error: the client is returned with a connection
// nats.go has yet to connect, which nats.go then connects in place, retrying forever as it does a
// reconnect (connectInBackground). A refusal of the credential is logged at ERROR at most once a
// minute and retried, never given up on. Until that first connection Connected reports false, a
// publish waits for it within its own deadline and then fails as one does while reconnecting, a
// core subscription is sent once it connects, and a JetStream subscription's bind fails while its
// registration stays, so the first connection binds it. Only nats.go dials before then: the
// constructor's connection is never closed, so nothing replaces it (ensureConnWithContext). The
// first connection is a connection event, as a reconnect is (onConnect): the JetStream
// subscription is bound and the reconnect hooks run, and the client's first hook does then what
// the constructor of a client that waits does at connect: an owning client (ConnectOwningStream)
// ensures the stream (ensureStreamOn, run again at every later connection event, as for any owning
// client), and any other reads the deployed stream's subjects (readDeployedSubjectsUntilRead).
//
// envoy-dispatch's server takes it for each of its clients, so a bus that is down when it boots
// leaves it serving everything that does not need the bus, and connected once the bus answers.
func WithConnectInBackground() DialOption {
	return func(o *connectOpts) { o.background = true }
}

// WithSilentCutDetection has every connection the client makes declare itself lost two to three
// seconds after the server stops answering it (detectSilentCuts), where nats.go's defaults take
// four to six minutes. A server that goes away cleanly closes the socket, which the client sees at
// once; one whose packets are dropped (a partition, a node that vanished) closes nothing, and only
// the client's own pings notice. envoy-dispatch's server takes it for each of its clients, so its
// /healthz reports a cut bus within seconds of the cut.
func WithSilentCutDetection() DialOption {
	return func(o *connectOpts) { o.silentCutDetection = true }
}

// WithPromptReconnect has every connection the client makes redial at once after a cut, within a
// jitter of 50 ms where every other client waits a second, and back off while the hub stays away
// or refuses it: each later wait is drawn anew between half and all of a bound that starts at
// 250 ms and doubles with each redial up to 2 s, and the next outage starts at once again
// (promptRedialWait). It is for the one client a hub's other clients wait on: the NATS auth
// service (internal/cipher/natscallout), which is itself a client of the hub it admits
// connections to, so a restart drops its connection too and every client that comes back before
// it is refused until it returns. So the cap is short: it bounds each wait at 2 s, whatever the
// outage's length, and a dial follows each wait; that dial, like one already in flight when the
// hub returns, takes its own time, up to its 5 s timeout when it hangs (connectTimeout). The
// backoff is for a hub that refuses the service's own user, as one whose auth_callout does not
// list it yet does: redialling within 50 ms each time, a replica connected to it 35 times a second
// for as long as it refused (1,055 times in 30 s against an in-process nats-server 2.15.1); with
// the backoff it connects 24 times in its first 30 s and at most once in any second after its
// first, for one URL (each wait is followed by a dial of every URL of the pool), which for two
// replicas is nothing to the hub. The jitter keeps two replicas from dialling in the same instant.
func WithPromptReconnect() DialOption {
	return func(o *connectOpts) { o.promptReconnect = true }
}

// WithInboxPrefix has every connection the client makes name its inboxes under prefix in place
// of nats.go's _INBOX (nats.CustomInboxPrefix): its request replies, and every inbox named by the
// connection's NewInbox, which is how this module names each one. An edge listener takes it
// (cmd/listener, natsgrant.EdgeInboxPrefix), so its user may subscribe its own inboxes and no other
// client's.
func WithInboxPrefix(prefix string) DialOption {
	return func(o *connectOpts) { o.inboxPrefix = prefix }
}

// WithStreamMessageReads has every KV bucket handle opened on the client's connection read a key
// with a stream message get ($JS.API.STREAM.MSG.GET.KV_<bucket>), which the JetStream API answers,
// in place of the direct get ($JS.API.DIRECT.GET.KV_<bucket>.>) nats.go sends for every bucket
// this module creates (streamMessageReads). An edge listener takes it (cmd/listener,
// natsgrant.Edge), whose grant holds no direct get: the stream itself publishes a direct get's
// answer, the stored value, on whatever reply subject the request names, and a value aimed at
// $JS.API. is taken by the JetStream API as a request of the account's.
func WithStreamMessageReads() ConnectOption {
	return clientOption(func(o *connectOpts) { o.streamMessageReads = true })
}

// WithCredentialWait has the constructor's start (Connect's or ConnectOwningStream's) wait out a
// refusal of the client's NATS credential for up to bound, counted from its first try, rather than
// fail at it: the server refusing the connect (authorizationRefusal), or refusing a request of the
// start check (WithStartCheck) under the credential's grant (ErrPublishDenied). Each try dials
// once, and an owning client's ensures the stream, and the check runs, on a client of its own, so a
// failed try leaves nothing running. An owning client's ensure of the stream that the grant
// refuses (ErrPublishDenied) is no refusal of the credential: the connect got in, so the start ends
// at once with that refusal, and the caller's own start tries again by its rule (cmd/listener's
// connectListener keeps the pod up and names the grant at each try, as its README's start rule
// has it for every refusal only the grant can lift). An ensure or a start check that fails for any
// other reason ends the start at once, with its own error, as it ends a start without the option.
// After a refusal the start waits a second before its next try and doubles each wait up to ten
// seconds, the last cut to end at bound; a try in progress at the bound may finish, and its
// success counts, and no try begins after the bound on a refusal (startTryLateness says what a
// late wake from the last wait may still begin). A dial that fails for any other reason counts
// toward the ten tries in a row, each followed by a wait of a second, the tenth too, that a start
// without the option makes, whatever the bound, so no start ends on such dials sooner than one
// without the option would; a refusal ends such a row. The start ends with an error at the first
// of the two limits, naming the last refusal when it met one, and with the failed step's own error
// when it met none (waitOutCredentialRefusals, credentialWaitEnded, withLastRefusal).
//
// refused, when not nil, is handed each refusal as it comes, on the constructor's goroutine, so the
// caller can report that its start is waiting (cmd/listener's /healthz answers 503 naming it).
// admitted, when not nil, is called once the constructor's start has got in: connected, and, for
// an owning client, ensured the stream, and run its start check, each a step that can refuse the
// credential. It runs before the constructor returns, so a caller whose own step after the
// constructor fails (a subscribe, a flush, a check of the server's version) knows its credential
// was admitted and is not what failed. Each refusal is also logged at WARN with the wait that
// follows it. A bound of zero or less is no wait: the start of a client without the option, whose
// connect tries ten times whatever the error and whose ensure fails at the server's first refusal;
// admitted is still called when it gets in. A start in the background (WithConnectInBackground)
// never fails, so it ignores the option.
//
// cmd/listener alone passes it, through its start (natsStart.option): to the main client, the role
// lane (ConnectRoleLane) and the session-consumer connection (ConnectSessionConsumers), each a
// connect of the listener's start, so a listener started before the hub's servers have reloaded
// its new nkey connects once they have, where it would exit and be restarted.
func WithCredentialWait(bound time.Duration, refused func(error), admitted func()) ConnectOption {
	return clientOption(func(o *connectOpts) {
		o.credentialWait, o.credentialRefused, o.credentialAdmitted = bound, refused, admitted
	})
}

// WithStartCheck has the constructor's start in the foreground (Connect's or ConnectOwningStream's)
// run check on the client each of its tries made, once that client has connected (and an owning
// client has ensured the stream), before the constructor returns it. An error from check closes the
// client, which stops everything it runs, and fails the try: with WithCredentialWait, an error that
// is a refusal of the credential (credentialRefusal: an authorization refusal, or a request the
// grant refuses, ErrPublishDenied) is waited out as a refusal of the connect is, and any other ends
// the start at once (endsStart); without it, the constructor returns the error. A start in the
// background (WithConnectInBackground) returns before it has connected, so it runs no check.
//
// cmd/listener's edge listener passes it, with its read of the stream (edgeStreamPresent): its grant
// may only read the stream, so that read is its start's step on the stream, as the ensure is an
// owning client's.
func WithStartCheck(check func(*Client) error) ConnectOption {
	return clientOption(func(o *connectOpts) { o.startCheck = check })
}

type subscriptionTransport uint8

const (
	jetStreamSubscription subscriptionTransport = iota
	coreSubscription
	subscriptionCount
)

// clientStopMode is how the client was stopped, which decides what a bind that ends after the stop
// does with what it bound (endBind).
type clientStopMode uint8

const (
	clientRunning clientStopMode = iota
	// clientDraining: Drain is taking the subscriptions it drains itself. A bind that ends now
	// installs for its next pass.
	clientDraining
	// clientDrainCollected: Drain has taken the last of those subscriptions, just before it drains
	// the connection. A bind that ends now drains what it bound.
	clientDrainCollected
	// clientClosed: Close stopped the client. A bind that ends now unsubscribes what it bound.
	clientClosed
)

type recoverableSubscription struct {
	transport subscriptionTransport
	subject   string
	queue     string
	handler   nats.MsgHandler
	opts      []nats.SubOpt
	active    *nats.Subscription
	// subscribed is the handle Subscribe returned: the one its own bind installed, when that bind
	// succeeded. DrainSubscription names the registration by it; a later restore's bind replaces
	// active and leaves it. When Subscribe's own bind failed (the server refused it, or a background
	// client has not connected yet) it stays nil, and the restore that binds the registration
	// installs only active, a handle no caller holds: the caller gives such a registration up with
	// ReleaseFailed, or its next Subscribe replaces it, never a DrainSubscription.
	subscribed *nats.Subscription
	// binding is set while a bind of this registration is in flight, and closed when it ends. A
	// registration has one bind at a time: a restore leaves one that has a bind in flight, and a
	// registration that replaces it waits for that bind first.
	binding chan struct{}
	// refused is set when the server refused the subscription the client last sent for this
	// registration on its current connection (onAsyncError), and cleared each time it is sent
	// again: by the bind of a registration or of a restore, and by nats.go itself on a reconnect in
	// place (countConnectionEvent). ResendRefusedCore clears it only once the server has accepted
	// what it sent. nats.go keeps a refused subscription valid, so without it a lane the server will
	// not deliver to would read as subscribed. Only the core registration is ever marked.
	refused bool
	// sentOn and sentReconnects name the connection session the subscription was last sent on: the
	// connection and its reconnect count (nats.Conn.Stats) at that send. Each bind records them, and
	// so does each reconnect in place (countConnectionEvent), where nats.go sends every subscription
	// again. ResendRefusedCore answers only a refusal on that session. Once nats.go has reconnected,
	// and until countConnectionEvent runs, the mark it would read is the previous session's, of a
	// subscription nats.go has already sent again to the new server.
	sentOn         *nats.Conn
	sentReconnects uint64
	// pendingMsgs and pendingBytes, when set, are the pending limits of a core subscription,
	// applied on every bind: the role lane's are far above any backlog its handler can build
	// (ConnectRoleLane), so nats.go queues what the server delivers rather than dropping it as a
	// slow consumer, and the connection's buffer is still bounded. Zero keeps nats.go's defaults.
	pendingMsgs, pendingBytes int
}

// Client is a NATS connection that publishes notifications and holds at most one JetStream
// subscription and one core subscription, which it keeps across a reconnect and binds again on a
// connection it dials to replace a closed one. Subscribe and SubscribeCore enforce that: a call on
// the subject (and queue group) the transport already holds replaces its subscription, and any
// other is refused with ErrSubscriptionHeld, so a caller that needs two durables takes one Client
// per durable.
type Client struct {
	Conn *nats.Conn
	// severable is Conn with its dialer, set with Conn (noteSeverable), so Sever reaches them
	// without mu, which callers hold while they wait for nats.go's lock (Close, for one).
	severable                   atomic.Pointer[severableConnection]
	js                          nats.JetStreamContext
	urls                        []string
	name                        string
	connectionSettings          nats.Option
	dialer                      *socks5Dialer
	publishAcknowledgementClock AcknowledgementClock
	reconnectErrClock           func() time.Time
	// reconnectWaitTick, when not nil, runs at each pass of ensureConnWithContext's wait for a
	// reconnect, before the pass blocks; a connection event it causes is read before the wait looks
	// at its context, as one that wakes the wait would be. It is nil outside tests: a test sets it
	// (WithReconnectWaitTick, export_test.go) to know a publish is inside that wait, or to act at
	// the moment the wait ends.
	reconnectWaitTick func()
	// afterInstall, when not nil, runs once an install has set the connected flag and let go of
	// mu (afterInstallForTest). It is nil outside tests: a test sets it to deliver a connection
	// event in that moment.
	afterInstall func(*nats.Conn)
	// beforeRequestFlush, when not nil, runs on the goroutine that flushes a Request's publish,
	// right before that flush begins, while the wait for the request's answer runs beside it
	// (window.request). It is nil outside tests: a test sets it (WithBeforeRequestFlush,
	// export_test.go) to hold the flush until the connection it would flush is gone, the
	// interleaving a flush begun after the connection turned reconnecting has.
	beforeRequestFlush func()
	// beforeRequestWait, when not nil, runs on a Request's wait for its answer each time the wait
	// is about to sleep, with the channel whose close is the loss the wait answers: the end of the
	// life its window pinned (window.request). It is nil outside tests: a test sets it
	// (WithBeforeRequestWait, export_test.go) to end that life only once the wait sleeps on a loss
	// it has not yet seen. Its cost in production is one nil check on each turn of the wait.
	beforeRequestWait func(loss <-chan struct{})
	// beforeCoreSendFlush and afterCoreSendFlush, when not nil, run on the goroutine of a send of the
	// core subscription (sendCore), right before the flush that waits for the server's answer and
	// right after it returns. They are nil outside tests: a test sets them
	// (WithBeforeCoreSendFlush, WithAfterCoreSendFlush, export_test.go) to lose or replace the
	// connection while the send is open, the interleavings in which the answer comes from another
	// session of the connection than the one the send began in.
	beforeCoreSendFlush func()
	afterCoreSendFlush  func()
	// requestExpiry, when not nil, gives each Request the channel its timeout fires on, in place of
	// a timer at the request's deadline, or nil to keep that timer, and a func the request calls
	// once it has settled a flush that returned before its timeout fired, or nil (window.request).
	// It is nil outside tests: a test sets it (WithRequestExpiry, export_test.go) to choose which
	// of the request's timeout and its flush it settles first, so the order of the two is the
	// test's and not the scheduler's.
	requestExpiry func(deadline time.Time) (expired <-chan time.Time, flushSettled func())
	// closedHook, when not nil, runs at the start of onClosed, before the recovery a close starts.
	// It is nil outside tests: a test sets it (WithClosedHook, export_test.go) to hold the work a
	// close sets off, so a caller that returned before that work ran is caught reading too early.
	// closeSettleBound, when above zero, replaces failedStartCloseBound in closeSettled; it is zero
	// outside tests (WithCloseSettleBound, export_test.go), so a test that holds a close can hold it
	// longer than the production bound without the bound ending the wait first.
	closedHook       func()
	closeSettleBound time.Duration
	mu               sync.Mutex

	// eventWork counts the goroutines the client's connection events start, each recovery
	// (recover, from onClosed and onReconnect) and the goroutine that works through the queued
	// reconnects (workThroughReconnects, from queueReconnect), so the close of a failed start can
	// wait for them and for every line they log (closeSettled).
	eventWork sync.WaitGroup
	// lastClosed is the last connection whose close onClosed has handled, and closeWatch the close
	// closeSettled waits for, both under mu (closeHandled). lastClosed keeps that one closed
	// connection from the collector until the next close replaces it.
	lastClosed *nats.Conn
	closeWatch *closeWatch

	// deniedPublishes holds, under mu, the server's refusals of publishes per subject it refused a
	// publish to, as the connection reported them (recordPublishDenial), queued in arrival order
	// for the sends of that subject to take one each (deniedPublish). nats.go keeps one last error
	// per connection, so a publish window that sent several denied subjects at once could name
	// only the violation that landed last and left every other denied send to its acknowledgement
	// timeout; and nats-server reports one violation per refused message, so a record of one
	// violation per subject would leave every send but one of a subject a window sent twice to
	// that same timeout. It belongs to the connection in Conn: a reconnect clears it
	// (clearPublishDenials, from countConnectionEvent under c.mu), and install, which runs only
	// from the constructor before any connection is the client's, finds it empty. A connection
	// ensureConnWithContext dials after a close does not clear it, and need not: a send takes only
	// a violation of its own subject recorded at or after its own send time (deniedPublish), so a
	// violation the closed connection left can answer a send on the new one only when that send's
	// clock was read before the replacement, and then it is a refusal of the same subject under
	// the same credentials. A record retention has spent and anything past publishDenialDepth are
	// dropped when the next one is recorded, and a read takes the violation it answers with out.
	deniedPublishes map[string][]publishViolation
	// refusal is the channel closed when the next refusal of a publish is recorded, which every send
	// waiting on an answer selects on beside it (refusalSignal): recordPublishDenial closes it and
	// puts a new one in its place, so a refused send is answered as the refusal arrives rather than
	// at the end of its own wait. It is atomic, apart from c.mu, since a send takes it with one load.
	refusal atomic.Pointer[chan struct{}]
	// installing is the connection install ensures the stream on before it becomes Conn, whose
	// refusals are recorded as Conn's are (recordPublishDenial); nil outside that ensure. c.mu
	// guards it.
	installing *nats.Conn

	// subscriptionsMu guards subscriptions and stopMode. It is held to read and write them, never
	// across a bind's request to the server, so a reconnect's restore, the health reads and Drain
	// never wait out a request the reconnect lost.
	subscriptionsMu sync.Mutex
	subscriptions   [subscriptionCount]recoverableSubscription
	stopMode        clientStopMode

	reconnectHooksMu sync.Mutex
	reconnectHooks   []func(context.Context, *nats.Conn) error
	// disconnectHooks run on each disconnect of the client's connection (AddDisconnectHook), under
	// reconnectHooksMu as the reconnect hooks are.
	disconnectHooks []func()
	// drainHooks run once in Drain, after the delivery subscriptions and before the connection's
	// own drain (AddDrainHook), under reconnectHooksMu as the reconnect hooks are.
	drainHooks []func(context.Context)
	// rewatchMu holds a run of the reconnect hooks (rewatch), so two never run at once.
	rewatchMu sync.Mutex
	// connEvents counts the connection events the reconnect hooks follow, under mu: each connection
	// the client installs after its first, and each reconnect of the current one in place.
	// hookedEvents is the count the last run that succeeded began at. The hooks are due while the
	// two differ.
	connEvents   uint64
	hookedEvents uint64
	// hooksDueSince is when the oldest connection event the hooks have not covered was counted,
	// under mu, and the zero time while the hooks are current: a second event that lands while they
	// are already due keeps the first one's time, since the stall HookStall reports is how long the
	// state behind the hooks, a KV cache among it, has been off its buckets, not how long the last
	// event has waited. lastHookErr is the error of the last run that failed, so the stall is
	// reported with the reason for it.
	hooksDueSince time.Time
	lastHookErr   error
	// drainedReconnects is how many times c.Conn had reconnected in place when Drain stopped the
	// client, set under mu in the step that stops it, so a reconnect callback can tell a reconnect
	// before the drain from one during it (closeStopped). Once the client is stopped c.Conn never
	// changes (ensureConnWithContext), so the count needs no connection beside it.
	drainedReconnects uint64
	// awaitingConnection is true, under mu, from the constructor of a client that connects in the
	// background (WithConnectInBackground) until nats.go has connected its connection for the
	// first time (onConnect), and false for every other client.
	awaitingConnection bool
	// connChanged, under mu, is closed at the client's next connection event and cleared: a
	// connection installed, and a disconnect, reconnect or close of a connection the client dialled.
	// ConnectionChanged makes the next one when a caller takes it.
	connChanged chan struct{}

	// reconnectWaits counts the waits ensureConnWithContext makes for a connection nats.go is
	// reconnecting, one per pass of its loop that waits; a test reads it to bound them.
	reconnectWaits atomic.Uint64

	// connectedConn is the connection the client serves with while it is connected, and nil while
	// it is not. It is outside mu, and outside nats.go's own connection lock, so Connected
	// answers without waiting on either: nats.go holds its connection's lock across every
	// reconnect dial and the connect init after it, which a route that drops the process's
	// packets leaves hanging for the connect timeout, and nats.Conn.Status waits for that lock.
	// cmd/listener's delivery lease reads this, through ConnectedConn, from the loop that renews
	// the machine's lease record (lease.SetReceiveSignal), so a read that waits stalls the
	// renewals with it.
	//
	// The connection's own events are its only source: nothing asks a connection whether it is
	// connected, since that read waits on the lock above. Every connect, reconnect, disconnect and
	// close callback records what it reports for its connection (noteConnection, the state callback
	// of options), installed or not, and moves this field only for the connection the client has
	// installed. An install (markConnected) sets the field from what its connection's events have
	// reported so far, or from how the dial returned when they have reported nothing yet. Both
	// writers take connFlagMu, so an event lands wholly before an install or wholly after it: one
	// before is in the record the install reads, and one after moves the field itself. A late
	// callback of a connection the client has replaced records only into the record, which the next
	// install prunes. Readers take no lock: Connected and ConnectedConn load this field alone.
	connectedConn atomic.Pointer[nats.Conn]
	// connFlagMu orders the writers of connectedConn (noteConnection, markConnected). installedConn
	// is the connection the client last made c.Conn, and connUp what each connection's events have
	// reported last, connected or not; both are under connFlagMu. No holder of connFlagMu calls
	// into nats.go or takes mu, so a callback never waits behind a dial or the client's lock.
	connFlagMu    sync.Mutex
	installedConn *nats.Conn
	connUp        map[*nats.Conn]bool

	// reconnectQueue holds the connections whose connection event has still to be worked through
	// (onReconnect), oldest first, and reconnectRunning whether a goroutine of the client's is
	// working through it; both are under reconnectQueueMu. The work of a connection event -
	// restoring the subscriptions and running the reconnect hooks, which make requests of the
	// server - must not run on nats.go's callback goroutine: that goroutine runs every callback
	// of the connection one at a time, so a disconnect that lands during the work would wait
	// behind it and the connected flag would read up throughout (queueReconnect).
	reconnectQueueMu sync.Mutex
	reconnectQueue   []*nats.Conn
	reconnectRunning bool

	// streamConfig is the stream an owning client ensures on its first connect and on every
	// connection event after it (ensureStreamOn); nil for a client that owns no stream.
	streamConfig *nats.StreamConfig
	// deployedSubjects holds the read of the deployed stream's subjects the client keeps
	// (DeployedStreamCaptures, keepSubjects): the subjects that read answered, and when it started
	// among the client's reads; nil until the client has kept one.
	deployedSubjects atomic.Pointer[subjectsRead]
	// subjectsReadSeq numbers the client's reads of the deployed stream's subjects in the order they
	// start, an ensure's (ensureStream) among them, so a read that ends after one that started
	// after it keeps nothing over it (keepSubjects).
	subjectsReadSeq atomic.Uint64
	// beforeSubjectsKept, when not nil, runs once a read of the deployed stream's subjects has its
	// answer and before keepSubjects keeps it. It is nil outside tests: a test sets it to hold a
	// read there while a read that started after it ends.
	beforeSubjectsKept func()
	// subjectsRefused is set once the server refused a read of the deployed stream's subjects under
	// the NATS user's permissions (readDeployedSubjectsAs): the client then reads them no more.
	subjectsRefused atomic.Bool
	// subjectsRereading is the read of the deployed subjects a judgement has in flight
	// (startRereadLocked: a fresh judgement's, subjectsReadSince, or a publish transport's,
	// rereadForTransport), closed when it ends, and nil while none is; subjectsRereadSeq is that
	// read's number among the client's reads (subjectsReadSeq), subjectsRereadStarted is when it
	// started, and subjectsRereadAt is when the last such read ended, zero before the first. All
	// four are under subjectsRereadMu.
	subjectsRereadMu      sync.Mutex
	subjectsRereading     chan struct{}
	subjectsRereadSeq     uint64
	subjectsRereadStarted time.Time
	subjectsRereadAt      time.Time
	// slowConsumers counts the slow-consumer errors the client's connections reported
	// (SlowConsumers).
	slowConsumers atomic.Uint64
	// coreQueues, set by ConnectRoleLane, are the queue groups the core subscription may be sent in,
	// in the order ResendRefusedCore tries them: the shared group first, then the machine's, as the
	// lane's start does. It is nil on any other client, whose core subscription keeps the queue group
	// it registered.
	coreQueues []string
	// streamMessageReads is WithStreamMessageReads: every KV handle opened on the client's
	// connection reads with a stream message get rather than a direct get (wrapKeyValue).
	streamMessageReads bool
	// keeperStepGuard is withKeeperStepGuard's hang guard of each keeper step, zero for
	// startHangGuard (keepPass). It is set before the keeper starts and never changed.
	keeperStepGuard time.Duration

	// recovery state
	recovering int32
	stopCh     chan struct{}
	closeOnce  sync.Once

	// keeperDone is closed when the keeper of a ConnectOwningStream client ends (keep), nil on a
	// client that runs none, or none yet: one that connects in the background starts it on its first
	// ensure of the stream that succeeds (startKeeperOnce). It is written and read under mu
	// (startKeeper, awaitKeeper).
	keeperDone chan struct{}
	// keptBuckets are the buckets the keeper raises every period: the ones the program named at
	// its connect (WithKeptBuckets) and the ones it opened since (KeepBucket). They are read and
	// written under mu, so a pass takes the list as it stands when it starts.
	keptBuckets []string
	// keepPasses counts the keeper's passes that have ended, a pass that failed included (keep,
	// keepPass), for a test that must know a pass ran after a change it made: a pass that finds
	// nothing to restore logs nothing and sends no update, so the count is the one sign it ran. It
	// says nothing of whether the pass did what it set out to; a failed pass logs that.
	keepPasses atomic.Uint64
}

// Dial opens a tuned core NATS connection using envoy's standard options (5s connect timeout,
// infinite reconnect every second, retry-loop for the initial 10 attempts), as the NATS user
// environment names (credential), through the SOCKS5 proxy it names (socks5Proxy). A caller
// that needs core pub/sub on a connection of its own uses it: nats.go re-subscribes core
// subscriptions on reconnect by itself, so it needs none of Client's recovery, and it touches no
// stream. It refuses a NATS server that is not this machine's on the same terms as Connect,
// reading ENVOY_ALLOW_REMOTE_NATS from environment too, which answers as os.LookupEnv does
// (WithEnvironment).
//
// It takes the DialOptions: WithConnectInBackground, which returns the connection before it has
// connected (a core subscription made on it before then is sent once it connects),
// WithSilentCutDetection, WithPromptReconnect and WithReconnectErrClock.
//
// For JetStream-backed publishing or a durable consumer, use Connect; to reconcile the stream
// this codebase owns, ConnectOwningStream.
func Dial(name string, urls []string, environment func(string) (string, bool), options ...DialOption) (*nats.Conn, error) {
	opts := parseConnectOptions(options)
	name = connectionName(name)
	if err := refuseRemoteNATS(urls, environment); err != nil {
		return nil, err
	}
	credential, err := credential(environment)
	if err != nil {
		return nil, err
	}
	dialer, err := socks5Proxy(environment)
	if err != nil {
		return nil, err
	}
	settings := opts.connectionSettings(credential)
	dial := func(first nats.Option) (*nats.Conn, error) {
		return connect(name, urls, allOf(settings, first), dialer, connectCallbacks{}, opts.reconnectErrClock)
	}
	if opts.background {
		return connectFirstInBackground(name, urls, opts.reconnectErrClock, dial, nil, nil)
	}
	return dial(nil)
}

// Connect opens a client that publishes and subscribes without touching the shared
// ENVOY_NOTIFICATIONS stream. Every caller that only publishes or only tails uses it -- natstail,
// the MCP bridge and envoy-dispatch's operator commands own nothing on the server they reach, and
// a stream ensure from one of them rewrites a resource several deployments share (LEGION-249).
// envoy-dispatch's delivery intakes use it too: each binds its durable on the stream Dispatch's
// publisher owns (ConnectOwningStream), and leaves the stream itself alone. It reads the subjects
// of the deployed stream at connect (at its first connection, with WithConnectInBackground),
// waiting at most firstStreamSubjectsRead for them, and again every streamSubjectsRefresh, so it
// publishes each topic on the transport the deployed stream calls for (UsesCoreTransport).
//
// Every client, from Connect or ConnectOwningStream, connects as the NATS user the environment
// names: an nkey seed (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED) or a service's platform token
// (NATS_SERVICE_TOKEN_FILE or NATS_SERVICE_GOOGLE_AUDIENCE; credential). An unusable one is an error
// before any dial; none set connects without a credential. It connects through the
// SOCKS5 proxy the environment names (ENVOY_NATS_SOCKS5_PROXY, socks5Proxy), or directly when it
// names none. Every connection the client dials later, to recover, is the same user's, through
// the same proxy.
//
// For the JetStream stream this codebase owns, use ConnectOwningStream.
func Connect(urls []string, options ...ConnectOption) (*Client, error) {
	return newClient(urls, false, parseConnectOptions(options))
}

// ConnectOwningStream opens a client that also ensures ENVOY_NOTIFICATIONS: it reconciles the
// stream with this binary's subjects, retention and duplicate window and the deployment's replica
// count and byte limit (WithReplicas, WithStreamMaxBytes; ensureStreamWithConfig), or creates it
// when the server has none. It ensures the stream when it connects, and again after every
// reconnect and every connection it dials to replace a closed one, so a stream lost while the
// process runs is created again. A failed ensure at connect fails the connect, at the server's
// first refusal when its grant refuses a request of the ensure (with WithConnectInBackground there
// is none: the first connection's ensure is a reconnect hook); with WithCredentialWait the start
// waits out such a refusal, and one of the connect, for up to the wait's bound. A failed ensure
// later is retried by the client's recovery, as a failed reconnect hook is (AddReconnectHook).
//
// The client then keeps the stream until it stops: every 30 seconds its keeper puts back the
// copies, the byte limit, the stream's id, the age, the duplicate window and discard that a
// concurrent start or an operator took away, adds back each of this binary's subjects the stream
// lacks that overlaps none it carries, and removes no subject (keepStream). It also raises each
// bucket WithKeptBuckets names that has fewer copies than WithReplicas' count (raiseReplicas).
//
// Every process of the deployed services calls it: envoy-listener and envoy-dispatch's server.
// It also reads the deployed stream's subjects every streamSubjectsRefresh, as Connect's client does.
func ConnectOwningStream(urls []string, options ...ConnectOption) (*Client, error) {
	return newClient(urls, true, parseConnectOptions(options))
}

// newClient makes the client Connect and ConnectOwningStream return, from what every try of its
// start shares: the servers it may reach (refuseRemoteNATS), and the credential and the proxy the
// environment names, each refused before any dial. A start in the foreground (credentialStart)
// with WithCredentialWait waits out a refusal of the credential (waitOutCredentialRefusals), each
// of its tries a client of its own whose start dials once (startClient) and whose check then runs
// (WithStartCheck, checkedStart), and logs each refusal at WARN with the wait that follows it. A
// try whose dial failed otherwise counts toward the ten tries in a row of a start without the
// wait, whatever the bound; a try whose install (an owning client's ensure of the stream among it)
// or start check failed otherwise ends the start at once (endsStart), as such a failure ends a
// start without the wait: no wait repairs a stream the server will not take, and the ensure's
// hang guard has already waited out its whole bound. So does an ensure the grant refuses
// (startRefusal): its connect got in. Every other start in the foreground is one
// startClient that dials up to connectTries times, connectRetryWait after each dial that fails, and
// whose check then runs. A start in the foreground that got in reports it to WithCredentialWait's
// admitted before newClient returns.
func newClient(urls []string, ownsStream bool, opts connectOpts) (*Client, error) {
	if err := refuseRemoteNATS(urls, opts.environment); err != nil {
		return nil, err
	}
	credential, err := credential(opts.environment)
	if err != nil {
		return nil, err
	}
	dialer, err := socks5Proxy(opts.environment)
	if err != nil {
		return nil, err
	}
	settings := opts.connectionSettings(credential)
	if opts.background {
		client, _, err := startClient(urls, ownsStream, opts, settings, dialer, connectTries, connectRetryWait)
		return client, err
	}
	client, err := credentialStart(urls, ownsStream, opts, settings, dialer)
	if err == nil && opts.credentialAdmitted != nil {
		opts.credentialAdmitted()
	}
	return client, err
}

// credentialStart is newClient's start in the foreground: one checkedStart of up to connectTries
// dials without WithCredentialWait's bound, and the start that waits out refusals of the
// credential with it.
func credentialStart(urls []string, ownsStream bool, opts connectOpts, settings nats.Option, dialer *socks5Dialer) (*Client, error) {
	if opts.credentialWait <= 0 {
		client, _, err := checkedStart(urls, ownsStream, opts, settings, dialer, connectTries, connectRetryWait)
		return client, err
	}
	refused := func(err error, next time.Duration) {
		if next > 0 {
			slog.Warn("envoy nats credential refused at start; trying again",
				slog.String("name", opts.name),
				slog.String("servers", serverList(urls)),
				slog.String("error", err.Error()),
				slog.Duration("wait", next))
		}
		if opts.credentialRefused != nil {
			opts.credentialRefused(err)
		}
	}
	return waitOutCredentialRefusals(opts.credentialWait, refused, wallStartClock, func() (*Client, error) {
		client, failedAt, err := checkedStart(urls, ownsStream, opts, settings, dialer, 1, 0)
		if err != nil && failedAt != stepDial && !startRefusal(failedAt, err) {
			return nil, endsStart{err}
		}
		return client, err
	})
}

// startRefusal reports whether err, the error of a try's install or start check (failedAt), is a
// refusal of the credential the start waits out: the server refusing it (authorizationRefusal) at
// either step, and a request the credential's grant refuses (ErrPublishDenied) at the start check
// alone, the one an edge listener's read of the stream makes in place of an ensure. A refusal of
// an owning client's ensure under the grant is the grant's, which only a change of the grant lifts:
// the connect got in, and the caller tries again by its own rule (cmd/listener's connectListener).
func startRefusal(failedAt startStep, err error) bool {
	if failedAt == stepInstall {
		return authorizationRefusal(err)
	}
	return credentialRefusal(err)
}

// startStep is the step of a start's try that failed (startClient, checkedStart): its dial, its
// install of the connection, which ensures an owning client's stream, or its start check.
type startStep uint8

const (
	stepDial startStep = iota
	stepInstall
	stepCheck
)

// checkedStart is startClient in the foreground followed by the start check (WithStartCheck), when
// the start made a client and the options name a check. It answers the client, or the error of the
// start or of the check and the step that failed; a client whose check failed is closed first, and
// checkedStart returns once its close's callbacks and recovery have run, or, when one hangs, once
// closeSettled's bound has passed and it has logged a WARN (closeFailedStart).
func checkedStart(urls []string, ownsStream bool, opts connectOpts, settings nats.Option, dialer *socks5Dialer, tries int, wait time.Duration) (*Client, startStep, error) {
	client, failedAt, err := startClient(urls, ownsStream, opts, settings, dialer, tries, wait)
	if err != nil || opts.startCheck == nil {
		return client, failedAt, err
	}
	if err := opts.startCheck(client); err != nil {
		client.closeFailedStart()
		return nil, stepCheck, err
	}
	return client, stepCheck, nil
}

// startClient is one start of the client newClient returns, over the connection settings and the
// proxy newClient read, answering the step that failed beside its error. An owning client ensures
// the stream (ensureStreamWithConfig) and then starts its keeper (startKeeper): once install has
// returned, or, connecting in the background (WithConnectInBackground), in the first connection's
// reconnect hook once its ensure has succeeded (startKeeperOnce), so its keeper runs no pass
// before that ensure. A start in the foreground dials up to tries times, waiting wait after each
// dial that fails (connectTrying), and one that fails stops the client it made before it closes the
// connection, so nothing of a failed try runs on: the next try of a start that waits out refusals
// makes a client afresh.
func startClient(urls []string, ownsStream bool, opts connectOpts, settings nats.Option, dialer *socks5Dialer, tries int, wait time.Duration) (*Client, startStep, error) {
	c := &Client{
		urls:                        urls,
		name:                        connectionName(opts.name),
		connectionSettings:          settings,
		dialer:                      dialer,
		publishAcknowledgementClock: opts.publishAcknowledgementClock,
		reconnectErrClock:           opts.reconnectErrClock,
		reconnectWaitTick:           opts.reconnectWaitTick,
		beforeRequestFlush:          opts.beforeRequestFlush,
		beforeRequestWait:           opts.beforeRequestWait,
		beforeCoreSendFlush:         opts.beforeCoreSendFlush,
		afterCoreSendFlush:          opts.afterCoreSendFlush,
		streamMessageReads:          opts.streamMessageReads,
		keeperStepGuard:             opts.keeperStepGuard,
		requestExpiry:               opts.requestExpiry,
		closedHook:                  opts.closedHook,
		closeSettleBound:            opts.closeSettleBound,
		stopCh:                      make(chan struct{}),
		keptBuckets:                 opts.keptBuckets,
	}
	stream := opts.streamConfig()
	if ownsStream {
		c.streamConfig = &stream
	}
	dial := func(first nats.Option) (*nats.Conn, error) {
		return connectTrying(context.Background(), tries, wait, c.name, urls, allOf(c.connectionSettings, first), dialer, c.callbacks(), c.reconnectErrClock)
	}
	if opts.background {
		c.awaitingConnection = true
		// The first connection is a connection event, so the hooks this registers before the
		// caller can add one of its own do what the constructor of a client that waits does at
		// connect: an owning client ensures the stream, and every other reads the deployed
		// subjects. The keeper starts once the first ensure has succeeded (startKeeperOnce), not
		// here: a pass before it could write back a subject the ensure replaced.
		if ownsStream {
			c.AddReconnectHook(c.startKeeperOnce(opts.keepInterval))
		} else {
			c.AddReconnectHook(c.readDeployedSubjectsUntilRead)
		}
		// onConnect counts the first connection only once it is c.Conn, which install makes it. An
		// install that fails returns no client, so it stops the client before
		// connectFirstInBackground closes the connection, as the foreground path does below. At
		// nats.go v1.55.0 it cannot fail: JetStream with these options returns no error, and
		// there is no stream to ensure here. The stop guards a nats.go whose JetStream can. The
		// dial returned a connection nats.go has yet to connect, so it reads disconnected until its
		// first connect reports itself (markConnected).
		install := func(nc *nats.Conn) error {
			if err := c.install(nc, false, false); err != nil {
				c.stop()
				return err
			}
			return nil
		}
		if _, err := connectFirstInBackground(c.name, urls, c.reconnectErrClock, dial, c.onConnect, install); err != nil {
			return nil, stepInstall, err
		}
		go c.followDeployedSubjects(opts.streamSubjectsRefresh)
		return c, stepInstall, nil
	}
	nc, err := dial(nil)
	if err != nil {
		return nil, stepDial, err
	}
	if err := c.install(nc, ownsStream, true); err != nil {
		// The constructor returns no client, so no caller can Close it: the client is stopped
		// before nc closes, so the recovery nc's close begins (onClosed) finds it stopped and dials
		// nothing. Running, that recovery would dial and install a connection held by no one, which
		// with MaxReconnect -1 reconnects for the rest of the process once its server is gone. The
		// constructor returns once that recovery, and nats.go's callbacks for the close, have run
		// (closeSettled), so none of their lines follows what its caller writes next, unless one of
		// them hangs past closeSettled's bound (connectTimeout), when it logs a WARN and returns, and
		// a line of that close can then follow.
		c.stop()
		c.closeSettled(nc, nc.Close)
		return nil, stepInstall, err
	}
	readable := true
	if ownsStream {
		// Registered before the caller can add a hook of its own, so after a connection event the
		// stream is ensured before the hooks that rewatch state kept on it.
		c.AddReconnectHook(c.ensureStreamOn)
		c.startKeeper(stream, opts.keepInterval)
	} else {
		readable = c.readDeployedSubjects(context.Background(), nc, firstStreamSubjectsRead)
	}
	if readable {
		go c.followDeployedSubjects(opts.streamSubjectsRefresh)
	}
	return c, stepInstall, nil
}

// install makes nc the client's connection, with the JetStream context taken from it, once it has
// ensured the client's stream on nc (ensureStream) when ensure is true. The ensure is told
// whether nc's server caps a stream that sets no consumer cap at a default of its own, read from
// the version nc reports (ensureStreamReported), which waits out a reconnect, until the client
// stops or natsversion.VersionWait passes, rather than decide on the empty version one answers:
// install with ensure runs in the constructor's foreground start alone, never on nats.go's
// callback goroutine (the background start installs with no ensure). Installing it is a
// connection event (ConnectionChanged). dialedUp is whether the dial returned nc connected, which
// the connected flag takes until nc's own events say otherwise (markConnected). It is also where
// the client enters the registry of live clients (registerClient), the constructor's own step:
// install runs only from the constructor and only a client it returns gets this far, so a caller
// holding a connection can find the client that owns it (clientOwning) for as long as that client
// is a caller's to use.
func (c *Client) install(nc *nats.Conn, ensure, dialedUp bool) error {
	js, err := nc.JetStream(nats.MaxWait(10*time.Second), nats.PublishAsyncTimeout(publishAcknowledgementTimeout))
	if err != nil {
		return err
	}
	if ensure {
		// The server's refusals on nc are recorded while the stream is ensured on it, before nc is
		// c.Conn (recordPublishDenial), so a request of the ensure the grant refuses ends at once.
		c.mu.Lock()
		c.installing = nc
		c.mu.Unlock()
		ctx, cancel := c.untilStopped()
		err := c.ensureStreamReported(ctx, ctx, nc, js, atStart)
		cancel()
		c.mu.Lock()
		c.installing = nil
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.Conn, c.js = nc, c.readsOwnRefusals(nc, js)
	c.markConnected(nc, dialedUp)
	c.noteSeverable(nc)
	c.connectionChangedLocked()
	// install runs only from the constructor, before any connection is c.Conn, so the record holds
	// at most what the stream's ensure on nc left unread (recordPublishDenial records for the
	// connection being installed too), and nothing of it is owed to a send: it is cleared.
	c.clearPublishDenials()
	c.mu.Unlock()
	registerClient(c)
	c.afterInstallForTest(nc)
	return nil
}

// streamSubjectsRefresh is how often a client reads the deployed stream's subjects again: a newer
// build that adds a subject while this process runs is followed within it.
const streamSubjectsRefresh = 60 * time.Second

// firstStreamSubjectsRead bounds the read of the deployed stream's subjects that a client that owns
// no stream makes at connect. A server answers it at once, unless the NATS user may not read the
// stream: the server then drops the request, and the read waits out this bound. A client that
// connects in the foreground waits for it at connect, so the server's report of that refusal
// reaches the connection before the client's first publish. A core publish whose flush the report
// reached would otherwise fail as unconfirmed, since the report names another subject
// (confirmPublished, publishDenial); a JetStream publish reads only its own subject's refusal and
// is not affected. A client that connects in the background (WithConnectInBackground) makes this
// read in a hook of its first connection, and its publishes do not wait for it. It bounds each
// read a judgement makes again (startRereadLocked) too, which a fresh judgement waits for
// (SubjectsAsk.Uncaptured), and how long such a read counts as in flight for a publish's
// transport, which answers on the subjects the client holds meanwhile rather than wait
// (PublishTakesJetStream).
const firstStreamSubjectsRead = 2 * time.Second

// streamSubjectsRead bounds each read of the deployed stream's subjects followDeployedSubjects
// makes.
const streamSubjectsRead = 10 * time.Second

// streamSubjectsRereadGap is the least time from the end of one read of the deployed stream's
// subjects a judgement makes again (startRereadLocked) to the start of the next. It bounds the
// server's load: however many judgements of topics the stream does not capture come at once - a
// registration naming several, every session following an ad-hoc topic at its heartbeat, a
// publish of each - the client sends at most one STREAM.INFO per gap. A fresh judgement inside the
// gap waits out the rest of it and reads (SubjectsAsk.Uncaptured), so it costs the caller up to
// this much more and never answers on a read made before it asked. A publish's transport answers
// on the subjects the client holds inside the gap (PublishTakesJetStream).
const streamSubjectsRereadGap = 250 * time.Millisecond

// ensureStream ensures the stream through js, for at, the caller that ensures it (streamEnsure),
// and keeps the subjects it carries afterwards as a read of them like any other: numbered as it
// starts, and kept unless the client already keeps a read that started after it (keepSubjects).
// The ensure's own list is what it wrote, after a read of its own, or what that read answered
// when it wrote nothing (the stream needed no change, or a newer
// build's generation owns its settings). A read the client began between the ensure's read and its
// write can be answered before the write lands, so when a read began while the ensure ran, the
// subjects are read back once the ensure ends, numbered as that read-back starts: a read answered
// before the write started before it and keeps nothing over it. An ensure no read began during
// keeps its own list, numbered as the ensure started, as does one whose read-back fails.
// defaultsAnUnsetCap is what the connected server is, read from the version it answers
// (serverDefaultsAnUnsetCap): whether a deployed stream with no consumer cap is one the server
// caps at a default of its own. ctx ends the ensure's waits on the server, which the start's hang
// guard bounds too (ensureStreamWithConfig).
func (c *Client) ensureStream(ctx context.Context, js nats.JetStreamContext, defaultsAnUnsetCap bool, at streamEnsure) error {
	started := c.subjectsReadSeq.Add(1)
	subjects, err := ensureStreamWithConfig(ctx, js, c.streamConfig, defaultsAnUnsetCap, at)
	if err != nil {
		return err
	}
	if c.subjectsReadSeq.Load() != started {
		readBack := c.subjectsReadSeq.Add(1)
		if read, err := readStreamSubjects(js); err == nil {
			started, subjects = readBack, read
		}
	}
	c.keepSubjects(started, subjects)
	return nil
}

// ensureStreamOn is an owning client's reconnect hook: it ensures the stream on conn after each
// connection event, so a stream lost while the process runs is created again. It reads the
// version off the connection nats.go hands it, so the ensure asks the server it writes to whether
// a stream with no consumer cap is one that server caps (ensureStreamReported), and it looks once
// and waits for nothing: a connection that reports no version has dropped again, and a run of the
// hooks that waited out that outage would keep the next reconnect's run behind it (rewatch). A
// connection reports no version only when it is not CONNECTED, so the run that met it is
// superseded (runSuperseded): it starts no recovery and leaves the hooks due, and the run the next
// connection event brings makes the ensure again (onReconnect, onClosed).
//
// Every request of the ensure is made under ctx, the run's context (whileConnected), and each but
// the role-lane migration's consumer listing (js.ConsumerNames) also ends as soon as conn is lost
// under it (connectionBoundJetStream), so one sent on a connection that drops ends with it, and the
// ensure's waits on the server end with ctx too. The hooks run one at a time and this one runs
// first, so a request whose answer died with a connection that dropped again mid-ensure would
// otherwise hold every hook after it - the KV rewatch, the listener's receive - for the JetStream
// context's whole 10 s wait. It waits for no election it did not cause (atReconnect): a stream it
// finds with no leader is left to elect one, so the hooks after it run, unless the stream must be
// reconciled, an update that fails at once while the cluster has no quorum, as at main; a create or
// a raise it makes is waited out.
func (c *Client) ensureStreamOn(ctx context.Context, conn *nats.Conn) error {
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err == nil {
		err = c.ensureStreamReported(lookOnce(), ctx, conn, connectionBoundJetStream{JetStreamContext: js, client: c, ctx: ctx}, atReconnect)
	}
	if err != nil {
		return fmt.Errorf("ensure stream %s: %w", Stream, err)
	}
	return nil
}

// connectionBoundJetStream is a JetStream context on the client's connection whose requests the
// stream's ensure makes (ensureStreamWithConfig: the stream's read, create, update and purge, and
// the consumer reads and updates of the role-lane migration) each end at ctx's end, the run's
// context a reconnect hook is given (whileConnected), and with ErrRequestLostWithConnection as
// soon as that connection is lost before the answer comes (RequestOnConnection), rather than
// waiting out MaxWait for an answer the server dropped with the old socket. The ensure counts a
// request lost that way as unanswered: a read or a create is sent again at once on the connection
// that came back (sentAgain), and an update is read back there (updateStream). The role-lane
// migration's consumer listing (ConsumerNames) ends at ctx's end alone; every other call is the
// JetStream context's own.
type connectionBoundJetStream struct {
	nats.JetStreamContext
	client *Client
	ctx    context.Context
}

// boundRequest makes call, a request of js's JetStream context, on its client's connection under
// js.ctx (RequestOnConnection), handing call opts with the request's context appended in a slice
// of its own, so the caller's opts are left as they were.
func boundRequest[T any](js connectionBoundJetStream, opts []nats.JSOpt, call func(...nats.JSOpt) (T, error)) (T, error) {
	return RequestOnConnection(js.ctx, js.client, func(ctx context.Context) (T, error) {
		return call(append(slices.Clip(opts), nats.Context(ctx))...)
	})
}

func (js connectionBoundJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return boundRequest(js, opts, func(opts ...nats.JSOpt) (*nats.StreamInfo, error) {
		return js.JetStreamContext.StreamInfo(stream, opts...)
	})
}

func (js connectionBoundJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return boundRequest(js, opts, func(opts ...nats.JSOpt) (*nats.StreamInfo, error) {
		return js.JetStreamContext.AddStream(cfg, opts...)
	})
}

func (js connectionBoundJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return boundRequest(js, opts, func(opts ...nats.JSOpt) (*nats.StreamInfo, error) {
		return js.JetStreamContext.UpdateStream(cfg, opts...)
	})
}

func (js connectionBoundJetStream) PurgeStream(name string, opts ...nats.JSOpt) error {
	_, err := boundRequest(js, opts, func(opts ...nats.JSOpt) (struct{}, error) {
		return struct{}{}, js.JetStreamContext.PurgeStream(name, opts...)
	})
	return err
}

func (js connectionBoundJetStream) ConsumerInfo(stream, name string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return boundRequest(js, opts, func(opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.ConsumerInfo(stream, name, opts...)
	})
}

func (js connectionBoundJetStream) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return boundRequest(js, opts, func(opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

func (js connectionBoundJetStream) ConsumerNames(stream string, opts ...nats.JSOpt) <-chan string {
	return requestsUnder(js.ctx, js.JetStreamContext).ConsumerNames(stream, opts...)
}

// ensureStreamReported ensures the stream through js with what conn's server reports it is
// (connectedDefaultsAnUnsetCap), waiting for that report until versionCtx is done or
// natsversion.VersionWait passes; a connection that reports none in that time ensures nothing and
// answers the error. ctx ends the ensure itself, made for at (ensureStream).
func (c *Client) ensureStreamReported(versionCtx, ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, at streamEnsure) error {
	defaultsAnUnsetCap, err := connectedDefaultsAnUnsetCap(versionCtx, conn)
	if err != nil {
		return err
	}
	return c.ensureStream(ctx, c.readsOwnRefusals(conn, js), defaultsAnUnsetCap, at)
}

// lookOnce is a context already done, for a read of the server version that must not wait: given
// it, natsversion.ConnectedVersion answers at its first look.
func lookOnce() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// untilStopped is a context that ends when the client stops (Close or Drain), for a wait that a
// stop must not outlast; the caller cancels it when the wait is over.
func (c *Client) untilStopped() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// readDeployedSubjects reads the deployed stream's subjects on conn within timeout and within ctx,
// numbered as it starts (readDeployedSubjectsAs).
func (c *Client) readDeployedSubjects(ctx context.Context, conn *nats.Conn, timeout time.Duration) bool {
	return c.readDeployedSubjectsAs(ctx, conn, timeout, c.subjectsReadSeq.Add(1))
}

// readDeployedSubjectsAs reads the deployed stream's subjects on conn within timeout, and within
// ctx, and keeps them for DeployedStreamCaptures as the read numbered started (subjectsReadSeq),
// unless the client already keeps a read of them that started after this one did (keepSubjects).
// started is taken before the read's request is sent, so a request that asked before it was taken
// asked before this read did (SubjectsAsk). A read that fails keeps what the client held. It
// reports false only when the server refused the read under the NATS user's permissions: such a
// user cannot read them later either, so the client keeps deciding by what it holds, and reads no
// more (subjectsRefused).
func (c *Client) readDeployedSubjectsAs(ctx context.Context, conn *nats.Conn, timeout time.Duration, started uint64) bool {
	js, err := conn.JetStream(nats.MaxWait(timeout))
	if err == nil {
		var subjects []string
		if subjects, err = readStreamSubjects(readsRefusals(conn, requestsUnder(ctx, js))); err == nil {
			c.keepSubjects(started, subjects)
			return true
		}
	}
	if errors.Is(err, ErrPublishDenied) || publishDenial(conn.LastError(), streamInfoSubject) != nil {
		slog.Info("envoy nats may not read the stream's subjects; the client decides each topic's transport by the subjects it compiles",
			slog.String("name", c.name), slog.String("subject", streamInfoSubject))
		c.subjectsRefused.Store(true)
		return false
	}
	slog.Debug("envoy nats could not read the stream's subjects", slog.String("name", c.name), slog.String("error", err.Error()))
	return true
}

// subjectsRead is a read of the deployed stream's subjects the client keeps: the subjects it
// answered, and seq, its number among the client's reads, taken as it started
// (Client.subjectsReadSeq).
type subjectsRead struct {
	seq      uint64
	subjects []string
}

// keepSubjects keeps subjects, the answer of the read numbered started (subjectsReadSeq), for
// DeployedStreamCaptures, unless the client already keeps the answer of a read that started after
// it. The client's reads overlap - a judgement's read again (startRereadLocked), the periodic one
// (followDeployedSubjects) and an ensure's (ensureStream) - and end in any order, so a read that
// started before another and ended after it would otherwise put back the older list over the
// newer one: the subjects as they stood before a newer build's start added its own, judged
// uncaptured again until the next read. The compare-and-swap leaves every judgement one atomic load
// of the list it reads.
func (c *Client) keepSubjects(started uint64, subjects []string) {
	if c.beforeSubjectsKept != nil {
		c.beforeSubjectsKept()
	}
	read := &subjectsRead{seq: started, subjects: subjects}
	for {
		kept := c.deployedSubjects.Load()
		if kept != nil && kept.seq > started {
			return
		}
		if c.deployedSubjects.CompareAndSwap(kept, read) {
			return
		}
	}
}

// readDeployedSubjectsUntilRead is the reconnect hook of a client that connects in the background
// and owns no stream: on its first connection, and on each later one until a read has succeeded, it
// reads the deployed subjects within firstStreamSubjectsRead and the run's context, as Connect's
// constructor does at connect for a client that waits. A read that fails fails no hook: the client
// decides by what it holds, as at connect, and followDeployedSubjects reads again.
func (c *Client) readDeployedSubjectsUntilRead(ctx context.Context, conn *nats.Conn) error {
	if c.deployedSubjects.Load() == nil && !c.subjectsRefused.Load() {
		c.readDeployedSubjects(ctx, conn, firstStreamSubjectsRead)
	}
	return nil
}

// streamInfoSubject is the JetStream API subject a read of the stream's configuration is sent on.
const streamInfoSubject = "$JS.API.STREAM.INFO." + Stream

// followDeployedSubjects reads the deployed stream's subjects every interval, while the client is
// connected, until the client stops or the server refuses the read.
func (c *Client) followDeployedSubjects(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		conn := c.Conn
		c.mu.Unlock()
		if !conn.IsConnected() {
			continue
		}
		if c.subjectsRefused.Load() || !c.followRead(conn) {
			return
		}
	}
}

// followRead is one read of followDeployedSubjects, within streamSubjectsRead and until the client
// stops.
func (c *Client) followRead(conn *nats.Conn) bool {
	ctx, cancel := c.untilStopped()
	defer cancel()
	return c.readDeployedSubjects(ctx, conn, streamSubjectsRead)
}

// startKeeperOnce is the reconnect hook of an owning client that connects in the background: it
// ensures the stream on every connection event (ensureStreamOn), and the first ensure that
// succeeds starts the client's keeper at interval (startKeeper), where every other owning client's
// constructor starts it. It is the client's first hook, and the hooks run one at a time (rewatch),
// so keeping needs no lock of its own. No pass of the keeper runs before that first ensure: a pass
// that read the stream before the ensure's write and wrote after it would put back a subject the
// ensure replaced, and no later ensure would remove it. A client stopped before then never starts
// one.
func (c *Client) startKeeperOnce(interval time.Duration) func(context.Context, *nats.Conn) error {
	keeping := false
	return func(ctx context.Context, conn *nats.Conn) error {
		if err := c.ensureStreamOn(ctx, conn); err != nil {
			return err
		}
		if !keeping {
			keeping = true
			c.startKeeper(*c.streamConfig, interval)
		}
		return nil
	}
}

// awaitingFirstConnection reports whether the client connects in the background and nats.go has
// yet to connect it for the first time.
func (c *Client) awaitingFirstConnection() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awaitingConnection
}

func (c *Client) JS() nats.JetStreamContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.js
}

// Connected reports whether the client currently has a live NATS connection. It reads the flag
// the connection's events keep (connectedConn, noteConnection) rather than the connection
// itself, so it waits neither on the client's lock nor on nats.go's, which a reconnect dial that
// hangs holds for the connect timeout at a time.
func (c *Client) Connected() bool {
	return c.ConnectedConn() != nil
}

// ConnectedConn is the connection the client is installed on and connected over now, and nil
// while it has none: the flag Connected reads, as the connection it names. A caller that holds a
// connection of its own compares the two to tell whether it is still the client's, which no read
// of nats.Conn can answer: a connection the client replaced goes on reading connected while
// nats.go reconnects it. It is one atomic load, so it waits on no lock either
// (cmd/listener's durableReceiver.receiveUp).
func (c *Client) ConnectedConn() *nats.Conn {
	return c.connectedConn.Load()
}

// noteConnection is how a connection's own events move the client's connected flag: nc connected
// or reconnected (connected set), or nc disconnected or closed. It records the report for nc,
// installed or not, so an install that comes after it reads it (markConnected), and moves the
// flag only when nc is the connection the client has installed: nats.go reports the connect of a
// connection the client dialled before the client has made it c.Conn, and a flag set then would
// read connected while c.Conn still names the connection before it. A late report of a
// connection the client has since replaced moves nothing, and a report cannot land after a later
// one of its own connection: nats.go runs a connection's callbacks in order on one goroutine of
// that connection's, the client dials a replacement only for a connection that is closed, and
// the close is the last event of one.
func (c *Client) noteConnection(nc *nats.Conn, connected bool) {
	c.connFlagMu.Lock()
	defer c.connFlagMu.Unlock()
	if c.connUp == nil {
		c.connUp = map[*nats.Conn]bool{}
	}
	c.connUp[nc] = connected
	if c.installedConn != nc {
		return
	}
	if connected {
		c.connectedConn.Store(nc)
		return
	}
	c.connectedConn.Store(nil)
}

// markConnected records nc as the connection the client has installed and sets the connected
// flag from what nc's events have reported so far (noteConnection), or from dialedUp, how the dial
// returned nc, when they have reported nothing yet. It runs under c.mu, after c.Conn names nc, so a
// reader that sees the flag sees c.Conn too. An event of nc that the install did not see takes
// connFlagMu after it and moves the flag itself, so no order of the two leaves the flag at odds
// with nc's last event. It forgets every other connection's reports: none of them moves the flag
// again.
func (c *Client) markConnected(nc *nats.Conn, dialedUp bool) {
	c.connFlagMu.Lock()
	defer c.connFlagMu.Unlock()
	up, reported := c.connUp[nc]
	if !reported {
		up = dialedUp
	}
	c.connUp = map[*nats.Conn]bool{nc: up}
	c.installedConn = nc
	if up {
		c.connectedConn.Store(nc)
		return
	}
	c.connectedConn.Store(nil)
}

// afterInstallForTest runs the client's afterInstall test hook, when one is set, once an install
// has set the connected flag and let go of c.mu: the moment a connection event can land after the
// install. Nothing outside a test sets it.
func (c *Client) afterInstallForTest(nc *nats.Conn) {
	if c.afterInstall != nil {
		c.afterInstall(nc)
	}
}

// Connection is the client's connection now, read under its lock. A KV handle opened outside this
// package is opened on it (OpenKeyValue, roles.Buckets), and so is re-opened on the replacement
// after a connection event.
func (c *Client) Connection() *nats.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn
}

// ConnectionChanged returns a channel that is closed at the client's next connection event: a
// connection the client installs, the first or one it dialled to replace a closed one, and a
// disconnect, a reconnect in place or a close of a connection it dialled. A caller that waits for
// the connection to change takes the channel before it reads the connection's state, so an event
// between the read and the wait still wakes it.
func (c *Client) ConnectionChanged() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connChangedLocked()
}

// connChangedLocked returns the channel ConnectionChanged returns, making it when none is taken,
// for a caller that holds c.mu.
func (c *Client) connChangedLocked() chan struct{} {
	if c.connChanged == nil {
		c.connChanged = make(chan struct{})
	}
	return c.connChanged
}

// connectionChanged closes the channel ConnectionChanged returned, if one was taken, so that each
// caller waiting on it reads the connection again.
func (c *Client) connectionChanged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectionChangedLocked()
}

// connectionChangedLocked is connectionChanged for a caller that holds c.mu. The next channel is
// made when one is next taken, so an event nobody waits for allocates nothing.
func (c *Client) connectionChangedLocked() {
	if c.connChanged != nil {
		close(c.connChanged)
		c.connChanged = nil
	}
}

// HookedConnectionEvents is how many connection events this client's reconnect hooks have run to
// completion for: the count the last run that succeeded began at (rewatch). It is state about the
// client's own hooks, for a test that must wait for them and for nothing else. A hook a caller
// adds after the constructor returns can miss the first connection event, because a run of the
// hooks copies the list at its start (runReconnectHooks). ConnectOwningStream registers its
// reconcile hook inside the constructor, before a caller can add one, so this count covers that
// reconcile.
func (c *Client) HookedConnectionEvents() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hookedEvents
}

// RepairGrace is how long the reconnect hooks may be uncovered before a failed run of them is an
// operator's business. A failure under it is run again by the recovery within seconds
// (recoverUntilHealthy), so it is reported at WARN; at or past it the hooks have still not covered
// their connection event, the state behind them is stale, and the report is an ERROR.
// internal/kvwatch waits the same constant for a repair of a watcher that stopped before it
// reports it, so both halves of the same outage report at the same minute.
const RepairGrace = time.Minute

// HookStall reports how long the reconnect hooks have been due without a run that succeeded, zero
// while none is due, and the error of the last run that failed. That error is nil when no run has
// failed since the last run that succeeded (rewatch clears it on every success, even one a
// connection event overtook), so a caller can read a stall with no error behind it, which
// cmd/listener's /healthz reports as the stall alone. That handler is a reader, as
// reportHookFailure is, so this takes the client's state under mu and touches no connection, as
// Connected and SubOK do.
func (c *Client) HookStall() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hooksDueSince.IsZero() {
		return 0, nil
	}
	// A clock a test moves itself can stand before the event it timed, and a wall clock can step
	// back, so the stall never reads as a negative duration.
	return max(c.now().Sub(c.hooksDueSince), 0), c.lastHookErr
}

// now reads the clock the client takes for its reconnect reporting (WithReconnectErrClock),
// time.Now when it took none.
func (c *Client) now() time.Time {
	if c.reconnectErrClock == nil {
		return time.Now()
	}
	return c.reconnectErrClock()
}

// markHooksDueLocked counts a connection event the reconnect hooks have yet to cover, under mu,
// and starts the stall at it when the hooks were current. An event that lands while they are
// already due leaves the stall where it is: it is the first uncovered event that says how long the
// hooks' state has been stale.
func (c *Client) markHooksDueLocked() {
	c.connEvents++
	if c.hooksDueSince.IsZero() {
		c.hooksDueSince = c.now()
	}
}

// Subscribe creates the client's recoverable JetStream subscription. The client keeps one
// JetStream subscription: a later call on the same subject replaces it, unsubscribing the handle
// an earlier one returned, a call on another subject is refused with ErrSubscriptionHeld, and a
// caller that is done with it ends it with DrainSubscription. On a client that connects in the
// background and has not connected yet, the bind fails (nothing reaches the server before then)
// and the registration stays, so the first connection binds it; a caller that gives up on a
// subscription whose bind failed gives up the registration with ReleaseFailed.
func (c *Client) Subscribe(subject string, handler nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(recoverableSubscription{
		transport: jetStreamSubscription,
		subject:   subject,
		handler:   handler,
		opts:      opts,
	}, conn, js)
}

// SubscribeCore creates a recoverable core NATS subscription. The optional queue
// identifies the stable queue group that shares role-lane delivery between
// overlapping listeners. The client keeps one core subscription: a later call on the same
// subject and queue replaces it, unsubscribing the handle an earlier one returned, and any other
// call is refused with ErrSubscriptionHeld. A client that connects in the background and has not
// connected yet binds it on the connection it has, which nats.go sends once it connects: waiting
// for that connection here would wait out the outage.
func (c *Client) SubscribeCore(subject string, handler nats.MsgHandler, queues ...string) (*nats.Subscription, error) {
	queue := ""
	if len(queues) > 0 {
		queue = queues[0]
	}
	return c.subscribeCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: handler})
}

// subscribeCore registers next as the client's core subscription (SubscribeCore). A client that
// connects in the background and has not connected yet binds it on the connection it has, which
// nats.go sends once it connects.
func (c *Client) subscribeCore(next recoverableSubscription) (*nats.Subscription, error) {
	if !c.awaitingFirstConnection() {
		if err := c.ensureConn(); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(next, conn, js)
}

// registerSubscription makes next the transport's subscription and binds it. The registration
// stays when the bind fails, so a later restore binds it. A bind of the transport's subscription
// already in flight, a restore's or another registration's, is waited for first. Once Drain or
// Close has stopped the client it registers nothing, as a restore binds nothing: replacing the
// transport's subscription would unsubscribe one the drain is letting finish. A registration on
// another subject or queue group than the transport's subscription is refused (ErrSubscriptionHeld).
func (c *Client) registerSubscription(next recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) (*nats.Subscription, error) {
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[next.transport]
	c.awaitBindLocked(subscription)
	if c.stopped() {
		c.subscriptionsMu.Unlock()
		return nil, errStopped
	}
	// A registration on another subject, or the same subject in another queue group, would replace
	// a subscription its caller still relies on, and stop it delivering with nothing logged: two of
	// Dispatch's delivery intakes on one client lost a durable that way (LEGION-690). It is refused,
	// and the held subscription goes on delivering.
	if subscription.handler != nil && (subscription.subject != next.subject || subscription.queue != next.queue) {
		held := describeSubscription(subscription.subject, subscription.queue)
		c.subscriptionsMu.Unlock()
		return nil, fmt.Errorf("%w: holds %s, asked for %s", ErrSubscriptionHeld, held, describeSubscription(next.subject, next.queue))
	}
	// Registering the held subject again replaces the transport's subscription, so the one it
	// replaces stops delivering: the listener's self-health rebuild re-registers after its durable
	// consumer was lost, and the old handle, bound to that consumer's deliver inbox, would otherwise
	// stay subscribed for good. For a consumer the library created, Unsubscribe also deletes it, as
	// it always does. It runs before the bind, which a durable still bound to the old handle would
	// refuse.
	replaced := subscription.active
	// next is unrefused: the bind below sends its subscription anew, and a refusal of it arrives
	// only after the send.
	*subscription = next
	subscription.sentOn, subscription.sentReconnects = conn, reconnectsOf(conn)
	subscription.binding = make(chan struct{})
	c.subscriptionsMu.Unlock()
	if replaced != nil {
		_ = replaced.Unsubscribe()
	}
	sub, err := bind(next, conn, js)
	if err := c.endBind(subscription, sub, err, true); err != nil {
		return nil, err
	}
	return sub, nil
}

// DrainSubscription ends for good the JetStream registration the Subscribe that returned sub made:
// it gives the registration up, so that no reconnect or recovery binds it again and a later
// Subscribe, on any subject, registers anew, and ends the subscription the client held for it.
// While the client's connection is live it drains that subscription and returns it, for its caller
// to wait until it is no longer valid, which is how nats.go reports the drain's end; the handler is
// handed what the client had buffered meanwhile, and nothing new. That subscription is sub until
// the client replaces its connection, and after that the handle the client bound on the new
// connection: sub went with the old one, and draining it would fail while the rebound handle went
// on delivering. With no valid handle to end (the connection was replaced and the registration not
// bound again yet) it returns nil, having given the registration up all the same. A sub the client
// no longer holds -- one a later Subscribe replaced -- gives up and ends nothing, and returns nil.
// A bind of the registration in flight, a restore's, is waited for first, as registerSubscription
// does, so what it bound is the handle ended.
//
// While the client has no live connection it unsubscribes the handle instead, and so returns nil:
// an unsubscribe leaves the handle invalid at once, and there is nothing left to wait on. The test
// is Connected(), which is false for every status but CONNECTED, so a client whose connection is
// reconnecting, draining or closed takes this branch, not only one whose link has just dropped.
// The two cases differ because of what a drain begun while the link is down leaves behind. In
// nats.go v1.55.0, Conn.unsubscribe in drain mode keeps the subscription in the connection's list,
// starts checkDrained on a goroutine, and writes the UNSUB only when the connection is not
// reconnecting. checkDrained opens with a flush, which does not return until the connection is
// back; meanwhile doReconnect's resendSubscriptions sends a SUB for every subscription the
// connection still holds, the draining one among them. checkDrained then drops it from the client
// without ever writing an UNSUB, so the server goes on reporting the durable push-bound, to a
// client that delivers nothing, for the life of that connection, and no other process can take the
// durable over. An unsubscribe drops the subscription from the connection's list at once, so the
// reconnect's resend does not find it, and the UNSUB it would have written is not needed: the
// server released that interest with the connection that dropped. Dispatch's delivery intake gives
// its subscription up exactly when its link is down (INFRA-1551), which is why this is the rule
// rather than a caller's choice.
//
// A drain begun on a live link leaves the same thing behind when the link drops before that drain
// has ended, which is a window of its own: checkDrained holds the draining subscription in the
// connection's list for its flush's round trip and then passes of 100 ms, until the handler has
// taken what was buffered. The drop itself releases that flush (processOpError clears the pending
// flush calls), so what holds the window open past it is a handler still inside a delivery, which
// is what a drain is for. So the drain is watched (watchDrain): at the first connection event after
// which the connection no longer reads the session the drain began in, while the handle is still
// valid, the watch unsubscribes it, and the resend finds nothing to send. The watch's first event
// is taken (ConnectionChanged) before the connection's state is read below, which is that method's
// contract, so a drop between the read and the drain wakes the watch rather than being missed by
// it. The returned handle keeps its meaning: a caller waiting for the drain's end waits for the
// handle to turn invalid, which is what an unsubscribe by the watch leaves it. What the handler had
// not yet been handed is dropped with the drain, which is the price of leaving the durable free:
// those deliveries were buffered against a link that is gone, and the server redelivers them to
// whoever binds the durable next.
func (c *Client) DrainSubscription(sub *nats.Subscription) (*nats.Subscription, error) {
	if sub == nil {
		return nil, nil
	}
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[jetStreamSubscription]
	c.awaitBindLocked(subscription)
	if subscription.subscribed != sub {
		c.subscriptionsMu.Unlock()
		return nil, nil
	}
	active := subscription.active
	*subscription = recoverableSubscription{transport: jetStreamSubscription}
	c.subscriptionsMu.Unlock()
	if active == nil || !active.IsValid() {
		return nil, nil
	}
	// Taken before the state it guards is read, so that an event between the two still wakes the
	// watch below.
	changed := c.ConnectionChanged()
	conn := c.Connection()
	if conn == nil || conn.Status() != nats.CONNECTED {
		if err := active.Unsubscribe(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	reconnects := reconnectsOf(conn)
	if err := active.Drain(); err != nil {
		return nil, err
	}
	go c.watchDrain(active, conn, reconnects, changed)
	return active, nil
}

// drainWatchStep is how long watchDrain goes without reading the drained handle again. nats.go
// reports a drain's end only by the handle turning invalid, so the watch has nothing to wait on
// and polls; the step costs it one timer wakeup per 10 ms of the drain, which checkDrained itself
// holds to its flush's round trip plus passes of 100 ms.
const drainWatchStep = 10 * time.Millisecond

// watchDrain turns the drain DrainSubscription (or releaseAttempt) began on a live link into an
// unsubscribe once that link has dropped or reconnected, while the handle is still valid: a drain
// still in the connection's subscription list when a reconnect lands is re-sent by
// resendSubscriptions and then dropped by checkDrained with no UNSUB ever written, which is what
// DrainSubscription's doc describes. An unsubscribe takes the subscription out of that list, so the
// resend finds nothing. changed is the channel taken before the caller read conn's state, so a
// drop between that read and the drain wakes the watch too. With one NATS URL there is room for
// it: doReconnect queues the disconnect callback this channel reports and then waits a full
// ReconnectWait (1 s) before the resend.
//
// The session the drain began in is conn read CONNECTED with reconnects as its count, both read
// after changed was taken and before the drain. A wake does not by itself say that session ended:
// nats.go queues a reconnect's callback, which reports the event (queueReconnect), under the
// connection's lock and runs it later on its callback goroutine, so a flush that waited for that
// lock can return, and a drain begin, on the reconnected session before the callback has run. The
// event it then reports is that reconnect, one the drain began after, and an unsubscribe there
// would drop what the session delivered to the drain. So at each wake the watch takes the next
// channel first and then reads conn's status and then its count, in sendCore's order: it
// unsubscribes when conn no longer reads CONNECTED or its count has moved, and goes on watching
// otherwise. nats.go counts a reconnect and re-sends the subscriptions under one hold of the
// connection's lock, so a resend the drain is in shows as a moved count; a connection the client
// replaced is closed, so it no longer reads CONNECTED.
//
// The watch ends with the drain, so it is never a goroutine parked for the life of the process.
// The drain always ends: checkDrained opens with nc.Flush(), which carries the client's flush
// timeout (10 s in nats.go v1.55.0) and so returns whether or not the link came back, and its loop
// then ends at the first pass where the handler has taken what was buffered, or the subscription
// or the connection is closed -- each of which leaves the handle invalid, which the watch reads at
// its next step. An unsubscribe here ends it too: checkDrained's next pass finds the subscription
// closed. For a consumer the library created, that unsubscribe deletes it, as the drain's own end
// would have, and checkDrained's pass then asks for a delete the server answers not found, which
// reaches the async error callback; Dispatch's intakes bind durables they add themselves, which
// nats.go deletes for neither.
func (c *Client) watchDrain(active *nats.Subscription, conn *nats.Conn, reconnects uint64, changed <-chan struct{}) {
	step := time.NewTicker(drainWatchStep)
	defer step.Stop()
	for {
		select {
		case <-changed:
			changed = c.ConnectionChanged()
			if conn.Status() == nats.CONNECTED && reconnectsOf(conn) == reconnects {
				continue
			}
			if active.IsValid() {
				// Its error would be the handle's own end -- the drain ended between the read and
				// here, or the connection closed under it -- which is all this watch waits for.
				_ = active.Unsubscribe()
			}
			return
		case <-step.C:
			if !active.IsValid() {
				return
			}
		}
	}
}

// ReleaseFailed gives up the JetStream registration a Subscribe on subject left behind when its own
// bind failed, so that no reconnect or recovery binds it. A caller that gives up on a subscription
// it could not bind calls it: the client binds the registration on its next connection otherwise,
// and hands the handler deliveries its caller no longer takes. A bind of the registration in flight
// is waited for first, as DrainSubscription does, and a handle a restore bound for it meanwhile is
// unsubscribed. It gives up nothing else: not a registration whose Subscribe's own bind succeeded
// (a later Subscribe's), nor one on another subject, which refused the call (ErrSubscriptionHeld).
func (c *Client) ReleaseFailed(subject string) {
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[jetStreamSubscription]
	c.awaitBindLocked(subscription)
	if subscription.handler == nil || subscription.subject != subject || subscription.subscribed != nil {
		c.subscriptionsMu.Unlock()
		return
	}
	rebound := subscription.active
	*subscription = recoverableSubscription{transport: jetStreamSubscription}
	c.subscriptionsMu.Unlock()
	if rebound != nil {
		_ = rebound.Unsubscribe()
	}
}

// awaitBindLocked waits until no bind of the registration s holds is in flight. It is called with
// subscriptionsMu held and returns with it held; the lock is let go while it waits, so a bind that
// ends while it waits may have changed s, and another may have started, which it waits for too.
func (c *Client) awaitBindLocked(s *recoverableSubscription) {
	for s.binding != nil {
		inFlight := s.binding
		c.subscriptionsMu.Unlock()
		<-inFlight
		c.subscriptionsMu.Lock()
	}
}

// describeSubscription names a subscription in ErrSubscriptionHeld's error: its subject, and its
// queue group when it has one.
func describeSubscription(subject, queue string) string {
	if queue == "" {
		return fmt.Sprintf("%q", subject)
	}
	return fmt.Sprintf("%q in queue group %q", subject, queue)
}

// bind subscribes registration's handler, through js for a JetStream subscription and on conn for
// a core one. It is the request to the server, so no lock of the client's is held across it.
func bind(registration recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) (*nats.Subscription, error) {
	if registration.transport == jetStreamSubscription {
		return js.Subscribe(registration.subject, registration.handler, registration.opts...)
	}
	var sub *nats.Subscription
	var err error
	if registration.queue == "" {
		sub, err = conn.Subscribe(registration.subject, registration.handler)
	} else {
		sub, err = conn.QueueSubscribe(registration.subject, registration.queue, registration.handler)
	}
	if err != nil || registration.pendingMsgs == 0 {
		return sub, err
	}
	if err := sub.SetPendingLimits(registration.pendingMsgs, registration.pendingBytes); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub, nil
}

// endBind ends the bind in flight of the registration subscription holds. A bind that ends during
// Drain's collection installs for the next pass; after that collection it drains itself. A Close
// never installs a bind that finishes after it stopped the client. returned is set for the bind of
// a Subscribe, whose handle it returns: only that one is installed as the registration's subscribed.
func (c *Client) endBind(subscription *recoverableSubscription, sub *nats.Subscription, err error, returned bool) error {
	c.subscriptionsMu.Lock()
	close(subscription.binding)
	subscription.binding = nil
	mode := c.stopMode
	if err == nil && (mode == clientRunning || mode == clientDraining) {
		subscription.active = sub
		if returned {
			subscription.subscribed = sub
		}
	}
	c.subscriptionsMu.Unlock()

	if err != nil {
		return err
	}
	switch mode {
	case clientDrainCollected:
		_ = sub.Drain()
		return errStopped
	case clientClosed:
		_ = sub.Unsubscribe()
		return errStopped
	}
	return nil
}

// SubOK reports whether the durable listener subscription is active on a live connection.
func (c *Client) SubOK() bool {
	return c.subscriptionOK(jetStreamSubscription)
}

// CoreSubOK reports whether the client's core subscription is active on a live connection and the
// server has not refused it. It reads the subscription the client holds now, not the handle
// SubscribeCore returned: a recovery onto a connection dialed to replace a closed one binds a
// replacement, and the returned handle stays invalid for good. A subscription the server refused
// stays valid in nats.go, which only records the refusal, and one it re-sends on a reconnect in
// place is refused again by a server that took the grant away during the outage, so the refusal
// is read from the server's own report of it (onAsyncError) until the subscription is sent again.
func (c *Client) CoreSubOK() bool {
	return c.subscriptionOK(coreSubscription)
}

// ResendRefusedCore sends the client's core subscription to the server again when the server has
// refused it on the connection the client holds and that connection is connected. Nothing else
// does. A reload of the server's configuration that takes the grant away revokes the subscription
// and keeps the connection, and a later reload that gives the grant back does not restore it: the
// server keeps no record of a subscription it refused, and nats.go sends a subscription again only
// when it reconnects. Without a new send the subscription stays refused for the life of the
// connection, whatever grant the server holds now.
//
// It sends the subscription in each queue group of coreQueues in turn, or in the registration's
// own when the client has none, and waits for the server's answer to each one (sendCore). It
// stops at the first group the server accepts. The new subscription replaces the refused one,
// which it then unsubscribes; the server holds no subscription under that sid, so it ignores the
// UNSUB. Each subscription the server refused is unsubscribed at once. When no group accepts it,
// the refused subscription the client held stays the one it holds, in its own queue group, so a
// reconnect sends that one again, as nats.go sends every subscription the connection holds: a
// refused attempt in another group left in its place would move the lane to that group for good.
// The mark stays set until the server has accepted a send, so CoreSubOK never reads true for a
// subscription the server refuses. A send whose answer does not come back leaves the refused
// subscription in place. So does every attempt that leaves the session of the connection this
// method read at its start, whichever group it was in (sendCore, errSendSuperseded): a verdict
// counts only from an attempt that began and ended inside that session. The reconnect sent the
// refused subscription again, and the new server's answer to it decides.
//
// It sends nothing in four cases. While a bind of the registration is in flight, that bind sends
// the subscription again itself. While the refused subscription still holds messages that the
// server delivered before the revocation, the unsubscribe would drop them, and its handler takes
// them first. On a connection that is not connected, the reconnect sends every subscription again.
// And when the refusal is of an earlier session of the connection (sentOn, sentReconnects):
// nats.go has reconnected and sent the subscription again, and countConnectionEvent has not run
// yet.
func (c *Client) ResendRefusedCore() error {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	// The status is read before the reconnect count: nats.go counts a reconnect before it reads
	// CONNECTED again, so a reconnect between the two reads changes the count and refuses the
	// send, where the other order could pair the new session's status with the old one's count.
	connected := conn != nil && conn.Status() == nats.CONNECTED
	reconnects := reconnectsOf(conn)
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[coreSubscription]
	refused := subscription.active
	send := !c.stopped() && connected && subscription.refused &&
		subscription.binding == nil && refused != nil && refused.IsValid() &&
		subscription.sentOn == conn && subscription.sentReconnects == reconnects
	if send {
		pending, _, err := refused.Pending()
		send = err == nil && pending == 0
	}
	if !send {
		c.subscriptionsMu.Unlock()
		c.mu.Unlock()
		return nil
	}
	subscription.binding = make(chan struct{})
	registration := *subscription
	c.subscriptionsMu.Unlock()
	c.mu.Unlock()

	queues := c.coreQueues
	if len(queues) == 0 {
		queues = []string{registration.queue}
	}
	var (
		sub      *nats.Subscription
		queue    string
		accepted bool
		err      error
	)
	for _, queue = range queues {
		if sub != nil {
			// The server refused the subscription in the previous group.
			_ = sub.Unsubscribe()
		}
		attempt := registration
		attempt.queue = queue
		// Every attempt is measured against the session read above, not one of its own: a
		// reconnect between two groups' attempts would otherwise let the machine's group be tried,
		// and kept, on a new server that may accept the shared group the client held.
		if sub, accepted, err = c.sendCore(attempt, conn, js, reconnects); err != nil || accepted {
			break
		}
	}
	if err == nil && !accepted {
		// No group accepted it: the last refused attempt is dropped like the others, and the
		// refused subscription the client held stays in place, still marked.
		_ = sub.Unsubscribe()
		sub, err = nil, errNoQueueAccepted
	}
	if err == nil {
		// The registration takes the accepted subscription's queue group before endBind installs
		// it, so a refusal of it after a reconnect in between names the queue group the
		// registration holds and is marked (onAsyncError). Should endBind then install nothing, the
		// client is stopped, and no refusal is read again.
		c.subscriptionsMu.Lock()
		subscription.queue = queue
		c.subscriptionsMu.Unlock()
	}
	if err := c.endBind(subscription, sub, err, false); err != nil {
		// No group accepted it, or a reconnect overtook the send (errSendSuperseded): the refused
		// subscription the client held stays in place, and so does its mark. After a reconnect,
		// nats.go has sent that subscription again and countConnectionEvent has cleared the mark,
		// so the new server's answer to it decides, as for any reconnect.
		if errors.Is(err, errNoQueueAccepted) || errors.Is(err, errSendSuperseded) {
			return nil
		}
		// A send that failed leaves the refused subscription as the one the client holds, still
		// marked. A bind that ended after Drain or Close stopped the client leaves both to them.
		return err
	}
	c.mu.Lock()
	c.subscriptionsMu.Lock()
	if subscription.active == sub {
		// A reconnect during the send has cleared the mark (countConnectionEvent), and the new
		// server's answer to the subscriptions nats.go sent it again is the one that counts.
		if c.Conn == conn && reconnectsOf(conn) == reconnects {
			subscription.refused = false
		}
	}
	c.subscriptionsMu.Unlock()
	c.mu.Unlock()
	if queue != registration.queue {
		slog.Warn("envoy nats core subscription sent again in another queue group, since the server refused it in its own",
			slog.String("name", c.name),
			slog.String("subject", registration.subject),
			slog.String("queue", queue),
			slog.String("refused_queue", registration.queue))
	}
	// The new subscription is the one the client holds now. The unsubscribe can only fail on a
	// connection that closed meanwhile, which the recovery replaces with every subscription bound.
	_ = refused.Unsubscribe()
	return nil
}

// coreSendAnswerWait bounds sendCore's wait for the server's answer to a subscription it sends.
const coreSendAnswerWait = 2 * time.Second

// errNoQueueAccepted is how ResendRefusedCore ends a bind no queue group accepted: it installs
// nothing, so the refused subscription the client held stays in place. It never leaves that method.
var errNoQueueAccepted = errors.New("bus: the server accepted the core subscription in no queue group")

// errSendSuperseded is sendCore's error for a send that left the session of the connection it
// was measured against: the connection did not read CONNECTED and that session's reconnect count
// as the send began, or no longer did once the server's answer was read. It never leaves
// ResendRefusedCore.
var errSendSuperseded = errors.New("bus: the connection reconnected during the core subscription's send")

// sendCore binds registration on conn and waits, within coreSendAnswerWait, until the server has
// read the subscription, and reports whether it accepted it. The server answers a flush only after
// every line the client sent before it, and nats.go records a refusal as the connection's last
// error as it reads the -ERR (processTransientError), before the PONG that ends the flush. So the
// server accepted the subscription when that error is still the one the connection held before
// the bind (sameError); any other error, a refusal among them, counts as not accepted. A flush
// that fails leaves the answer unknown: the subscription is released (releaseAttempt) and the
// failure returned.
//
// That comparison holds only within one session of the connection, since nats.go sets the last
// error anew within a reconnect: to nil as the reconnect begins (doReconnect), to nil after each
// failed attempt, and to the handshake's result once an attempt connects. Compared across that, a
// refusal before and nil after counts as refused a subscription the new server accepted, and nil
// before and nil after counts as accepted an answer no server gave in the session the client
// keeps. A flush begun while the connection reconnects waits in the reconnect buffer and succeeds
// on the new server, and a drop after the flush returned clears the error with the count still
// unchanged, so neither the flush nor the count alone catches a reconnect. So the whole send must
// stay inside the session ResendRefusedCore read at its start, reconnects: the connection reads
// CONNECTED and that count both before the bind and after the answer is read. The status is read
// before the count each time. nats.go turns a connection RECONNECTING before it clears the error,
// and counts a reconnect once a dial succeeds, before the connection reads CONNECTED again; so a
// connection that dropped reads RECONNECTING until a dial succeeds, and one that reconnected reads
// a higher count. A send outside the session ends with errSendSuperseded and its subscription
// released, and leaves the subscription the client held, which a reconnect sends again, to the
// new server's answer.
func (c *Client) sendCore(registration recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext, reconnects uint64) (*nats.Subscription, bool, error) {
	if conn.Status() != nats.CONNECTED || reconnectsOf(conn) != reconnects {
		return nil, false, errSendSuperseded
	}
	before := conn.LastError()
	sub, err := bind(registration, conn, js)
	if err != nil {
		return nil, false, err
	}
	if hook := c.beforeCoreSendFlush; hook != nil {
		hook()
	}
	if err := conn.FlushTimeout(coreSendAnswerWait); err != nil {
		c.releaseAttempt(conn, sub)
		return nil, false, err
	}
	if hook := c.afterCoreSendFlush; hook != nil {
		hook()
	}
	after := conn.LastError()
	if conn.Status() != nats.CONNECTED || reconnectsOf(conn) != reconnects {
		c.releaseAttempt(conn, sub)
		return nil, false, errSendSuperseded
	}
	return sub, sameError(after, before), nil
}

// releaseAttempt ends a subscription sendCore made and gives no verdict on, which a server may have
// accepted and delivered to: a reconnect sends it again with every other subscription, and the new
// server may accept it. On a connection that reads CONNECTED it drains it: nats.go sends the UNSUB
// at once and hands the messages the subscription holds to its handler before it removes it, where
// Unsubscribe would drop them. A drain keeps the subscription in nats.go's set until the handler
// has taken what it holds, and a reconnect in that time sends it again, after which checkDrained
// removes it with no UNSUB ever written (resendSubscriptions, checkDrained). That would leave the
// server a member of the queue group that the client no longer reads, taking a share of the
// group's messages. So the drain is watched, as DrainSubscription's is (watchDrain): once the
// connection no longer reads the session the drain began in, while the subscription is still
// valid, the watch unsubscribes it, and the reconnect's resend finds nothing to send. The watch's
// first event is taken (ConnectionChanged) before the status is read, so a drop between the read
// and the drain wakes the watch too; the count is read after the status, and both before the
// drain, as sendCore reads them. On a connection that does not read CONNECTED it unsubscribes it
// at once, for the same reason. Either way, the messages it still holds when it is unsubscribed
// are lost with the session that delivered them.
func (c *Client) releaseAttempt(conn *nats.Conn, sub *nats.Subscription) {
	changed := c.ConnectionChanged()
	if conn.Status() != nats.CONNECTED {
		_ = sub.Unsubscribe()
		return
	}
	reconnects := reconnectsOf(conn)
	if err := sub.Drain(); err != nil {
		_ = sub.Unsubscribe()
		return
	}
	go c.watchDrain(sub, conn, reconnects, changed)
}

// reconnectsOf is conn's reconnect count, 0 for no connection: the session of a subscription's
// send (recoverableSubscription.sentReconnects).
func reconnectsOf(conn *nats.Conn) uint64 {
	if conn == nil {
		return 0
	}
	return conn.Stats().Reconnects
}

func (c *Client) subscriptionOK(transport subscriptionTransport) bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[transport]
	subOK := subscription.active != nil && subscription.active.IsValid() && !subscription.refused
	c.subscriptionsMu.Unlock()
	return connOK && subOK
}

// subscriptionRefusal reads the server's refusal of a subscription: the subject, the queue group
// when there is one, and the sid when the refusal is a configuration reload's revocation of a
// subscription it had accepted. A refusal of a subscription the client sent reads `Permissions
// Violation for Subscription to "<subject>" using queue "<queue>"` (nats-server v2.15.1
// client.go:5915-5919); a revocation reads `Permissions Violation for Subscription to "<subject>"
// (sid "<sid>")`, with `using queue "<queue>"` before the sid when the subscription has one
// (client.go:6268-6271; nats-server before v2.15.0 named no queue group in a revocation at all).
// The server writes each name with %q, so each is matched as a whole Go-quoted string, escapes
// included, and unquoted before it is compared: a name holding a double quote or a backslash
// reaches the text escaped. nats.go's own reading (permissionsRe and permissionsQueueRe, nats.go
// v1.55.0 nats.go:4162-4182) keeps the escapes, so it compares unequal to such a name.
var subscriptionRefusal = regexp.MustCompile(`Subscription to ("(?:[^"\\]|\\.)*")(?: using queue ("(?:[^"\\]|\\.)*"))?(?: \(sid ("(?:[^"\\]|\\.)*")\))?`)

// onAsyncError is wired as the connection's async error callback, after the error is logged. A
// slow-consumer error is counted (SlowConsumers). A server's refusal of a publish is recorded under
// the subject it refused, for that publish to read (recordPublishDenial); its refusal of the
// client's core subscription marks it refused for CoreSubOK. Any other error, a refusal of another
// subscription among them, changes nothing, and so does one a connection the client has since
// replaced reports late.
//
// The publish case is here because the connection is where the server reports it: a denied publish
// is acknowledged by nothing, and nats.go keeps one last error per connection, so a publish window
// that sent several denied subjects at once leaves only the last of their violations in that slot.
// The record keeps each under its own subject instead, so every denied send of such a window is
// answered its own denial (window.acknowledged).
//
// A refusal of a subscription the client sent names its subject and, when it has one, its queue
// group, and both must be the core registration's. A reload's revocation names the subject, the
// sid, and from v2.15.0 the queue group, so a revocation that names none is identified by its
// subject alone: the client holds one core subscription, and nats.go itself leaves that refusal
// unrecorded on a queue subscription and the subscription valid, though the server has removed it.
//
// The mark cannot outlive the refusal: nats.go queues the reconnect callback, which clears it
// (countConnectionEvent, on the callback goroutine, before the event's own work is queued),
// before any refusal of the subscriptions it re-sent, since it holds the connection's lock from
// the re-send until that callback is queued and reads the server's -ERR only under that lock,
// and both callbacks run in order on one goroutine. c.mu is held across the
// mark so a replacement connection cannot be installed between the check and the mark.
func (c *Client) onAsyncError(nc *nats.Conn, err error) {
	if errors.Is(err, nats.ErrSlowConsumer) {
		c.slowConsumers.Add(1)
		return
	}
	if errors.Is(err, nats.ErrPermissionViolation) && c.recordPublishDenial(nc, err) {
		return
	}
	refusal, ok := readSubscriptionRefusal(err)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if nc != c.Conn {
		return
	}
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	core := &c.subscriptions[coreSubscription]
	if core.handler != nil && core.subject == refusal.subject && (refusal.revoked || core.queue == refusal.queue) {
		core.refused = true
	}
}

// SlowConsumers is how many slow-consumer errors the client's connections have reported: one each
// time a subscription fell past its pending limits and nats.go began dropping its messages. The
// role lane's limits are set far above any backlog its handler can build (ConnectRoleLane), so it
// reports one only under a flood no measured load comes near.
func (c *Client) SlowConsumers() uint64 {
	return c.slowConsumers.Load()
}

// subscriptionRefusalText is a server's refusal of a subscription, read by readSubscriptionRefusal.
type subscriptionRefusalText struct {
	subject, queue string
	// revoked is set for a configuration reload's revocation, which names no queue group.
	revoked bool
}

// readSubscriptionRefusal reads err as the server's refusal of a subscription
// (subscriptionRefusal), reporting false for any other error.
func readSubscriptionRefusal(err error) (subscriptionRefusalText, bool) {
	if !errors.Is(err, nats.ErrPermissionViolation) {
		return subscriptionRefusalText{}, false
	}
	match := subscriptionRefusal.FindStringSubmatch(err.Error())
	if match == nil {
		return subscriptionRefusalText{}, false
	}
	subject, unquoteErr := strconv.Unquote(match[1])
	if unquoteErr != nil {
		return subscriptionRefusalText{}, false
	}
	queue := ""
	if match[2] != "" {
		if queue, unquoteErr = strconv.Unquote(match[2]); unquoteErr != nil {
			return subscriptionRefusalText{}, false
		}
	}
	return subscriptionRefusalText{subject: subject, queue: queue, revoked: match[2] == "" && match[3] != ""}, true
}

// ErrSubscriptionHeld is what Subscribe and SubscribeCore return for a registration on another
// subject, or the same subject in another queue group, than the subscription the client already
// holds on that transport. The held subscription goes on delivering; a caller that needs another
// durable takes another Client.
var ErrSubscriptionHeld = errors.New("bus: client already holds a subscription of this transport")

// errStopped is what a client refuses once Drain or Close stopped it: dialling or installing a
// connection, or re-subscribing.
var errStopped = errors.New("bus: client is stopped")

// stop stops the client: recovery ends, and nothing dials, installs a connection or re-subscribes.
func (c *Client) stop() {
	c.closeOnce.Do(func() { close(c.stopCh) })
}

func (c *Client) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// Close stops any recovery goroutine and the keeper, and closes the underlying NATS connection,
// and takes the client out of the registry of live clients (forgetClient), since its connection
// is going. It returns once the keeper has ended (awaitKeeper).
func (c *Client) Close() {
	forgetClient(c)
	c.subscriptionsMu.Lock()
	if c.stopMode == clientRunning {
		c.stopMode = clientClosed
	}
	c.subscriptionsMu.Unlock()
	c.stop()
	c.mu.Lock()
	if c.Conn != nil {
		// The close reports itself (noteConnection), but only once nats.go has run its callback:
		// a read right after this call answers that the client has no connection either way.
		c.noteConnection(c.Conn, false)
		c.Conn.Close()
	}
	c.mu.Unlock()
	c.awaitKeeper()
}

// Drain stops the client and drains it, letting the deliveries already in their handlers finish,
// and returns once the connection has closed, at the latest once timeout passes, so a blocked
// drain cannot keep a process alive. It stops the client first, so neither the drain's close nor a
// recovery already under way reconnects or re-subscribes a process that is shutting down. It then
// drains the delivery subscriptions while the connection still accepts new ones: a handler
// finishing its delivery may subscribe (a forward's reply inbox, StartRequestStoredTo), which a
// draining connection refuses. Next it runs the drain hooks (AddDrainHook), which end the KV
// watchers with requests of their own while the connection still carries them, even when the
// deliveries ran out their share of the timeout (drainHookReserve) or failed to drain. Only then
// does it drain the connection, whose Drain only starts the drain, and waits for it to close
// itself. A connection that is reconnecting is closed at once, once the hooks have run on a
// context that has already ended: nothing it sends reaches the server, so no subscription would
// drain and a reconnect would re-send them, but each hook still ends what it holds and says what
// it left to the server. A bind in flight at the stop is not waited for: one that ends while the
// subscriptions drain installs its own, which the next pass drains, and one that ends after the
// last pass drains its own, which the connection's drain takes (endBind). The close ends one
// still in flight. A reconnect nats.go reports once the client has stopped closes the connection
// only when it came after the stop (closeStopped). The stop ends the keeper too, which Drain
// waits for once the connection is closed (awaitKeeper). The stop also takes the client out of
// the registry of live clients (forgetClient), as Close does, since the connection it owns is
// going.
func (c *Client) Drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	deliveriesDeadline := deadline.Add(-min(drainHookReserve, timeout*3/10))
	c.subscriptionsMu.Lock()
	if c.stopMode == clientRunning {
		c.stopMode = clientDraining
	}
	c.subscriptionsMu.Unlock()
	forgetClient(c)
	// The stop and the reconnect count are one step under mu, so a reconnect callback that finds
	// the client stopped also finds the count (closeStopped).
	c.mu.Lock()
	c.stop()
	conn := c.Conn
	// A draining connection is no connection to serve with, as it read before this flag
	// (nats.Conn.Status reads DRAINING_SUBS, never CONNECTED).
	c.noteConnection(conn, false)
	c.drainedReconnects = conn.Stats().Reconnects
	c.mu.Unlock()
	defer c.awaitKeeper()
	defer conn.Close()
	if conn.IsReconnecting() {
		// Nothing sent while reconnecting reaches the server, so no subscription would drain and
		// a reconnect would re-send them: the connection is closed at once. The drain hooks still
		// run first, on a context that has already ended (runDrainHooks reads the connection as
		// lost), so each ends what it holds and says what it left to the server; a drain that ran
		// none ended no KV watcher and said nothing of any.
		c.runDrainHooks(conn, deadline)
		return fmt.Errorf("drain NATS: %w", nats.ErrConnectionReconnecting)
	}
	if err := c.drainDeliveries(deliveriesDeadline); err != nil {
		// The deliveries did not drain in their share. The hooks still end the KV watchers within
		// what is left, so the close does not leave their consumers for the server to time out.
		c.runDrainHooks(conn, deadline)
		return err
	}
	// The drain hooks end what the connection's drain cannot end cleanly, such as the KV watchers'
	// ordered consumers, whose delete would race the connection's drain (AddDrainHook), and they
	// run while the connection still carries their requests.
	c.runDrainHooks(conn, deadline)
	// nats.go's Drain only starts the drain, and the CLOSED event decides when it ends. nats.go
	// v1.55.0 keeps Close terminal when it races Drain, so this event does not rely on a transient
	// DRAINING_PUBS state. The wait stays necessary because Drain returns before the connection
	// closes.
	closed := conn.StatusChanged(nats.CLOSED)
	defer conn.RemoveStatusListener(closed)
	if err := conn.Drain(); err != nil {
		return err
	}
	expired := time.NewTimer(time.Until(deadline))
	defer expired.Stop()
	select {
	case <-closed:
		return nil
	case <-expired.C:
		return fmt.Errorf("drain NATS: %w", context.DeadlineExceeded)
	}
}

// drainHookReserve is the part of Drain's timeout kept from the delivery subscriptions for the
// drain hooks and the connection's own drain, so a delivery still in its handler near the deadline
// cannot leave the hooks no time and make each KV watcher's consumer delete fail at ERROR. A
// delivery's handler can take seconds (a role-lane forward waits up to 2 s for the stream's
// acknowledgement, a session delivery up to its request limit), while a hook's delete is one
// round trip per watcher, run for every watcher at once, and the connection's drain a flush and
// a close: 3 s is comfortably more than one round trip even over a relay. The listener drains for
// 13 s, so its deliveries keep the 10 s they had before the hooks and the hooks get their 3 s on
// top. A drain shorter than 10 s keeps 30% of its timeout instead, so its deliveries still get
// most of it.
const drainHookReserve = 3 * time.Second

// drainDeliveries drains the delivery subscriptions, pass after pass while a bind in flight
// installs one more, and waits until each is no longer valid, at the latest until deadline.
func (c *Client) drainDeliveries(deadline time.Time) error {
	waitUntil := func(done func() bool) error {
		for !done() {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("drain NATS: %w", context.DeadlineExceeded)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	for {
		delivering := c.deliveringSubscriptions()
		if len(delivering) == 0 {
			return nil
		}
		for _, subscription := range delivering {
			if err := subscription.Drain(); err != nil {
				return err
			}
		}
		for _, subscription := range delivering {
			if err := waitUntil(func() bool { return !subscription.IsValid() }); err != nil {
				return err
			}
		}
	}
}

// deliveringSubscriptions returns the installed subscriptions that are still valid, those a pass
// of Drain has yet to drain. Finding none during a Drain, it records that Drain has taken its last
// (clientDrainCollected).
func (c *Client) deliveringSubscriptions() []*nats.Subscription {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	var delivering []*nats.Subscription
	for _, subscription := range c.subscriptions {
		if subscription.active != nil && subscription.active.IsValid() {
			delivering = append(delivering, subscription.active)
		}
	}
	if len(delivering) == 0 && c.stopMode == clientDraining {
		c.stopMode = clientDrainCollected
	}
	return delivering
}

func (c *Client) ensureConn() error {
	return c.ensureConnWithContext(context.Background())
}

// AwaitConnected returns once the client has a usable connection, waiting within ctx for one
// nats.go is reconnecting, as every call of the client's own does first (ensureConnWithContext).
// A caller that reads through the client's connection by a handle opened outside this package,
// a KV read with the reconnect buffer off (connect.go's options), would otherwise fail at once
// while the connection reconnects; it waits here first instead. Its error is ctx's, or the
// client's stop, or the dial of a replacement for a closed connection.
func (c *Client) AwaitConnected(ctx context.Context) error {
	return c.ensureConnWithContext(ctx)
}

// reconnectRecheck is how long ensureConnWithContext's wait for a reconnect goes without reading
// the connection's own status again. nats.go runs a connection's callbacks one after another on
// one goroutine, so the reconnect's event (ConnectionChanged) can wait behind a slow callback
// queued before it while the connection is already connected; the re-read finds it then.
const reconnectRecheck = time.Second

// ensureConnWithContext returns once the client has a usable connection. A connection nats.go is
// reconnecting is waited for, until it reconnects, ctx ends or the client stops, and read again at
// each connection event (ConnectionChanged) and at least every reconnectRecheck, rather than
// polled: with the reconnect buffer off, a publish while reconnecting would otherwise fail at once,
// and a webhook that fails answers 503 to a sender that does not redeliver. A closed connection is
// replaced by dialling a new one, unless the client is stopped. Callers that find the same
// connection closed at once (the recovery its ClosedCB starts, and a publish) each dial one, and
// only the first to finish replaces it: the others close theirs and serve with that one. So every
// connection the client dials is either c.Conn, which Close and Drain close, or closed already; one
// left open beside it would outlive Close, and, with MaxReconnect -1, reconnect forever once its
// server is gone. A client that connects in the background (WithConnectInBackground) holds, from
// its constructor on, a connection nats.go has yet to connect for the first time and does not
// close before then, so a caller waits for that first connection as for a reconnect, and nothing
// here dials beside nats.go before it; a connection dialled later to replace a closed one is an
// ordinary one, connected before it is installed.
func (c *Client) ensureConnWithContext(ctx context.Context) error {
	for {
		c.mu.Lock()
		conn := c.Conn
		// The channel is taken with the connection, before its state is read, so an event that
		// lands between the read and the wait below closes the channel the wait is on.
		changed := c.connChangedLocked()
		c.mu.Unlock()
		// A live connection serves even a stopped client: deliveries still in their handlers during
		// Drain publish their forwards and exceptions through it.
		if conn != nil && (conn.IsConnected() || conn.IsDraining()) {
			return nil
		}
		if c.stopped() {
			return errStopped
		}
		if conn != nil && !conn.IsClosed() {
			c.reconnectWaits.Add(1)
			if c.reconnectWaitTick != nil {
				c.reconnectWaitTick()
				select {
				case <-changed:
					continue
				default:
				}
			}
			recheck := time.NewTimer(reconnectRecheck)
			select {
			case <-ctx.Done():
				recheck.Stop()
				return fmt.Errorf("bus: waiting for NATS to reconnect: %w", ctx.Err())
			case <-c.stopCh:
				recheck.Stop()
				return errStopped
			case <-changed:
			case <-recheck.C:
			}
			recheck.Stop()
			continue
		}

		nc, err := connectWithContext(ctx, c.name, c.urls, c.connectionSettings, c.dialer, c.callbacks(), c.reconnectErrClock)
		if err != nil {
			return err
		}
		js, err := nc.JetStream(nats.MaxWait(10*time.Second), nats.PublishAsyncTimeout(publishAcknowledgementTimeout))
		if err != nil {
			nc.Close()
			return err
		}

		c.mu.Lock()
		// Drain and Close read c.Conn under c.mu after stopping the client, so checking here, under
		// the same lock, means a connection dialled while they ran is either theirs to close or
		// never installed.
		if c.stopped() {
			c.mu.Unlock()
			nc.Close()
			return errStopped
		}
		if c.Conn != conn {
			// Another caller replaced the closed connection first. Its replacement is the one to
			// serve with, so this one closes without its callbacks: its ClosedCB would start a
			// recovery of a client that has a connection, and its DisconnectedErrCB would log
			// "envoy nats disconnected" under the client's name, as if the connection it serves with
			// had dropped. The line below says what happened instead.
			c.mu.Unlock()
			nc.SetClosedHandler(nil)
			nc.SetDisconnectErrHandler(nil)
			nc.Close()
			slog.Info("envoy nats discarded a second replacement connection", slog.String("name", c.name))
			continue
		}
		c.Conn = nc
		c.noteSeverable(nc)
		c.js = c.readsOwnRefusals(nc, js)
		// connectWithContext returns only a connection that connected.
		c.markConnected(nc, true)
		c.markHooksDueLocked()
		c.connectionChangedLocked()
		c.mu.Unlock()
		c.afterInstallForTest(nc)
		return nil
	}
}

// UsesCoreTransport reports whether topic travels over core NATS rather than JetStream
// (publishPathOf), as the client last read the deployed stream's subjects: a role lane or its
// exception lane, which the stream must never retain (TestStreamConfigExcludesRoleLanes) and which
// take core NATS even from a stream that still captures them, and a notification topic that
// neither the deployed stream (DeployedStreamCaptures) nor this build (StreamCaptures) captures:
// an ad-hoc topic such as notifications.lxspec.probe.all and the exception lane of one, and the
// exception lanes of the github, dispatch, slack, legion, ghostwispr and whatsapp topics, since
// the stream captures the exception lanes of agent and team topics alone (streamSubjects).
// JetStream answers a publish to a subject no stream captures only with "no responders", and not
// at all while a core subscriber holds the subject, since that subscriber is handed the
// acknowledgement request: the publish would wait out its whole deadline for an acknowledgement
// that never comes, after the subscribers had already received the message. A subject outside
// NotificationTopicPrefix keeps JetStream.
//
// A topic this build stores never takes core NATS, whatever the deployed stream captures: while
// the deployed stream does not capture it, its publish is refused before anything is sent, once a
// read of the subjects that started after the publish asked agrees (ErrSubjectNotDeployed,
// undeployedOnFreshRead), and UsesCoreTransport answers false for it. Core NATS stores nothing,
// so sending such a topic there, as a narrowed stream once made this client do, hands it only to
// whoever is subscribed at that instant and answers its sender as if it were stored.
//
// A publish judges a topic this answers core for again before it sends it over core NATS, a role
// lane excepted (PublishTakesJetStream), so a topic only a newer build's subject captures, added
// since the client last read them, takes JetStream once that read captures it: the stream stores
// a core publish of a topic it captures too, but under no Nats-Msg-Id, so a repeat is stored
// again, and with no acknowledgement, so a store the stream refuses goes unreported. The
// listener's publish route answers 400 to a core topic other than a role lane (ReachesNoSession)
// once a read of the subjects that started after the request asked leaves it uncaptured
// (SubjectsAsk.Uncaptured), since no session's consumer would hold it.
func (c *Client) UsesCoreTransport(topic string) bool {
	return c.publishPathOf(topic) == overCoreNATS
}

// publishPath is the way a publish of one topic goes (publishPathOf).
type publishPath uint8

const (
	// overJetStream is a publish the stream stores: through JetStream, under its message id.
	overJetStream publishPath = iota
	// overCoreNATS is a publish no stream stores: over core NATS, to the subscribers connected now.
	overCoreNATS
	// refusedUndeployed is a publish of a topic this build stores that the deployed stream does not
	// capture: nothing is sent (ErrSubjectNotDeployed).
	refusedUndeployed
)

func (p publishPath) String() string {
	switch p {
	case overJetStream:
		return "JetStream"
	case overCoreNATS:
		return "core NATS"
	case refusedUndeployed:
		return "refused, not deployed"
	}
	return fmt.Sprintf("publishPath(%d)", uint8(p))
}

// ErrSubjectNotDeployed is wrapped by the error of a publish that sent nothing because its topic is
// one this build stores and the deployed stream does not capture it, as a read of the stream's
// subjects that started after the publish asked confirms (undeployedOnFreshRead). It is none of
// ErrRefused, ErrUnacknowledged, ErrUnavailable, ErrStreamFull and ErrPublishDenied: the same
// publish is stored once the stream captures the topic again, so a caller retries it as any other
// failure. The listener's send and publish routes answer it 503 with Retry-After under a prefix of
// its own that says the message was not sent and may be sent again later (cmd/listener's
// writeBusUnavailable, contracts.MessageNotSentPrefix), not as a bus that did not answer, since the
// bus was never asked, and its webhooks answer 503, which Dispatch's redelivery sweep has GitHub
// send again.
var ErrSubjectNotDeployed = errors.New("nothing was sent: the deployed stream does not capture a topic this build stores")

// subjectNotDeployed is the refusal of a publish of topic that undeployedOnFreshRead refused: it
// wraps ErrSubjectNotDeployed and names the stream and the topic.
func subjectNotDeployed(topic string) error {
	return fmt.Errorf("%w (stream %s, topic %s)", ErrSubjectNotDeployed, Stream, topic)
}

// UndeployedRefusal is the refusal a publish of topic meets before anything is sent, as
// PublishAckWithin answers it (ErrSubjectNotDeployed, from the same judgement,
// undeployedOnFreshRead), or nil for a topic the client would send. Like the publish's, it refuses
// only once a read of the deployed stream's subjects that started after this call agrees with the
// subjects the client holds, and waits within ctx for that read. A caller that does work on a
// publish's behalf before the publish asks it first, so a publish the bus refuses raises nothing:
// cmd/listener's send route answers this refusal before it reports a stopped target's wake, which
// would otherwise go out once per send a sender repeats at Retry-After: 1 for as long as a
// narrowing lasts.
func (c *Client) UndeployedRefusal(ctx context.Context, topic string) error {
	if c.undeployedOnFreshRead(ctx, c.AskDeployedSubjects(), topic) {
		return subjectNotDeployed(topic)
	}
	return nil
}

// undeployedOnFreshRead reports whether a publish of topic is refused as one the deployed stream
// does not capture (ErrSubjectNotDeployed): the subjects the client holds refuse it
// (publishPathOf's refusedUndeployed), and a read of the deployed subjects that started after ask
// leaves it uncaptured too (subjectsReadSince), which waits out the rest of
// streamSubjectsRereadGap after the last such read and shares a read with every judgement asked
// before it started. The subjects the client holds can be streamSubjectsRefresh old, and during a
// roll a newer build's start gives the stream back a subject a narrowing took, so a refusal on
// them alone would reject a message the stream stores. Where no such read can be had - the client
// holds no connection, ctx ends first, the read fails, or its NATS user may not read the stream
// (subjectsRefused) - nothing is refused, and the publish goes through JetStream, which stores it
// when the stream captures the topic and answers no responders when it does not: a topic this
// build stores never takes core NATS. A topic the held subjects send some other way reads nothing.
func (c *Client) undeployedOnFreshRead(ctx context.Context, ask SubjectsAsk, topic string) bool {
	if c == nil || c.publishPathOf(topic) != refusedUndeployed {
		return false
	}
	read := c.subjectsReadSince(ctx, ask.seq)
	return read != nil && !subjectsCapture(read.subjects, topic)
}

// publishPathOf is the one verdict on the path a publish of topic takes, which UsesCoreTransport,
// ReachesNoSession and every publish function (PublishAckWithin) read. Its answers, in order:
//
//  1. A subject outside NotificationTopicPrefix takes JetStream.
//  2. A role lane or its exception lane (RoleLane) takes core NATS.
//  3. A topic the deployed stream captures (DeployedStreamCaptures) takes JetStream, also one only
//     a newer build's subject captures: two builds on one stream both store it. A client that has
//     not read the deployed subjects judges by the subjects this build compiles.
//  4. A topic this build stores (StreamCaptures) that the deployed stream does not capture is
//     refused (refusedUndeployed): nothing is sent, neither over JetStream, where no stream would
//     answer, nor over core NATS, where nothing stores it. That is the verdict of the subjects the
//     client holds, and a publish is refused only once a read that started after it asked agrees
//     (undeployedOnFreshRead); else it takes JetStream.
//  5. Any other topic takes core NATS: an ad-hoc topic no stream stores.
//
// The verdict reads the client's last read of the deployed subjects, which it makes every
// streamSubjectsRefresh and at each judgement that reads them again, and keeps when a read fails.
// So after a narrowing, until the client's next read, a publish takes JetStream and no stream
// answers it ("no responders"); and after the stream captures the topic again, a publish's own
// read finds that, and the publish is stored.
func (c *Client) publishPathOf(topic string) publishPath {
	switch {
	case !strings.HasPrefix(topic, NotificationTopicPrefix):
		return overJetStream
	case RoleLane(topic):
		return overCoreNATS
	case c.DeployedStreamCaptures(topic):
		return overJetStream
	case StreamCaptures(topic):
		return refusedUndeployed
	default:
		return overCoreNATS
	}
}

// DeployedStreamCaptures reports whether the deployed stream retains a publish to topic: some
// subject of the stream matches it, and it is no role lane or exception lane of one, which the
// stream must never retain even where it still captures them.
//
// The capture is judged on the subjects of the deployed stream as the client last read them - read
// at each ensure of an owning client, at connect for any other, every streamSubjectsRefresh, and at
// each read a judgement makes again (startRereadLocked), of which the client keeps, among those
// that ended, the one that started last (keepSubjects) - and on the subjects this build compiles
// (StreamCaptures) only while the client has never read them.
// A nil client has read no deployed stream - a caller that holds none, and the deps a test builds
// without one - so it answers on the compiled subjects too. Two builds can run at once, and the
// deployed stream holds the subjects of both: a topic under a subject only the newer build adds is
// published through JetStream, with its MsgId, by the older build too once its transport has read
// the subjects again (PublishTakesJetStream), and the older build's listener refuses a publish of
// it only on a read made after the publish asked (SubjectsAsk.Uncaptured, undeployedOnFreshRead).
// That listener's registration stores, and its session consumer filter carries, a followed topic
// whatever this answers (cmd/listener's subscribeHandler and sessionConsumerFilter), so neither
// takes a no from subjects read before the newer build added its own.
//
// It decides the transport only together with StreamCaptures (publishPathOf): a topic this build
// stores that the deployed stream does not capture is refused, once a read made after the publish
// asked agrees, and never sent over core NATS. Nor does a session's consumer filter read it: the
// listener's filter holds every topic of the session's interest row whatever the deployed stream
// captures (cmd/listener's sessionConsumerFilter), so a narrowing takes no topic out of it. The
// listener reads it of a message already stored, to judge whether the session's own consumer
// relays that message (cmd/listener's delivery.go).
func (c *Client) DeployedStreamCaptures(topic string) bool {
	if RoleLane(topic) {
		return false
	}
	if c != nil {
		if read := c.deployedSubjects.Load(); read != nil {
			return subjectsCapture(read.subjects, topic)
		}
	}
	return StreamCaptures(topic)
}

// SubjectsAsk is the moment a request asked its client to judge topics on the deployed stream's
// subjects, as a point among the client's reads of them (Client.AskDeployedSubjects): a read
// numbered after it (subjectsReadSeq) took its number, and so sent its request, after the request
// asked. A request takes one ask and judges each of its topics under it (Uncaptured), so the
// topics of one registration share the read the first of them waits for.
type SubjectsAsk struct {
	client *Client
	seq    uint64
}

// AskDeployedSubjects is the ask of a request that judges topics on the deployed stream's subjects
// from now on (SubjectsAsk.Uncaptured). A nil client's ask judges the subjects this build
// compiles.
func (c *Client) AskDeployedSubjects() SubjectsAsk {
	if c == nil {
		return SubjectsAsk{}
	}
	return SubjectsAsk{client: c, seq: c.subjectsReadSeq.Load()}
}

// Uncaptured reports whether the deployed stream leaves topic uncaptured, judged on a read of its
// subjects that started after the ask (AskDeployedSubjects): a subject a newer build added a
// moment before the request asked, inside streamSubjectsRereadGap of the client's last read,
// captures the topic too. It is for a caller that refuses or warns on that
// no: the listener's publish route refuses the publish (ReachesNoSession, cmd/listener's
// publishHandler), and its registration warns that no stream subject captures the topic yet
// (uncapturedTopics), keeping the topic either way. During a roll the newer build's start
// adds its subjects to the stream, and an older pod that read the subjects before then would
// otherwise judge a topic on one of them uncaptured until its next read.
//
// A topic the subjects the client holds capture is captured, with no read. A role lane, or the
// exception lane of one, is uncaptured, and so is a topic outside NotificationTopicPrefix, with no
// read either: no subject a build adds makes either a topic the stream stores for a session. Any
// other topic waits for the client's next read that starts after the ask (subjectsReadSince):
// inside streamSubjectsRereadGap of the last read's end it waits out the rest of the gap, and a
// read in flight that started before the ask is waited for and then read again. Every judgement
// under asks taken before a read started shares that read, so a burst costs the server one
// STREAM.INFO per gap.
//
// Where no such read can be had - the client holds no connection, ctx ends first, or the read
// fails - the answer is captured, so the caller refuses nothing and warns of nothing: a refused
// publish of a topic the stream does capture is a message lost, and an accepted one of a topic it
// does not is a message stored nowhere, which loses nothing a refusal would have kept. ctx bounds
// the caller's wait and not the read, which other callers may share and which runs on to its own
// bound, firstStreamSubjectsRead. The one exception is a client whose NATS user may not read the
// stream (subjectsRefused): no read will ever come, the subjects it holds are all there is, and
// the answer is theirs. A nil client answers on the subjects this build compiles, for the same
// reason.
func (a SubjectsAsk) Uncaptured(ctx context.Context, topic string) bool {
	c := a.client
	if c.DeployedStreamCaptures(topic) {
		return false
	}
	if c == nil || !strings.HasPrefix(topic, NotificationTopicPrefix) || subjectsCapture(roleLaneSubjects, topic) {
		return true
	}
	if read := c.subjectsReadSince(ctx, a.seq); read != nil {
		return !subjectsCapture(read.subjects, topic)
	}
	return c.subjectsRefused.Load() && !c.DeployedStreamCaptures(topic)
}

// subjectsReadSince returns the read of the deployed stream's subjects the client keeps once that
// is a read numbered after asked (subjectsReadSeq), starting one on the connection the client is
// connected over when it must: inside streamSubjectsRereadGap of the last such read's end it waits
// out the rest of the gap first, and a read in flight numbered at or before asked is waited for
// and then read again. It returns nil when no such read can be had: the client holds no
// connection, ctx ends, the read that started after asked failed, or the NATS user may not read
// the stream (subjectsRefused). A read that another started after asked - a periodic read, an
// ensure's, another judgement's - serves too.
func (c *Client) subjectsReadSince(ctx context.Context, asked uint64) *subjectsRead {
	for {
		if kept := c.deployedSubjects.Load(); kept != nil && kept.seq > asked {
			return kept
		}
		if c.subjectsRefused.Load() || ctx.Err() != nil {
			return nil
		}
		c.subjectsRereadMu.Lock()
		reading, seq := c.subjectsRereading, c.subjectsRereadSeq
		var gap time.Duration
		if reading == nil {
			conn := c.ConnectedConn()
			if conn == nil {
				c.subjectsRereadMu.Unlock()
				return nil
			}
			if gap = streamSubjectsRereadGap - time.Since(c.subjectsRereadAt); gap <= 0 {
				reading, seq = c.startRereadLocked(conn)
			}
		}
		c.subjectsRereadMu.Unlock()
		if reading == nil {
			wait := time.NewTimer(gap)
			select {
			case <-wait.C:
			case <-ctx.Done():
				wait.Stop()
				return nil
			}
			continue
		}
		select {
		case <-reading:
		case <-ctx.Done():
			return nil
		}
		if seq > asked {
			// That read started after the ask and has ended: what the client keeps now is its
			// answer or a later read's, and anything older means it failed.
			if kept := c.deployedSubjects.Load(); kept != nil && kept.seq > asked {
				return kept
			}
			return nil
		}
	}
}

// startRereadLocked starts a judgement's read of the deployed subjects on conn, within
// firstStreamSubjectsRead, and returns the channel its end closes and its number, taken here,
// before its request is sent. The caller holds subjectsRereadMu and has found no such read in
// flight.
func (c *Client) startRereadLocked(conn *nats.Conn) (chan struct{}, uint64) {
	reading := make(chan struct{})
	seq := c.subjectsReadSeq.Add(1)
	c.subjectsRereading, c.subjectsRereadSeq, c.subjectsRereadStarted = reading, seq, time.Now()
	go func() {
		c.readDeployedSubjectsAs(context.Background(), conn, firstStreamSubjectsRead, seq)
		c.subjectsRereadMu.Lock()
		c.subjectsRereading = nil
		c.subjectsRereadAt = time.Now()
		c.subjectsRereadMu.Unlock()
		close(reading)
	}()
	return reading, seq
}

// SubjectsRereadEnded is when the last read of the deployed subjects that a judgement started
// (startRereadLocked) ended, zero before the first: the moment streamSubjectsRereadGap runs from.
// It is a seam for tests, which time a stream subject they add inside that gap from it rather than
// from a moment they noticed after the read, which is later than its end; nothing else calls it.
func (c *Client) SubjectsRereadEnded() time.Time {
	c.subjectsRereadMu.Lock()
	defer c.subjectsRereadMu.Unlock()
	return c.subjectsRereadAt
}

// ReachesNoSession reports whether a topic a caller names would deliver to no session at all: a
// notification topic that neither the deployed stream nor this build captures, which takes core
// NATS (UsesCoreTransport), and that is not a role lane. A session receives what its own consumer
// delivers, which is what the stream stored, so a publish to such a topic reaches nobody, now or
// later. POST /v1/messages/publish refuses such a publish by name, since its sender is there to
// read the answer, and POST /v1/interests/subscribe names such a topic in a warning
// (cmd/listener/api.go). Each takes that no from SubjectsAsk.Uncaptured. A role lane is the
// exception the name's "no session" turns on: it travels over core NATS to the listener, which
// forwards the message to its holder's agent subject, where the stream stores it for the holder's
// own consumer.
//
// It is a method for the same reason UsesCoreTransport is: the question is what the DEPLOYED
// stream captures as well as what this binary compiles. While two builds serve one NATS, the
// stream holds the subjects of both, so a topic only the newer build adds reaches a session's
// consumer and the older build must not refuse a publish of it on the subjects this client last
// read (deployedSubjects). A topic this build stores is never one of these, even while the
// deployed stream does not capture it: its publish is refused as a bus fault the sender retries
// (ErrSubjectNotDeployed), not as a topic that reaches nobody, and its interest is kept, since the
// stream stores it again once a narrowing ends.
func (c *Client) ReachesNoSession(topic string) bool {
	return c.UsesCoreTransport(topic) && !strings.HasPrefix(topic, contracts.RoleTopicPrefix)
}

// ErrRefused is returned for an envelope that is refused the same way however often it is
// published, or a KV key or value refused however often a call names it, so a caller answers it as
// a refusal rather than a failure to retry. It is ErrTooLarge, ErrInvalidSubject, ErrInvalidKey,
// ErrNoForwardMsgID, or internal/kvchunk's ReservedKey, a KV key under the chunk keys' prefix that
// a store laying its records out in chunks takes for no record.
var ErrRefused = errors.New("refused")

// refusal is a kind of ErrRefused. Its text names only the kind, since every line and answer that
// reports a refusal already says it is one (`github publish refused: too large to publish whole`).
type refusal string

func (r refusal) Error() string { return string(r) }

func (r refusal) Is(target error) bool { return target == ErrRefused }

// ErrTooLarge is the ErrRefused of an envelope too large to publish whole: a message past the NATS
// server's max payload, which nats.go refuses before sending it, a subject past maxSubjectBytes, a
// KV key whose subject would be, a KV value past the server's max payload or its bucket's largest
// value (KeyValue.tooLarge), or a CI record or settlement past the bound its store keeps below
// those. Its error names the size and the bound.
var ErrTooLarge error = refusal("too large to publish whole")

// ErrInvalidSubject is the ErrRefused of a subject NATS does not accept: an empty one or one holding
// whitespace, which nats.go refuses, or one holding an empty token (`a..b`, or a leading or trailing
// dot), which no stream's subjects match, so a JetStream publish waits out its deadline for an
// answer that never comes and a core publish is dropped. Its error names the subject, or the KV key
// that would have made it.
var ErrInvalidSubject error = refusal("not a subject NATS accepts")

// ErrInvalidKey is the ErrRefused of a KV key outside nats.go's key alphabet (`ses:bad`), which
// nats.go refuses on every read, write, delete and watch before sending anything, wherever the key
// came from: no build writes one, but a direct bucket write can store one, and a stored claim can
// name one as its holder. Its error names the key, and still matches nats.ErrInvalidKey.
var ErrInvalidKey error = refusal("not a key nats.go accepts")

// ErrStreamFull is wrapped by the error of a JetStream publish the stream did not store because it
// holds its byte limit (WithStreamMaxBytes): a full stream refuses new messages rather than drop
// stored ones (streamCfg). It is not an ErrRefused: the same publish is taken once age has made
// room or an operator raised the limit, so a caller retries it as any other failure, and the
// webhook listener answers it 503, which GitHub redelivers. The error still matches nats-server's
// *nats.APIError.
var ErrStreamFull = errors.New("stream " + Stream + " is full")

// jsStreamStoreFailed is nats-server's code for a message the stream did not store
// (JSStreamStoreFailedF), and maxBytesExceeded its description when the store refused the message
// because a DiscardNew stream holds its byte limit (nats-server's ErrMaxBytes). nats.go names
// neither.
const (
	jsStreamStoreFailed = 10077
	maxBytesExceeded    = "maximum bytes exceeded"
)

// streamFull names the server's answer to a JetStream publish that a full stream refused as the
// ErrStreamFull it is, and returns any other error as it is.
func streamFull(err error) error {
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamStoreFailed && apiErr.Description == maxBytesExceeded {
		return fmt.Errorf("%w: %w", ErrStreamFull, err)
	}
	return err
}

// ErrDuplicateInProcess is wrapped by the error of a JetStream publish under a message id that a
// clustered stream refused because an earlier publish under the same id is still in process: the
// stream holds that id's dedupe entry with no sequence yet, so it neither stores this copy nor
// names the one it will hold (nats-server's JSStreamDuplicateMessageConflict). It is not an
// ErrRefused: the same publish sent again once the earlier one is stored is answered as a
// duplicate with that copy's sequence, which is what a caller that reads the acknowledgement
// retries for (PublishStoredAck, PublishAckWithin; cmd/listener's publishPastInProcess, which the
// listener's message routes, role forwards, keep and takeover copy take, and whose doc names the
// publishes that do not, the webhook routes' among them). The error still matches nats-server's
// *nats.APIError.
var ErrDuplicateInProcess = errors.New("a publish under the same message id is still in process")

// jsStreamDuplicateMessageConflict is nats-server's code for a publish whose message id is a
// duplicate of one still in process (JSStreamDuplicateMessageConflict). nats.go v1.55.0 names no
// constant for it.
const jsStreamDuplicateMessageConflict = 10158

// duplicateInProcess names the server's answer to a JetStream publish whose message id is still
// in process under an earlier publish as the ErrDuplicateInProcess it is, and returns any other
// error as it is.
func duplicateInProcess(err error) error {
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamDuplicateMessageConflict {
		return fmt.Errorf("%w: %w", ErrDuplicateInProcess, err)
	}
	return err
}

// storedRefusal names the server's answer to a publish of the stored path as the ErrStreamFull or
// the ErrDuplicateInProcess it is, and returns any other error as it is. It is the mapping
// sendStored makes after c.refused, in a function of its own so that its test
// (duplicate_in_process_test.go) reads the mapping sendStored makes and not a copy of it.
func storedRefusal(err error) error {
	return duplicateInProcess(streamFull(err))
}

// maxSubjectBytes bounds a subject the bus publishes on, or a KV call builds from a key. The server
// closes a connection whose protocol line runs past its max control line (4 KiB by default) and
// nats.go does not check it, so a longer subject would close the connection every subscription and
// watcher of the client runs on, a core publish reporting success first. Besides the subject, the
// longest line nats.go sends here, `HPUB <subject> <reply> <header size> <total size>`, holds a
// 38-byte reply inbox (`_INBOX.<nuid>.<token>`), two sizes of at most seven digits (the 1 MiB max
// payload), its verb, spaces and line ending: 62 bytes, which 64 covers.
const maxSubjectBytes = 4<<10 - 64

// checkSubject refuses, before anything is sent, a subject the server would close the connection
// over (ErrTooLarge) and one NATS does not accept (ErrInvalidSubject).
func checkSubject(subject string) error {
	if len(subject) > maxSubjectBytes {
		return fmt.Errorf("%w: a subject of %d bytes, past %d", ErrTooLarge, len(subject), maxSubjectBytes)
	}
	if !validSubject(subject) {
		return fmt.Errorf("%w: %q", ErrInvalidSubject, subject)
	}
	return nil
}

// validSubject reports whether NATS accepts subject: it is not empty, holds no whitespace, and has
// no empty token.
func validSubject(subject string) bool {
	return subject != "" && subject[0] != '.' && subject[len(subject)-1] != '.' &&
		!strings.Contains(subject, "..") && !strings.ContainsAny(subject, " \t\r\n")
}

// refused names nats.go's refusal of a message past the server's max payload as the ErrTooLarge it
// is, naming its envelope of size bytes. checkSubject has already refused every subject nats.go
// would.
func (c *Client) refused(err error, size int) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		conn := c.Connection()
		return fmt.Errorf("%w: an envelope of %d bytes against the server's max payload of %d bytes",
			ErrTooLarge, size, conn.MaxPayload())
	}
	return err
}

// publishBound bounds Publish and PublishReportingDuplicate, and the publishJetStream and
// PublishCoreContext of a caller that passed no deadline of its own: their wait for a reconnect
// and their flush or JetStream acknowledgement together. A publish window's sends take
// publishAcknowledgementTimeout instead. It is also the HTTP client's per-attempt deadline
// (packages/envoy-client/src/transport.ts), which a publish that spends the whole bound can never
// be answered inside, and that is what the Within forms below exist for.
const publishBound = 5 * time.Second

// Publish routes role lanes, their delivery-exception lanes and every notification topic that
// neither the deployed stream nor this build captures through core NATS (UsesCoreTransport, a
// topic other than a role lane judged again on a read of the deployed stream's subjects first
// where PublishTakesJetStream makes one); every notification the deployed stream captures, as the
// client judges it, retains JetStream durability. A topic this build stores never takes core NATS:
// while the deployed stream does not capture it, as a read of its subjects that started after the
// publish asked confirms, Publish sends nothing and returns ErrSubjectNotDeployed, so its caller
// reports the failure rather than a message nothing stored (undeployedOnFreshRead). Its signature
// is an interface method in cistore, outbox and webhook, so a caller that wants JetStream's
// duplicate verdict calls PublishReportingDuplicate instead, and one that has a deadline of its
// own calls PublishWithin.
func (c *Client) Publish(item contracts.Envelope) error {
	_, err := c.PublishReportingDuplicate(item)
	return err
}

// PublishWithin publishes as Publish does, with ctx as the whole publish's bound - the wait for a
// reconnect, the flush and the JetStream acknowledgement all end with it - instead of publishBound.
// The listener's HTTP handlers publish this way, so a bus they cannot reach is answered inside the
// deadline their own caller gave them.
func (c *Client) PublishWithin(ctx context.Context, item contracts.Envelope) error {
	_, err := c.PublishReportingDuplicateWithin(ctx, item)
	return err
}

// PublishReportingDuplicate publishes as Publish does and reports whether
// JetStream recognised the message as one the stream already holds. That is only
// ever possible for an envelope whose dedupe key names the upstream event
// (contracts.DedupeKeyNamesTheUpstreamEvent), the only one published under a
// MsgId, and only within the stream's duplicate window; a core-transport topic
// has no acknowledgement at all and always reports false.
//
// True means "the stream already held this MsgId", not "this call published
// nothing new": the reconnect retry (sendStored) republishes after a failure that
// may have landed, so the very publish that put the message on the subject can
// come back a duplicate. PublishAckWithin tells that case apart (StoredAck.Resent).
func (c *Client) PublishReportingDuplicate(item contracts.Envelope) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), publishBound)
	defer cancel()
	return c.PublishReportingDuplicateWithin(ctx, item)
}

// PublishReportingDuplicateWithin publishes as PublishReportingDuplicate does, reporting the same
// duplicate verdict over the same two transports, with ctx as the whole publish's bound instead of
// publishBound. A topic the subjects the client holds leave uncaptured (UsesCoreTransport) is sent
// over core NATS once a read of them again within ctx agrees, and on those subjects wherever that
// read is not made (PublishTakesJetStream): inside streamSubjectsRereadGap of the last such read,
// while one is in flight and younger than firstStreamSubjectsRead, with no connection or no grant
// to read the stream, and for a role lane, which reads nothing. During a roll a topic only the
// newer build's subject captures, added since this client last read them, would otherwise go over
// core NATS from an older build's publish: the stream stores that copy too, but under no
// Nats-Msg-Id, so a repeat of an envelope that earns one is stored again, and with no
// acknowledgement, so a store the stream refuses (ErrStreamFull) goes unreported. A read that
// fails, or one ctx ends before, leaves the topic on core NATS as the subjects the client held
// judge it too: a direct publish in the roll window inside the gap or during an unanswered read
// goes over core NATS, stored, with no MsgId and no acknowledgement. A topic this build stores
// that the deployed stream does not capture is refused as PublishAckWithin refuses it
// (ErrSubjectNotDeployed). The verdict is PublishAckWithin's Duplicate, a resend's included.
func (c *Client) PublishReportingDuplicateWithin(ctx context.Context, item contracts.Envelope) (bool, error) {
	ack, err := c.PublishAckWithin(ctx, item)
	return ack.Duplicate, err
}

// DeliveryExceptionDedupePrefix begins the dedupe key of a delivery exception the listener
// publishes on an ExceptionTopicPrefix topic. The listener derives the exception's id from the
// delivery it reports (the event, the recipient and the reason), so a redelivery that ends the same
// way publishes the same key, and the stream stores that exception once.
const DeliveryExceptionDedupePrefix = "envoy.exception."

// namesItsDeliveryException reports whether item is the listener's delivery exception, whose
// dedupe key names the delivery it reports (DeliveryExceptionDedupePrefix).
func namesItsDeliveryException(item contracts.Envelope) bool {
	return item.Source == "envoy" && strings.HasPrefix(item.Topic, ExceptionTopicPrefix) &&
		strings.HasPrefix(item.DedupeKey, DeliveryExceptionDedupePrefix)
}

// StoredAck is the stream's acknowledgement of a stored publish (PublishStoredAck,
// PublishAckWithin): the stream sequence of the copy the stream holds under the publish's message
// id, whether an earlier publish stored that copy, and whether the acknowledgement answers a
// resend. A duplicate's Sequence is that earlier copy's, which nats-server names in its answer, so
// a caller can read the copy the stream already holds (NextStoredMessage at that sequence).
//
// Resent is a publish sent a second time after the connection closed under the first
// (sendStored). The first may have reached the stream before the close, so a Duplicate that comes
// with Resent may name the copy this very call stored: a caller that charges nothing for a repeat
// of an earlier message (cmd/listener's sender limit, and its keep of a role message) reads such a
// verdict as the call's own copy rather than as a repeat. cmd/listener's publishPastInProcess sets
// it for the same reason on the answer to a publish it made again after the stream refused, as
// still in process, a send of the same call that nothing had answered (ErrUnacknowledged).
type StoredAck struct {
	Sequence  uint64
	Duplicate bool
	Resent    bool
}

// PublishStoredAck publishes item to the notification stream on subject, under Nats-Msg-Id msgID,
// none when it is empty, within ctx's bound as PublishWithin's, for a caller that names both the
// subject it is stored on and the stored message itself, and returns the stream's
// acknowledgement. publishJetStream publishes an envelope on its own topic through it, under the
// id it derives. It is also how the listener keeps a role message it could not deliver: stored on
// that role's kept subject (KeptRoleSubject) while the envelope keeps its own topic field, so the
// holder that later takes the role is handed the message as its sender sent it. Every listener
// that keeps the same message writes it under one id (KeptMsgID), and the keep reads the
// acknowledgement to tell a new copy from one the stream already held, and which copy that is
// (cmd/listener/role_keep.go).
//
// A duplicate is no error: it comes back Duplicate, with the sequence of the copy the stream
// holds, and Resent where it answers a resend. Its refusals are those of every stored publish,
// which publishJetStream describes (ErrTooLarge, ErrRefused, ErrPublishDenied, ErrStreamFull), and
// ErrDuplicateInProcess while a clustered stream is still storing an earlier publish under the
// same id; subject must be one the stream captures, or nothing answers the publish. A failure
// after a send that nothing answered wraps ErrUnacknowledged, since the stream may hold the copy
// (sendStored).
func (c *Client) PublishStoredAck(ctx context.Context, subject string, item contracts.Envelope, msgID string) (StoredAck, error) {
	if err := checkSubject(subject); err != nil {
		return StoredAck{}, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return StoredAck{}, err
	}
	ctx, cancel := c.publishAcknowledgementClock.WithTimeout(ctx, publishBound)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return StoredAck{}, err
	}
	options := []nats.PubOpt{nats.Context(ctx)}
	if msgID != "" {
		options = append(options, nats.MsgId(msgID))
	}
	return c.sendStored(ctx, subject, data, options...)
}

// PublishStoredAs is PublishStoredAck for a caller that needs no acknowledgement: the listener's
// exception about a stored message, published on the exception's own topic, which every listener
// that raises it from its own durable publishes under one id, so the stream keeps one copy. A
// duplicate the stream drops counts as published, which is the ordinary case for such an
// exception rather than a fault. Its refusals are PublishStoredAck's.
func (c *Client) PublishStoredAs(ctx context.Context, subject string, item contracts.Envelope, msgID string) error {
	_, err := c.PublishStoredAck(ctx, subject, item, msgID)
	return err
}

// publishJetStream publishes item to the notification stream within ctx's bound, and returns the
// stream's acknowledgement (StoredAck). The MsgId, where the envelope earns one (its key names its
// event, contracts.DedupeKeyNamesTheUpstreamEvent, or it is the listener's delivery exception,
// namesItsDeliveryException), is the dedupe key and the topic together, because one upstream event
// can be published on several topics under one key - a GitHub comment that mentions the trigger is
// published on its mention topic beside its comment topic - and each of those must be retained.
// The bound PublishStoredAck sets is the earlier of the caller's own deadline and publishBound,
// since WithTimeout keeps the parent's deadline when that is the earlier one; publishBound is what
// bounds a caller that passed none. The acknowledgement wait is still taken from
// publishAcknowledgementClock, the seam a test uses to expire that bound without spending it.
//
// A publish the server refuses under the connection's own publish permissions is answered
// ErrPublishDenied, as a publish window's sends are (window.acknowledged): a denied JetStream
// publish is acknowledged by nothing, so its caller would otherwise meet the acknowledgement
// wait's timeout, or a no-responders error, and never the denial. What names it is the violation
// the connection recorded under this very subject (deniedPublish), from the time this publish put
// the message on the wire on: a violation of another subject no longer stands in for this
// publish's, as it did while the connection's one last error was what the denial was read off, and
// one recorded before this send belongs to an earlier publish of the subject.
//
// A ctx that has already ended never puts the message on the wire here, and that protection is
// the dependency's rather than this repository's: js.Publish under nats.Context is a request, and
// nats.go's requestWithContext returns ctx.Err() before it sends anything (nats.go v1.55.0,
// context.go), on the first attempt, on each of its no-responders retries and on the reconnect
// retry below alike. That read comes after ensureConnWithContext has returned, so it also refuses
// a ctx cancelled as a reconnect wait ends. So the core path's own check (publishConfirmed's read
// of ctx) has no counterpart here, and it is not redundant with this one: a change that stopped
// passing nats.Context(ctx), or a nats.go that stopped checking first, would reopen the case on
// this path silently.
// TestAPublishWithAnEndedContextSendsNothing pins it, so either change fails that test instead of
// reaching the wire. What a context that ends during the acknowledgement wait leaves is a message
// the stream may hold while the caller is answered failure; that is the acknowledgement's own
// ambiguity, which the MsgId's duplicate window absorbs for an envelope that earns one, and which
// the failure names (ErrUnacknowledged, sendStored).
func (c *Client) publishJetStream(ctx context.Context, item contracts.Envelope) (StoredAck, error) {
	msgID := ""
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) || namesItsDeliveryException(item) {
		msgID = ForwardMsgID(item.DedupeKey, item.Topic)
	}
	return c.PublishStoredAck(ctx, item.Topic, item, msgID)
}

// ErrUnacknowledged is wrapped by the error of a stored publish that failed and may still have put
// its message in the stream: a send went out while its context was live and nothing answered it
// (sendUnanswered). The stream may hold the message, or store it yet from bytes still on their way
// to the server, so a caller that counts what it stored counts this one too (cmd/listener's sender
// limit). It wraps the failure itself, so every comparison the failure answered
// (context.DeadlineExceeded, nats.ErrConnectionClosed, nats.ErrDisconnected and the rest) still holds.
//
// A failure before anything was sent does not wrap it: a subject or an envelope refused before the
// send, a wait for a reconnect that ended before any send, and a context that had already ended,
// under which nats.go sends nothing (publishJetStream). Nor does a server's answer, a refusal (an
// *nats.APIError, ErrStreamFull, ErrDuplicateInProcess) or no stream answering the subject
// (nats.ErrNoStreamResponse), since the stream stored nothing, unless an earlier send of the same
// call went unanswered: then the resend is what the server refused (sendStored). A denied publish
// (ErrPublishDenied) never wraps it, since the grant that denied it denied every send of the call.
var ErrUnacknowledged = errors.New("the stream did not acknowledge the publish, so it may hold the message")

// sendUnanswered reports whether err, from a JetStream publish handed to nats.go under a live
// context, leaves the send unanswered rather than refused, so that it may have reached the stream.
// It is a positive list of what nats.go v1.55.0's js.PublishMsg returns once its request is out
// and no answer came: under nats.Context, the context's own error when it ends during the
// acknowledgement wait, nats.ErrConnectionClosed when the connection closes with the request
// outstanding (clearPendingRequestCalls closes the reply's channel), and nats.ErrDisconnected
// when reconnect interrupts that request. nats.ErrTimeout is the same wait's end where a publish
// takes a timeout instead of a context. Two of the list also come back before a send,
// nats.ErrConnectionClosed from a connection that closed between the connect and the send and the
// context's error from one that ended in the instant before nats.go's own check, and they count all
// the same, since telling them apart would race the close and the deadline. Every other error is
// the server's answer or a refusal before the send.
func sendUnanswered(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrDisconnected)
}

// unacknowledged is err wrapped in ErrUnacknowledged when a send of the call may have stored the
// message (mayHold), and err as it is otherwise.
func unacknowledged(err error, mayHold bool) error {
	if !mayHold {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnacknowledged, err)
}

// sendStored publishes data to subject on the notification stream under options, once more if the
// connection had closed under it, and names a refusal as the ErrPublishDenied, ErrTooLarge,
// ErrStreamFull or ErrDuplicateInProcess it is. It is the one send of the stored path: its callers
// (PublishStoredAck, publishStoredRaw) bound ctx, connected first and pass nats.Context(ctx)
// among the options, so the acknowledgement wait ends with that bound.
//
// A failure after a send that may have reached the stream wraps ErrUnacknowledged: ctx was live
// as the send was made, read just before it, and what came back was no answer (sendUnanswered).
// Once one send of the call is such a send, every failure after it wraps it too, the resend's and
// that of the reconnect before the resend, since the first send may have stored the message. The
// resend's acknowledgement is Resent, since its Duplicate may name the copy the first send stored.
func (c *Client) sendStored(ctx context.Context, subject string, data []byte, options ...nats.PubOpt) (StoredAck, error) {
	sentAt := time.Now()
	live := ctx.Err() == nil
	ack, err := c.JS().Publish(subject, data, options...)
	mayHold := live && sendUnanswered(err)
	resent := false
	if err != nil && (errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrDisconnected)) {
		if err := c.ensureConnWithContext(ctx); err != nil {
			return StoredAck{}, unacknowledged(err, mayHold)
		}
		sentAt = time.Now()
		resent = true
		live = ctx.Err() == nil
		ack, err = c.JS().Publish(subject, data, options...)
		mayHold = mayHold || live && sendUnanswered(err)
	}
	if err != nil {
		if denied := c.deniedPublish(subject, sentAt); denied != nil {
			return StoredAck{}, denied
		}
		return StoredAck{}, unacknowledged(storedRefusal(c.refused(err, len(data))), mayHold)
	}
	return StoredAck{Sequence: ack.Sequence, Duplicate: ack.Duplicate, Resent: resent}, nil
}

// ErrCursorMoved is wrapped by the error of a PublishCursorAt the stream refused because its
// subject's last sequence was no longer the one the caller read: another writer advanced the
// cursor in between, and this write stored nothing. It is the one failure of that call which says
// the caller lost a race rather than that the bus is unwell, so a caller reads the cursor again
// and decides, where it retries any other failure.
var ErrCursorMoved = errors.New("the cursor moved under this write")

// PublishCursorAt publishes the raw bytes data to the notification stream on subject - bytes of
// the caller's own, not an envelope - only while the stream's last sequence on subject is
// expected, and returns the sequence the stream stored them at. expected is the sequence of the
// message the caller read on subject, and 0 when it read none, which stores only while the stream
// holds nothing there (nats.ExpectLastSequencePerSubject).
//
// It is what the listener's per-role drain cursor is written with. Two listeners draining one role
// at once each read the cursor and write the one after it, and only the first of them may land:
// the second would otherwise move the cursor back over a kept message the first had already
// forwarded, or forward one twice. The loser's error wraps ErrCursorMoved and stored nothing.
// Every other failure keeps the error the publish path reports: ErrPublishDenied for a write the
// connection's user may not make, ErrTooLarge for a subject or body past the server's bounds,
// ErrStreamFull while the stream holds its byte limit, and ctx's error when ctx ends first, which
// wraps ErrUnacknowledged once the write went out, since the stream may then hold it (sendStored).
// A write whose first send went unanswered and whose resend the stream refused for the cursor's
// sequence wraps ErrUnacknowledged too, not ErrCursorMoved: the first send may have stored it and
// moved the cursor itself, so the caller reads the cursor back to tell (cursorMoved).
func (c *Client) PublishCursorAt(ctx context.Context, subject string, data []byte, expected uint64) (uint64, error) {
	ack, err := c.publishStoredRaw(ctx, subject, data, nats.ExpectLastSequencePerSubject(expected))
	if err != nil {
		return 0, cursorMoved(err)
	}
	return ack.Sequence, nil
}

// publishStoredRaw publishes the raw bytes data to the notification stream on subject - bytes of
// the caller's own rather than an envelope - under extra, one publish option beyond the context
// the send already carries, or none when extra is nil, and returns the server's acknowledgement.
// It is the body of every raw write of the stored path: the subject check, the acknowledgement
// bound, the connect and the send (sendStored). Its callers name their own failures, since what
// a refusal means is theirs: PublishCursorAt, which writes under a precondition and reads a
// refused one as ErrCursorMoved, and SessionConsumers.recordResumeFloorAt, which writes a record
// whose one writer keeps no sequence to write against, so the newest write stands.
//
// extra is one option rather than a variadic list because each caller has exactly one or none:
// a list would have the caller build a slice and this method append to it, where the two sends
// below build sendStored's own variadic slice once, exactly as each caller built it before.
func (c *Client) publishStoredRaw(ctx context.Context, subject string, data []byte, extra nats.PubOpt) (StoredAck, error) {
	if err := checkSubject(subject); err != nil {
		return StoredAck{}, err
	}
	ctx, cancel := c.publishAcknowledgementClock.WithTimeout(ctx, publishBound)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return StoredAck{}, err
	}
	if extra == nil {
		return c.sendStored(ctx, subject, data, nats.Context(ctx))
	}
	return c.sendStored(ctx, subject, data, nats.Context(ctx), extra)
}

// cursorMoved names the server's refusal of a publish whose expected last sequence per subject did
// not match as the ErrCursorMoved it is, and returns any other error as it is. The refusal is
// either code JetStream answers a lost conditional write with (IsCASConflict): on a stream of more
// than one copy a write that meets another listener's cursor write still being replicated is
// answered 10164, not 10071, and has lost the cursor all the same. A refusal that wraps
// ErrUnacknowledged is returned as it is too: it refused a resend after a send that went
// unanswered (sendStored), and that first send may have stored this write, so it is no proof the
// write stored nothing.
func cursorMoved(err error) error {
	if IsCASConflict(err) && !errors.Is(err, ErrUnacknowledged) {
		return fmt.Errorf("%w: %w", ErrCursorMoved, err)
	}
	return err
}

// PublishCoreContext publishes item directly to subject under ctx, confirming the server accepted
// it (publishConfirmed), so a nil error means it did: a publish NATS denies (a permissions
// violation of a per-client grant), a flush that fails and a publish the server cannot be shown
// to have accepted all return an error. The subject can differ from item.Topic when an
// authoritative router forwards an envelope while retaining its original topic for the recipient.
//
// ctx is the whole publish's bound, so it is both what the wait for a reconnect ends with and the
// window publishConfirmed flushes and confirms within, and a caller answering inside a deadline
// of its own (the listener's routes: a publish through PublishWithin) is never held past it.
// The bound taken below is the earlier of ctx's own deadline and publishBound, since WithTimeout
// keeps the parent's deadline when that is the earlier one; publishBound is what bounds a caller
// that passed none.
//
// A ctx that has ended puts nothing on the wire: ensureConnWithContext returns its error rather
// than wait for a reconnect or dial one, and publishConfirmed refuses it immediately before the
// publish, which also catches a cancel that lands as a reconnect completes. A deadline arrives
// spent through publishHandler (cmd/listener/api.go), whose reads before the publish (the sender
// stamp's) share the route's deadline with it.
func (c *Client) PublishCoreContext(ctx context.Context, subject string, item contracts.Envelope) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, publishBound)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	conn := c.Conn
	return c.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: data}, conn.PublishMsg)
}

// ForwardMsgID is the Nats-Msg-Id of an envelope whose dedupe key is dedupeKey
// sent to subject: the key, a colon, the subject. A publish and a window's
// JetStream send stamp an envelope that earns a MsgId with it
// (publishJetStream, window.sendJetStream), so each topic one upstream event
// is published on keeps its own copy, and a forward passes it to
// StartRequestStoredTo as the forward's Nats-Msg-Id. A stream that captures
// subject stores a forward sent again to the same subject once, within its
// duplicate window, and one sent to another subject again, as a message of its
// own there.
func ForwardMsgID(dedupeKey, subject string) string {
	return dedupeKey + ":" + subject
}

// KeptMsgID is the Nats-Msg-Id of the kept copy of a role message whose dedupe key is dedupeKey:
// its forward id on that role's kept subject (ForwardMsgID, KeptRoleSubject). Every
// listener of the deployment takes the same raw role publish on its own role-lane subscription,
// so each of them may find the role undeliverable and write a kept copy of that one message.
// Under one id the stream's duplicate window collapses those copies into the single stored copy
// the next holder of the role is handed.
func KeptMsgID(dedupeKey, role string) string {
	return ForwardMsgID(dedupeKey, KeptRoleSubject(role))
}

// ErrNoForwardMsgID is the ErrRefused of a forward sent with no Nats-Msg-Id. A forward's one
// lasting copy is the stored one, so the name the stream keeps it under is what holds it to one
// copy per message and recipient however often the forward is sent, and every caller has that
// name (ForwardMsgID). An unnamed forward is stored again by every attempt, so
// StartRequestStoredTo refuses it rather than send it, as a window's Request does
// (RequestOptions.MsgID).
var ErrNoForwardMsgID error = refusal("a forward without a " + nats.MsgIdHdr)

// ErrForwardNotStored is returned by a forward's Wait when the stream's acknowledgement that it
// stored the forward did not arrive before the deadline, or came back as the stream's error: the
// forward's one lasting copy, which the holder's session consumer delivers, is missing. The
// forward does not count as delivered; the listener reports it delivery_failed.
var ErrForwardNotStored = errors.New("bus: the stream did not acknowledge storing the forward")

// StartRequestStoredTo publishes a forward of item to subject and returns once the server has it,
// leaving the wait for the stream's acknowledgement to the returned forward's Wait, so a caller
// can publish forwards in order on one goroutine and wait for each on another. The window,
// timeout, starts with this call. Every holder is reached this way: no session subscribes to its
// agent subject any more (INFRA-771), so the forward's one lasting copy is the stored one, which
// the holder's session consumer delivers through the listener's relay, with acknowledgement and
// redelivery.
//
// The forward carries msgID as its Nats-Msg-Id header, and an empty msgID is refused
// ErrNoForwardMsgID before anything is sent. A sender that forwards one
// envelope again under one MsgId until it is stored leaves the stream one copy per message and
// recipient, however many attempts it makes, and the stream acknowledges each repeat as a
// duplicate, which counts as stored. An error here is the publish's own, before any reply could
// arrive (a refused, denied or invalid forward, a connection that never came back within
// timeout); every returned forward must be waited for, which releases its reply subscription.
func (c *Client) StartRequestStoredTo(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (*PendingForward, error) {
	if msgID == "" {
		return nil, fmt.Errorf("%w: to %q", ErrNoForwardMsgID, subject)
	}
	if err := checkSubject(subject); err != nil {
		return nil, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	conn := c.Conn
	inbox := conn.NewInbox()
	replies, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	forward := &nats.Msg{
		Subject: subject,
		Reply:   inbox,
		Data:    data,
		Header:  nats.Header{nats.MsgIdHdr: []string{msgID}},
	}
	err = c.publishConfirmed(ctx, conn, forward, conn.PublishMsg)
	if err != nil && !errors.Is(err, ErrPublishUnconfirmed) {
		_ = replies.Unsubscribe()
		return nil, err
	}
	return &PendingForward{replies: replies, deadline: deadline, unconfirmed: err}, nil
}

// PendingForward is a forward StartRequestStoredTo published, whose replies Wait reads.
type PendingForward struct {
	// replies is the forward's reply inbox, which the stream answers on.
	replies  *nats.Subscription
	deadline time.Time
	// unconfirmed is publishConfirmed's error for a publish it could not confirm, which the
	// stream's acknowledgement may still overrule; nil when it confirmed the publish.
	unconfirmed error
}

// Wait reads the forward's replies until the stream has acknowledged storing it or its window
// ends, releases its reply subscription, and returns the stream's acknowledgement: the sequence
// of the copy it holds under the forward's Nats-Msg-Id, and Duplicate where an earlier forward
// stored that copy. No acknowledgement by the deadline is ErrForwardNotStored, wrapping, when
// publishConfirmed could not confirm the publish, the reason for that. The stream's error answer,
// the server's no-responders answer (nothing captured the subject), and the stream's duplicate
// acknowledgement that names no sequence are each ErrForwardNotStored wrapping that answer. A
// clustered stream's refusal of a forward whose Nats-Msg-Id an earlier forward still has in
// process wraps ErrDuplicateInProcess too, since the same forward sent again once the earlier one
// is stored is acknowledged as its duplicate (cmd/listener's publishPastInProcess). The duplicate
// that names no sequence wraps ErrDuplicateWithoutSequence, which says the stream answered and no
// later reply comes, so a caller that answers for the forward can say so at once rather than wait
// out its window. Any other reply is not the stream's acknowledgement and is passed over.
func (f *PendingForward) Wait() (StoredAck, error) {
	defer f.replies.Unsubscribe()
	for {
		remaining := time.Until(f.deadline)
		if remaining <= 0 {
			break
		}
		response, err := f.replies.NextMsg(remaining)
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		if err != nil {
			// The server's no-responders answer: nothing, not even a stream, took the forward.
			if errors.Is(err, nats.ErrNoResponders) {
				return StoredAck{}, fmt.Errorf("%w: %w", ErrForwardNotStored, err)
			}
			return StoredAck{}, err
		}
		ack, ok := readPublishAcknowledgement(response.Data)
		if !ok {
			// A stream's duplicate with sequence 0 names no copy (nats-server v2.10.29 sent one for
			// an id it had staged at 0; the 2.11 line answers that with the in-process error, and
			// later lines stage nothing): nothing says the forward is stored, and no later reply
			// comes. Any other reply that is no acknowledgement (readPublishAcknowledgement) is
			// passed over.
			if ack.Stream != "" && ack.Duplicate {
				return StoredAck{}, fmt.Errorf("%w: %w", ErrForwardNotStored, ErrDuplicateWithoutSequence)
			}
			continue
		}
		// The stream's error answer to the forward, which no later reply changes.
		if ack.Error != nil {
			return StoredAck{}, fmt.Errorf("%w: %w", ErrForwardNotStored, duplicateInProcess(ack.Error))
		}
		return StoredAck{Sequence: ack.Seq, Duplicate: ack.Duplicate}, nil
	}
	if f.unconfirmed != nil {
		return StoredAck{}, fmt.Errorf("%w: %w", ErrForwardNotStored, f.unconfirmed)
	}
	return StoredAck{}, ErrForwardNotStored
}

// ErrDuplicateWithoutSequence is wrapped by a forward's Wait when the stream answered it as a
// duplicate of a copy it named no sequence of, so no copy can be read back (PendingForward.Wait).
var ErrDuplicateWithoutSequence = errors.New("the stream acknowledged a duplicate with no sequence")

// publishAcknowledgement is a stream's answer to a publish it captured: the stream, the sequence
// it stored the message at (a duplicate's earlier sequence) and whether it was a duplicate, or its
// error.
type publishAcknowledgement struct {
	Stream    string         `json:"stream"`
	Seq       uint64         `json:"seq"`
	Duplicate bool           `json:"duplicate"`
	Error     *nats.APIError `json:"error"`
}

// readPublishAcknowledgement reads data as a stream's publish acknowledgement, reporting false for
// any other reply: one that is not JSON, or JSON naming neither a stored sequence nor an error.
func readPublishAcknowledgement(data []byte) (publishAcknowledgement, bool) {
	var ack publishAcknowledgement
	if json.Unmarshal(data, &ack) != nil {
		return publishAcknowledgement{}, false
	}
	return ack, ack.Error != nil || (ack.Stream != "" && ack.Seq > 0)
}
