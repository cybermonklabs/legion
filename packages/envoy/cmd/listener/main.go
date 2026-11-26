package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/envconf"
	"github.com/sjawhar/envoy/internal/ghrename"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/webhook"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/listenfd"
	"github.com/sjawhar/envoy/natsversion"
)

// listenerDeps holds the NATS-dependent resources, all open. main builds the /v1 handlers over it
// once the stores are open. /healthz, which answers during startup too, reads it through an
// atomic.Pointer that stays nil until main has started the receive loop; the list gauges read it
// through another that openV1Routes publishes when /v1 opens (registerListGauges).
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	// known is the store of every session that registered in the last 30 days
	// (session.KnownSessions). It keeps a cache of its own outside caches below, so the
	// self-health monitor never restarts a listener over it. A listener whose NATS user may not
	// open its bucket holds a store that answers no record and writes none until an attempt of its
	// own opens the bucket; nil here is a listener that could not build a JetStream context and a
	// test that builds the handlers without a store. Every method of it is safe on a nil store,
	// so no call site checks.
	known *session.KnownSessions
	// legionAgents is the Legion agent registry the /v1/legion/agents routes serve, or why it is
	// unavailable. It keeps no cache, so it is in no list of caches.
	legionAgents *legionAgentRegistry
	// ciStore is nil on a listener that mounts no GitHub webhook route, the one route that uses it.
	ciStore *cistore.Store
	// roleLane is the role lane's own client (bus.ConnectRoleLane), nil in a test that builds the
	// handlers without it.
	roleLane *bus.Client
	// consumers manages the session consumers registrations ask for (sessionDelivery); nil in a
	// test that builds the handlers without it, which refuses every registration with
	// consumer_error. delivery, below, is the delivery handlers' own configuration, which the
	// routes share.
	consumers *sessionDelivery
	// caches lists the stores above that are open, for the sites that treat every cache alike.
	caches []listenerCache
	// receiver is the durable's receive loop, whose role and lag /healthz reports.
	receiver *durableReceiver
	// wakes publishes the wake reports a role send with no holder and an unplaceable agent
	// message raise, off those paths and under the listener's own lifetime (wakeReporter); nil
	// in a test that builds the deps without it, which reports no wake.
	wakes      *wakeReporter
	streamName string
	streamInfo streamInfoLookup
	// delivery is the config main hands the delivery handlers (the role lane and the durable),
	// wired once, so the publish route forwards through its forwardRole, records each session it
	// confirms in its delivery ledger (so no lane forwards that key to the session again, on this
	// pod or any other of the machine), and writes each send's delivery line and delivery_status
	// count through its logger and messagesDelivered, reading as the role lane's do, beside the
	// route's own request record (request_record.go). The route never reads that ledger for its
	// own dedupe answer: the ledger records what this machine delivered, a forward this pod
	// published, which is not this route's own forward to the holder.
	delivery *listenerDeliveryHandlerConfig
	// confirmed is the publish route's own record of each (dedupe key, session) the route stored a
	// forward for, which only the route writes, and which this pod alone holds (README, "State the
	// pods share"). A send skips a holder or recipient recorded there, so envoy-client's retry of
	// a send does not forward again to one the first attempt already had the stream store the
	// message for.
	confirmed *dedupe.Cache
	// sendsToUnresponsive counts the sends POST /v1/messages/send took for a recipient past the
	// live window and stored for it (sendHandler). It is how a box's operator sees that a growing
	// share of its traffic is going to sessions that have stopped answering, before any of them
	// reaches the gone horizon.
	sendsToUnresponsive *metrics.Counter
	// senderLimit is the operator's limit on the messages each agent session sends through this
	// listener (sender_limit.go). nil is no limit, which is what every test that builds
	// listenerDeps itself runs with.
	senderLimit *senderLimit
	// dispatchPrincipal is ENVOY_DISPATCH_PRINCIPAL, Dispatch's own verified principal
	// (resolveListenerDispatchPrincipal): the one caller whose send may carry a role mark
	// (sendHandler). Empty names nobody, and every send carrying one is refused.
	dispatchPrincipal string
	// relaySockets is an edge listener's relay sockets (relay_socket.go), nil on every other
	// listener, which answers the socket's path 404 (relaySocketRoute).
	relaySockets *relaySockets
	// repositories is the edge listener's reader of the repositories its projects' issues
	// resolve to (the broker's GET /v1/project-repositories, edge_repositories.go), nil on every
	// listener but an edge one.
	repositories *edgeRepositories
	// registrationBudget is the store work budget of POST /v1/interests/subscribe
	// (subscribeHandler), timed from the request's arrival: zero is sendRequestBudget, which every
	// listener main builds runs with. It is a seam for a test whose own work runs inside that
	// budget, as a session's consumer lock it holds while it writes, or a hook a pass calls: such a
	// test gives the registration what is left of the test binary's deadline here, so the budget
	// does not count the test's work, which a loaded machine slows as it slows the bus.
	registrationBudget time.Duration
}

// registrationBudgetOr is the registration's store work budget (registrationBudget):
// sendRequestBudget unless a test set one.
func (d *listenerDeps) registrationBudgetOr() time.Duration {
	if d.registrationBudget == 0 {
		return sendRequestBudget
	}
	return d.registrationBudget
}

// kvCache is what the listener asks of each KV-backed cache it keeps.
type kvCache interface {
	Rewatch(context.Context, *nats.Conn) error
	WatchErr() error
	// NeedsRead reports a copy that does not follow its bucket with no read under way to make it
	// (kvwatch.Watcher.NeedsRead), which only a Rewatch repairs.
	NeedsRead() bool
	// Disconnected says the connection the cache's watcher reads through was lost
	// (kvwatch.Watcher.Disconnected).
	Disconnected()
	Ping() error
	StopWatch()
	EndWatch(context.Context)
}

// listenerCache is one cache under the name /healthz and self-health report it by.
type listenerCache struct {
	name  string
	cache kvCache
}

// listenerCaches lists the caches the listener keeps. It is the one place that names them, so
// rewatch, self-health, /healthz and shutdown each reach every cache. ciStore is nil on a listener
// that mounts no GitHub webhook route, and then no site reaches a CI cache.
func listenerCaches(registry *store.Registry, sessions *session.SessionRegistry, ciStore *cistore.Store) []listenerCache {
	caches := []listenerCache{
		{name: "interest", cache: registry},
		{name: "session", cache: sessions},
	}
	if ciStore != nil {
		caches = append(caches, listenerCache{name: "CI", cache: ciStore})
	}
	return caches
}

// sessionMayBeLive is the reapers' test of a session: it answers false only for a session that is
// gone (session.Gone), and the session bucket's stream leader is what says so (liveSessionEntry: a
// record the cache holds, or else a read of the session's own key from the leader,
// session.SessionRegistry.RefreshFromLeader). A direct get would not do: on a replicated bucket a
// copy that has not applied a registration made a moment ago answers that the session has no
// record, and the reaper would delete a live session's interest or release its role. So a reaper
// takes nothing from a session that has merely stopped answering - its interests and its role
// claims stand for the whole gone horizon - nor from one another listener registered a moment
// ago, nor from one the session cache does not hold because it has not read its bucket yet or is
// reading it again. Its error is a read that did not answer, on which the reaper keeps what it
// would have removed for its next cycle.
//
// A reaper answers no caller, so it holds no request context and reads under context.Background()
// with roleHolderReadTimeout as its bound, the same bound each read of role delivery's resolution
// waits.
func sessionMayBeLive(sessions *session.SessionRegistry, sessionID string) (bool, error) {
	_, _, err := liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, notGone, sessions.RefreshFromLeader)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, nil
	}
	return true, err
}

// roleHolderMayBeLive is the role reaper's liveness check (sessionMayBeLive), logging a read that
// did not answer, on which the claim is kept for the next cycle.
func roleHolderMayBeLive(logger *logging.Logger, sessions *session.SessionRegistry, sessionID string) bool {
	live, err := sessionMayBeLive(sessions, sessionID)
	if err != nil {
		logger.Warn("role claim reaper kept a claim whose holder's session it could not read", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
	return live
}

// defaultSessionReapInterval is how often the session reaper runs when ENVOY_SESSION_REAP_INTERVAL
// names no other period. A record is deleted within one period of reaching the gone horizon, and
// the bucket's maximum age is settled once per period, so a raise made elsewhere reaches this
// listener that soon (session.ReapSessions): a minute, which is what the two-minute adoption this
// change is accepted on needs, and what the setting's documentation states.
const defaultSessionReapInterval = time.Minute

// defaultRoleClaimReapInterval is how often the role claim reaper runs when
// ENVOY_ROLE_CLAIM_REAP_INTERVAL names no other period: the five minutes the fixed pass this
// change replaces ran at, which the deployment keeps.
//
// It is not the session reaper's period, short though that is. A session's record reads gone
// when the session bucket expires its key as well as when its last registration passes the gone
// horizon, and the bucket ships with a five-minute maximum age (session.BucketMaxAge), so a
// stalled session's key is gone five minutes after its last heartbeat and the reaper then finds
// its holder gone. Judged every minute, a stalled holder would lose its role up to five times
// sooner than the fixed pass this change replaces, which is the opposite of what this change is
// for; the raise of that maximum age past the gone horizon is dispatch://INFRA-887's, and it
// cannot land first. The grace a restored claim gets - and a claim the hand-back marked gets
// none, since no registration renews a marked claim - is measured from the listener's own start
// against the live window (store.Registry.releaseReadExpiredRoleClaim, which the reaper's pass
// hands the claim it read), not counted in passes, so the period does not move it; what the
// period decides is how long after a holder is gone its role is free.
const defaultRoleClaimReapInterval = 5 * time.Minute

// warnIgnoredEnv writes one WARN naming an environment variable this listener no longer reads,
// for each such variable present in the environment at all - an empty value is a variable an
// operator still sets, so it is still reported. reason says what the listener stopped doing, so
// the line tells the operator why the setting configures nothing now, and every line ends by
// asking for its removal from the deployment. The listener starts either way: a deployment that
// still carries the setting must keep running until its operator catches up, which is the
// warn-and-continue line envDuration takes for a value it cannot use. The message and its fields
// are what an operator's log filter matches, so they name the variable exactly as it is written
// in the environment.
func warnIgnoredEnv(logger *logging.Logger, name string, reason string) {
	value, set := os.LookupEnv(name)
	if !set {
		return
	}
	logger.Warn(name+" is set and ignored: "+reason+"; remove it from the deployment",
		slog.String("value", value))
}

// rewatchListenerKVWatchers recreates cache watchers that are not represented by bus.Client
// subscriptions, each move waiting on nothing past ctx. It attempts every one so a failed rebuild
// of one cannot leave the other caches permanently stale too.
func rewatchListenerKVWatchers(ctx context.Context, conn *nats.Conn, caches []listenerCache) error {
	var errs []error
	for _, c := range caches {
		if err := c.cache.Rewatch(ctx, conn); err != nil {
			errs = append(errs, fmt.Errorf("rewatch %s cache: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// endListenerKVWatchers ends every cache's watchers at shutdown, all at once and within ctx, while
// the connection still carries their consumer deletes (kvwatch.Watcher.End); the bus's Drain runs
// it as a drain hook before it drains the connection.
func endListenerKVWatchers(ctx context.Context, caches []listenerCache) {
	var ended sync.WaitGroup
	for _, c := range caches {
		ended.Go(func() { c.cache.EndWatch(ctx) })
	}
	ended.Wait()
}

// hookListenerKVWatchers ties every cache's KV watcher to the client's connection. KV watchers are
// not bus subscriptions, so a disconnect tells each cache that its copy stopped following its
// bucket (nats.go keeps a watcher's subscription open through an outage, so nothing else would),
// every recovered connection rewatches them, which reads each bucket again, and the drain ends
// them before the connection's own.
func hookListenerKVWatchers(client *bus.Client, caches []listenerCache) {
	client.AddDisconnectHook(func() {
		for _, c := range caches {
			c.cache.Disconnected()
		}
	})
	client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
		return rewatchListenerKVWatchers(ctx, conn, caches)
	})
	client.AddDrainHook(func(ctx context.Context) {
		endListenerKVWatchers(ctx, caches)
	})
}

// knownSessionMoveHook is the known-session store's reconnect hook: it moves the store to conn
// (session.KnownSessions.Rewatch) and answers nil, logging a WARN for a move that failed while its
// run was live. A move whose run is over (bus.RunEnded, the rule the bus decides a superseded run
// by) logs nothing, so a race the bus reads as superseded is not a WARN here
// (TestTheKnownSessionHookLogsNothingForARunThatIsOver).
func knownSessionMoveHook(known *session.KnownSessions, logger *logging.Logger) func(context.Context, *nats.Conn) error {
	return func(ctx context.Context, conn *nats.Conn) error {
		if err := known.Rewatch(ctx, conn); err != nil && !bus.RunEnded(ctx, conn) {
			logger.Warn("the known-session cache did not move to the recovered connection; the store retries the move on its own backoff",
				slog.String("bucket", bus.KnownSessionBucket), slog.String("error", err.Error()))
		}
		return nil
	}
}

// isUnrecoverableSelfHealthFailure distinguishes state that must be rebuilt
// from transient JetStream deadlines. Rebuild only while the NATS client is
// connected; a disconnected client owns its own infinite reconnect loop. A
// missing bucket is terminal too: a rebuild cannot create a bucket, so its
// failed rebuilds end in a restart, whose store Open creates it again. A cache
// whose copy needs a read (kvCache.NeedsRead) is rebuilt as a stopped watcher
// is: nothing but a Rewatch reads its bucket again, for instance after the
// reconnect hook's Rewatch failed while the old watcher survived. A role lane
// failure is never one (checkSelfHealth), whatever it wraps: the error of a send
// that failed on a lane connection closed under it says nothing of the main bus,
// which a rebuild would rebuild.
func isUnrecoverableSelfHealthFailure(err error, client *bus.Client, caches []listenerCache) bool {
	if err == nil || errors.Is(err, errRoleLaneInactive) || !client.Connected() {
		return false
	}
	if errors.Is(err, nats.ErrConsumerNotFound) || errors.Is(err, errDurableUnbound) ||
		errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrStreamNotFound) {
		return true
	}
	for _, c := range caches {
		if c.cache.WatchErr() != nil || c.cache.NeedsRead() {
			return true
		}
	}
	return false
}

// rebuildListenerDependencies rewatches the KV caches and, when the durable probe reports the
// consumer gone or not delivering to this listener's subscription, subscribes the durable again
// (resubscribe), creating it when it is gone. The resubscribe reports success only once the server
// reports this listener bound, so the probe that follows a rebuild reads a settled consumer rather
// than the gap in which the subscription is being replaced. Only the receiving pod's probe reports
// the consumer gone or unbound; a standby's never does, so a standby's rebuild touches nothing but
// the caches.
//
// Nothing here touches relay ownership: the receive term alone elects the pod that relays its
// machine's session consumers (durableReceiver.begin and durableReceiver.end,
// sessionRelays.setOwned). A resubscribe reports success for a pod with no term as well
// (durableReceiver.resubscribe), so a rebuild that set the flag would have a standby relay beside
// the holder.
func rebuildListenerDependencies(
	ctx context.Context,
	client *bus.Client,
	caches []listenerCache,
	durableProbe func() error,
	resubscribe func(context.Context) error,
) error {
	if !client.Connected() {
		return nats.ErrConnectionClosed
	}
	err := rewatchListenerKVWatchers(ctx, client.Conn, caches)
	if durableProbe == nil {
		return err
	}
	probeErr := durableProbe()
	if !errors.Is(probeErr, nats.ErrConsumerNotFound) && !errors.Is(probeErr, errDurableUnbound) {
		return err
	}
	if durableErr := resubscribe(ctx); durableErr != nil {
		return errors.Join(err, fmt.Errorf("resubscribe the durable consumer: %w", durableErr))
	}
	return err
}

// disconnectedFailureThreshold bounds how many consecutive probe failures the self-health monitor
// tolerates while the NATS client reads as not connected (RECONNECTING or CLOSED, mid-recovery).
// nats.go retries a dropped connection every second (ReconnectWait) on its own, so a client still
// disconnected after this many intervals is not inside a brief reconnect gap: the transport itself
// has not come back (a NATS restart or deploy taking longer than expected, or a local forward that
// keeps accepting connections without actually reaching the server), and nats.go's in-place
// reconnect has no way to notice that and give up on its own. At the production interval (30s)
// this is 10 minutes of reading as disconnected. A transport that stalls the open connection
// rather than closing it reads as connected until nats.go's ping check marks it stale (its
// defaults: a ping every 2 minutes, 2 outstanding), so there the exit comes about 15 minutes
// after the stall.
const disconnectedFailureThreshold = 20

// runSelfHealthMonitor leaves transient dependency timeouts degraded while
// NATS reconnects. A terminal watcher, closed KV handle, or missing durable
// consumer is rebuilt immediately, and a rebuild that reports success is
// probed at once: a healthy probe is a recovery and resets the count, so
// separate faults that each rebuild repairs never add up. A failed probe
// there, transient or terminal, keeps the count and becomes the error the
// terminal line names. Only a terminal failure still there after threshold
// consecutive rebuilds enters the bounded shutdown path so Docker can replace
// an unrecoverable listener.
// A probe that lands in a reconnect gap is that transient case: it fails at
// once with "outbound buffer limit exceeded" while the bus is disconnected (its
// reconnect buffer is off), or at its deadline when the reconnect lands during
// the probe. Either is one WARN, then "self-health recovered" from the next -
// unless connected reads false for disconnectedFailureThreshold consecutive
// probes, in which case the bounded shutdown path runs for this too. That
// count is its own, advanced only while connected reads false and reset the
// moment it reads true again, so it never mixes with a transient failure that
// happens while the client is connected (an unclassified JetStream timeout,
// say), which keeps retrying under the unbounded path above.
func runSelfHealthMonitor(
	ctx context.Context,
	logger *logging.Logger,
	probe func() error,
	isUnrecoverable func(error) bool,
	connected func() bool,
	rebuild func() error,
	terminate func(),
	interval time.Duration,
	threshold int,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	terminalFailures := 0
	disconnectedFailures := 0
	// readsDisconnected classifies every probe result, the one right after a rebuild included, so
	// the disconnected count is the same run of probes wherever in the loop they fall.
	readsDisconnected := func() bool { return connected != nil && !connected() }
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := probe()
		if err != nil {
			failures++
			disconnected := readsDisconnected()
			bound := threshold
			if disconnected {
				bound = disconnectedFailureThreshold
			}
			logger.Warn("self-health probe failed", slog.Int("consecutive", failures), slog.Int("threshold", bound), slog.String("error", err.Error()))
			if disconnected {
				disconnectedFailures++
				terminalFailures = 0
				if disconnectedFailures >= disconnectedFailureThreshold {
					logger.Error("self-health disconnected failure threshold exceeded; terminating for restart",
						slog.Int("consecutive", disconnectedFailures),
						slog.String("error", err.Error()),
					)
					if terminate != nil {
						terminate()
					}
					return
				}
				continue
			}
			disconnectedFailures = 0
			if isUnrecoverable == nil || !isUnrecoverable(err) {
				terminalFailures = 0
				continue
			}
			terminalFailures++
			if rebuild != nil {
				if rebuildErr := rebuild(); rebuildErr != nil {
					// The rebuild runs under ctx, so a shutdown that cancels ctx mid-rebuild cuts it
					// with context.Canceled. That error is the shutdown, not a failed rebuild, and an
					// ordered shutdown writes no ERROR, so the monitor stops here without logging it.
					if ctx.Err() != nil {
						return
					}
					logger.Error("self-health rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", rebuildErr.Error()))
				} else {
					logger.Info("self-health rebuild started", slog.Int("consecutive", terminalFailures))
					// Whatever this probe reads, transient or terminal, the count keeps its
					// increment: only a healthy read is a recovery. A failure of the role lane alone
					// is the one exception: the rebuild repaired what it rebuilds, and the lane is
					// neither rebuilt nor a cause of a restart (checkSelfHealth), so such a read
					// ends the count as a healthy one would, while failures keeps counting and
					// `self-health recovered` waits for the lane.
					if err = probe(); err != nil {
						logger.Warn("self-health probe after rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", err.Error()))
						if readsDisconnected() {
							// The connected branch above reset the count, so this probe starts it.
							disconnectedFailures = 1
						} else if errors.Is(err, errRoleLaneInactive) {
							terminalFailures = 0
						}
					}
				}
			}
		}
		if err == nil {
			if failures > 0 {
				logger.Info("self-health recovered", slog.Int("prior_consecutive_failures", failures))
			}
			failures = 0
			terminalFailures = 0
			disconnectedFailures = 0
			continue
		}
		if terminalFailures < threshold {
			continue
		}
		logger.Error("self-health terminal failure threshold exceeded; terminating for restart",
			slog.String("error", err.Error()),
		)
		if terminate != nil {
			terminate()
		}
		return
	}
}

// errCacheNeedsRead is the self-health failure of a cache whose copy does not follow its bucket
// with no read under way (kvCache.NeedsRead).
var errCacheNeedsRead = errors.New("the cache does not follow its bucket and no read of it is under way")

// errRoleLaneInactive is the self-health failure, and the /healthz error, of a role lane that
// forwards no role message (roleLaneInactive).
var errRoleLaneInactive = errors.New("role lane subscription inactive")

// roleLaneInactive reports whether lane, the role lane's own client (bus.ConnectRoleLane),
// forwards no role message: its connection is not connected, or it holds no valid subscription
// that the server accepted (bus.Client.CoreSubOK). A nil lane, an edge listener's, which opens
// none (startRoleLane), or a test's that builds the handlers without it, is not inactive. /healthz
// and the self-health probe both read it.
func roleLaneInactive(lane *bus.Client) bool {
	return lane != nil && (!lane.Connected() || !lane.CoreSubOK())
}

// checkRoleLane is the self-health probe's check of the role lane: errRoleLaneInactive while the
// lane is inactive (roleLaneInactive). When the server refused the lane's subscription on a
// connection that stays connected, the check first sends the subscription again
// (bus.Client.ResendRefusedCore), in the shared queue group and then in the machine's. The server
// applies a grant change by a reload of its configuration, which keeps the connection, so this
// send is what brings the lane back after a reload gives the grant back. The probe reports the
// lane as it found it, and the next probe reads the lane the send left.
func checkRoleLane(lane *bus.Client) error {
	if !roleLaneInactive(lane) {
		return nil
	}
	if err := lane.ResendRefusedCore(); err != nil {
		return fmt.Errorf("%w: sending it again failed: %w", errRoleLaneInactive, err)
	}
	return errRoleLaneInactive
}

// checkSelfHealth pings the KV-backed caches, reports one whose copy needs a read, runs the
// optional durable consumer probe and checks the role lane (checkRoleLane). Returns the first error
// encountered, or nil when all are healthy. A copy that needs a read answers its Ping, so without
// its own failure here the monitor would never rebuild it (isUnrecoverableSelfHealthFailure).
//
// The role lane is checked first, so a refused lane is sent again whatever else fails, and its
// failure is returned last: each failure before it is one the monitor may rebuild, which the
// monitor decides from the error returned. A role lane's failure is never rebuilt and never
// counts toward a restart. A restart would stop every other route, and a start whose grant still
// lacks the role lane waits at its subscribe with /v1 closed (startRoleLane, openStartConnection).
// So every probe while the lane stays inactive logs its WARN, `self-health probe failed`, and the
// first healthy one logs `self-health recovered`. Each pod, the lease holder and each standby,
// probes its own lane: a lane follows no lease.
func checkSelfHealth(caches []listenerCache, roleLane *bus.Client, durable func() error) error {
	roleLaneErr := checkRoleLane(roleLane)
	for _, c := range caches {
		if err := c.cache.Ping(); err != nil {
			return fmt.Errorf("%s kv: %w", c.name, err)
		}
		if c.cache.NeedsRead() {
			return fmt.Errorf("%s kv: %w", c.name, errCacheNeedsRead)
		}
	}
	if durable != nil {
		if err := durable(); err != nil {
			return fmt.Errorf("durable consumer: %w", err)
		}
	}
	return roleLaneErr
}

// sessionHealthFields is the session cache's state as /healthz reports it. Kept separate from the
// handler so it stays cheap (no Keys()/List(), just in-memory reads) and unit-testable.
func sessionHealthFields(sessions *session.SessionRegistry) *listenerwire.SessionCache {
	watchError := ""
	if err := sessions.WatchErr(); err != nil {
		watchError = err.Error()
	}
	return &listenerwire.SessionCache{
		SessionCacheReady: sessions.CacheReady(),
		SessionCacheSize:  sessions.CacheSize(),
		SessionWatchError: watchError,
		// The maximum age in use: the age the session bucket really carries, which the registry
		// reads back after it writes the deployment's own (ENVOY_SESSION_MAX_AGE) and again on
		// every pass of the session reaper. With the setting unset it is the registry's default,
		// so the field is always present once the registry is open, and an operator can see from
		// here whether a record stands long enough for the gone horizon to be reached.
		SessionMaxAge: sessions.TTL().String(),
	}
}

// healthzDeadline bounds one /healthz answer. A dependency that has not answered when it passes
// makes the answer 503, so a readiness probe whose own timeout is longer always gets an answer.
const healthzDeadline = 5 * time.Second

// healthAnswer is one /healthz answer: its HTTP status and its JSON body.
type healthAnswer struct {
	code int
	body listenerwire.Health
}

// healthzHandler is the listener's /healthz, the readiness answer of every pod of a machine id. It
// answers 503 "starting" until main publishes deps, which it does once NATS is connected, the
// webhook and /v1 gates are open and the receive loop has started, and until the interest copy has
// completed its first read of its bucket since the process started (interestCopyRead), which a
// later read of it does not undo.
// Then it answers 200 "healthy" once every dependency answers, naming this pod's "role": "active"
// while it receives the durable (it holds the lease and is subscribed, or it runs without a lease
// and receives), with the durable's lag (num_pending, num_ack_pending), or "standby" otherwise,
// naming the holder in "active_instance" when the lease record names one and, while it holds the
// lease but does not receive yet, what it waits for in "receive". A pod that is the machine's
// deliverer and does not receive yet - it holds the lease, or runs without one, or has read no
// holder at all - is 200 "degraded" rather than healthy, however well its dependencies answer:
// nothing of the durable reaches a session until it receives. It stays ready, since a readiness
// probe that takes any 200 keeps routing /v1 to it, as it does to a standby. A standby of another
// holder is ready and healthy: it serves /v1, the webhooks and the role lane, and takes over
// delivery when the lease comes free. A listener without the lease's grant says so in "lease"
// (listenerLeaseUnavailable) until it joins the lease (joinDeliveryLease). It answers 503
// "unhealthy" when NATS is unavailable, the role
// lane's own client is not connected or holds no valid core subscription, since no role message
// is forwarded then, a cache's watcher has stopped, a bus check did not answer within
// healthzBudget, or on the active pod when its subscription is inactive, the durable consumer is
// gone, or the durable consumer is "not bound to this listener's subscription" because it was
// recreated server-side and now delivers elsewhere; and 200
// "degraded" for a transient KV failure the self-health monitor and the NATS reconnect retry, an
// active pod's durable lag that could not be read, or a KV watch rebuild stalled past
// bus.RepairGrace (below). A pod that holds the lease, or runs without one, and fails to read,
// create, correct or subscribe to the durable answers 200 "degraded" with the failure in "error"
// and how long it has failed in "receive_failing_ms", and 503 "unhealthy" once that has lasted
// the alarm bound (durableTakeoverAlarmAfter): a holder gives the lease up then and is a standby
// again. A failure that is the NATS grant's refusal of the durable's read, create or correction
// answers 503 "unhealthy" from the first refused try for as long as the refusal lasts, and the
// holder keeps the lease through it (durableReceiver.prepare); /livez answers 204 throughout.
// A listener that mounts no GitHub webhook route keeps no CI cache, and its answer
// carries "ci_cache": "not_applicable". The checks run under one healthzDeadline, and an answer
// they have not reached by then is 503 "unhealthy".
//
// A rewatch of the KV caches that keeps failing after a reconnect ends no watcher and clears no
// cache, so every other check still passes while the caches follow buckets they no longer watch
// (INFRA-743). The handler therefore reads the client's HookStall, how long the bus's reconnect
// hooks have been due without a run that succeeded, and reports the dependency "KV watch rebuild"
// once it reaches bus.RepairGrace. It reads it last, below every 503 check and immediately before
// either 200 answer, an active pod's and a standby's alike (okAnswer), so a stall never stands in
// for a worse fault: a durable consumer that is gone, unbound, or whose info read did not answer,
// a subscription that is not valid and a takeover that has failed past durableTakeoverAlarmAfter
// are each still 503, since a listener that receives nothing is not the case the 200 is for. That
// answer is 200 "degraded" and not 503, because the probes act on /healthz: the ECS container
// health check is curl -sf /healthz, every 30 s with 3 retries
// (infra/aws/rendering/listener.go, the healthCheck entry of
// ListenerContainerDefinitions), and GKE's readiness probe reads it too
// (infra/gcp/dispatch_kube.go, readyProbe). A 503 would unroute, or replace, a pod whose caches
// still hold what they warmed up with, and its replacement rebuilds into the same failing rewatch
// with cold caches that warm up empty, whose fanout then acknowledges general-topic messages as
// unmatched and loses them (delivery.go, fanoutDelivery); a standby taken out of service this way
// is also one pod less to take the lease over. The paging signal for this state is the bus's ERROR
// "envoy nats reconnect hooks have not succeeded" (internal/bus/recovery.go, reportHookFailure),
// which the AWS rendering counts as a log metric filter (infra/aws/rendering/metricfilters.go). A
// shorter stall is reported nowhere here: the bus runs the hooks again within seconds of a failure
// (internal/bus/recovery.go, recoverUntilHealthy), so a rebuild under the bound is being repaired,
// and a listener would otherwise read as degraded over every reconnect. The stall is one
// bus.Client's, naming no cache, so a listener that keeps no CI cache needs no branch of its own.
//
// The checks that go to the bus share one healthzBudget taken from that deadline: the flush, each
// cache's KV ping and the active pod's durable consumer info read. A ping or that read that has not
// answered when it ends (healthzPing) is 503 unhealthy naming the dependency and the context's
// deadline, so a stall in JetStream alone, which the flush's core PING does not see, fails the
// container health check inside the budget, as the 10 s JetStream wait it replaces failed it by
// running past the check's timeout. A check that answered with a failure keeps its answer.
//
// The role claim view is reported too, below every 503 check and as a 200 "degraded" naming it
// (store.Registry.RoleViewErr, read in okAnswer): until its watcher's first scan has delivered
// every claim the view answers no read, so GET /v1/sessions and GET /v1/registry/<session_id>
// answer 503 while every other check here passes, and the report reads the very state those
// routes read. Every pod of a machine serves those routes, the lease holder and each standby
// alike (openV1Routes), so the active pod's 200 and a standby's each carry it. It is read there
// so that it hides nothing: every check that can answer 503 has passed by then, so a degraded
// view never takes the place of a fault that does not repair itself. A KV ping or durable info
// read that answered with a transient failure is reported before it, as its own 200 "degraded"
// carrying only status and error (dependencyHealth), and returns before the checks after it. The
// role claim view's degraded answer instead carries the healthy body's own fields, so a reader
// loses no counter while the view fills, and it stands beside the other reports a 200 carries - a
// standby's failing takeover, an active pod's durable lag that could not be read, the KV watch
// rebuild stall, which is named after it - rather than in place of any of them. Degraded and not
// unhealthy, because that view fills itself through its watcher's own rescan and a 503 would take
// a serving listener out of a load balancer for a state that repairs itself: a readiness check
// runs curl -f or -sf against /healthz, which a 200 passes. For the same reason the view is no
// part of Ping or WatchErr, whose terminal failures the self-health monitor self-terminates the
// listener over: a restart completes no scan that keeps timing out.
//
// The answer reports the role claim copy's freshness too, under "role_claim_repair"
// (listenerwire.RoleClaimRepair): when the role claim watcher's last whole read of the role bucket
// completed, as RFC 3339 in UTC, and how long ago on the registry's clock, both null until a read
// has completed; the bound that age is held to, store.RoleClaimRepairBound, in seconds; and how many
// claims that read put back into the copy and took out of it (store.Registry.RoleClaimRepair). The
// healthy answer carries it, and so does every degraded answer that keeps the healthy body's
// fields, so a reader and an alert see how stale the copy is in either. A copy whose last whole
// read is older than the bound (store.RoleClaimRepairReport.Err) is 200 "degraded" with the
// error "role claim repair unavailable: …", keeping every other field. It is judged after the role
// claim view and only while the view answers: a view that cannot answer refuses every read of the
// copy whatever its last read's age, and is reported as itself. Degraded and never unhealthy,
// whatever the age: the dispatch-listener's readiness probe runs curl -fsS against /healthz and
// would take that pod out of service in about ten seconds on a 503, and every listener runs this
// one binary. A late repair is the scheduled read's to repair, while the grant guard
// (roleOwnSessionVerdict) refuses to give a role away from its holder meanwhile, and GET
// /v1/sessions keeps answering the copy, which it refuses only when the view cannot be served. The
// answers dependencyHealth gives carry only status and error, as before: they report a fault
// below the copy, not its freshness.
//
// Every answer, starting, healthy, degraded and unhealthy alike, also carries the fields issuers
// sets (issuerHealthFields: the identity token issuers' "oidc" and "google"); nil sets neither,
// which leaves both null. Those fields never decide the status (writeHealth).
//
// The route serves this handler behind the start's own answer (registerProbeRoutes): while the
// listener's start waits out a refusal of its NATS credential, /healthz answers 503 naming the
// refusal before this handler is asked (natsStart.healthz). Before the start's first refusal and
// once the connect that met it has got in (natsStart.admitted), this handler answers, 503
// "starting" until the pod can serve (listenerHealth).
func healthzHandler(deps *atomic.Pointer[listenerDeps], issuers func(*listenerwire.Health)) http.HandlerFunc {
	// The latch of the interest copy's first read (interestCopyRead): main builds this handler once,
	// so it lasts as long as the process.
	var interestsRead atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthzDeadline)
		defer cancel()
		answered := make(chan healthAnswer, 1)
		go func() { answered <- listenerHealth(ctx, deps.Load(), &interestsRead) }()
		var answer healthAnswer
		select {
		case answer = <-answered:
		case <-ctx.Done():
			answer = healthAnswer{http.StatusServiceUnavailable, listenerwire.Health{
				Status: listenerwire.HealthUnhealthy,
				Error:  "health checks did not finish within " + healthzDeadline.String(),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		writeHealth(w, answer, issuers)
	}
}

// listenerHealth runs /healthz's checks over d, nil until main publishes it, within ctx, its bus
// checks within healthzBudget of it. interestsRead is the handler's latch of the interest copy's
// first read (interestCopyRead).
func listenerHealth(ctx context.Context, d *listenerDeps, interestsRead *atomic.Bool) healthAnswer {
	if d == nil || !interestCopyRead(d, interestsRead) {
		return healthAnswer{http.StatusServiceUnavailable, listenerwire.Health{Status: listenerwire.HealthStarting}}
	}
	unhealthy := func(body listenerwire.Health) healthAnswer {
		body.Status = listenerwire.HealthUnhealthy
		return healthAnswer{http.StatusServiceUnavailable, body}
	}
	ctx, cancel := context.WithTimeout(ctx, healthzBudget)
	defer cancel()
	if err := busAnswers(ctx, d); err != nil {
		return unhealthy(listenerwire.Health{Error: "nats unavailable"})
	}
	if roleLaneInactive(d.roleLane) {
		return unhealthy(listenerwire.Health{Error: errRoleLaneInactive.Error()})
	}
	for _, c := range d.caches {
		if err := c.cache.WatchErr(); err != nil {
			return dependencyHealth(c.name+" KV watcher", err, true)
		}
	}
	for _, c := range d.caches {
		if answered, err := healthzPing(ctx, c.cache); err != nil {
			return dependencyHealth(c.name+" KV", err, !answered || c.cache.WatchErr() != nil || errors.Is(err, nats.ErrConnectionClosed))
		}
	}
	response := listenerwire.Health{Status: listenerwire.HealthHealthy, SessionCache: sessionHealthFields(d.sessions)}
	if d.ciStore == nil {
		response.CICache = "not_applicable"
	}
	report := d.receiver.report()
	if report.NoLease {
		response.Lease = listenerLeaseUnavailable
	}
	if report.State != receiveActive {
		response.Role = listenerwire.RoleStandby
		response.ActiveInstance = report.Lease.ActiveInstance
		if report.State != receiveStandby {
			response.Receive = report.State.String()
		}
		if report.Deliverer {
			// This pod is the machine's deliverer and does not receive yet (receiveReport.Deliverer):
			// nothing of the durable reaches a session until that changes, which no answer of this
			// pod may call healthy. It stays 200, as a standby does, so a readiness probe keeps
			// sending it /v1.
			response.Status = listenerwire.HealthDegraded
		}
		if report.Failing > 0 {
			response.Error = "durable consumer unavailable: " + report.FailingReason
			failing := report.Failing.Milliseconds()
			response.ReceiveFailingMS = &failing
			if report.Stuck || report.Refused {
				return unhealthy(response)
			}
			response.Status = listenerwire.HealthDegraded
		}
		return okAnswer(response, d)
	}
	response.Role = listenerwire.RoleActive
	if !report.SubscriptionValid {
		return unhealthy(listenerwire.Health{Role: listenerwire.RoleActive, Error: "subscription inactive"})
	}
	consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, d.receiver.consumer, nats.Context(ctx))
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
		return dependencyHealth("durable consumer", err, true)
	case err != nil && ctx.Err() != nil:
		// A read ctx ended did not answer, the info read's own form of healthzPing's unanswered.
		return dependencyHealth("durable consumer", err, true)
	case err != nil:
		response.Status = listenerwire.HealthDegraded
		response.Error = "durable consumer unavailable: " + err.Error()
	case consumerInfo.Config.DeliverSubject != report.Subject:
		// One read and no confirming second one, unlike the self-health probe's: the endpoint
		// reports the fault the moment a request finds it, and a request landing in a
		// resubscribe's own gap simply answers 503 until the next one.
		return dependencyHealth("durable consumer", errDurableUnbound, true)
	default:
		response.DurableLag = &listenerwire.DurableLag{NumAckPending: consumerInfo.NumAckPending, NumPending: consumerInfo.NumPending}
	}
	return okAnswer(response, d)
}

// interestCopyRead reports whether d's interest copy has completed its first read of the interest
// bucket, which /healthz answers "starting" until. /v1 opening does not say so: the start waits at
// most 30 s for the interest watcher and opens /v1 when that bound runs out too, and the watcher
// counts as ready after a first scan that timed out (store.Registry.WaitForCacheReady), so /v1 can
// be open while GET /v1/interests/ answers its refilling 503 (store.Registry.Servable). That
// fail-open stays: the routes that do not need the copy serve from then on, and only readiness
// waits for the read.
//
// Only the first read gates. The first time the registry answers Servable nil, read records it,
// and from then on this answers true without asking again, so a later read of the copy - after a
// reconnect, a scan that timed out, or a stale episode past its bound - does not make a pod that
// has been Ready "starting" again, while the routes that need the copy answer their own refilling
// 503. Readiness that followed the copy at all times would turn a NATS blip, which makes every pod
// of a machine read its buckets again at once, into a Service with no endpoints, and every route
// would fail, not only the list. read is the handler's own (healthzHandler). A deps built without
// a registry, which only a test builds, has no copy to wait for.
func interestCopyRead(d *listenerDeps, read *atomic.Bool) bool {
	if read.Load() {
		return true
	}
	if d.registry != nil && d.registry.Servable() != nil {
		return false
	}
	read.Store(true)
	return true
}

// roleClaimRepairHealthOf is report as /healthz's "role_claim_repair" field answers it
// (listenerwire.RoleClaimRepair): the role claim watcher's last whole read of the role bucket that
// completed (store.Registry.RoleClaimRepair), its age to the millisecond on the registry's clock,
// the age store.RoleClaimRepairReport.Err judges, beside the bound that age is held to,
// store.RoleClaimRepairBound.
func roleClaimRepairHealthOf(report store.RoleClaimRepairReport) *listenerwire.RoleClaimRepair {
	health := &listenerwire.RoleClaimRepair{
		BoundSeconds: store.RoleClaimRepairBound.Seconds(),
		PutBack:      report.PutBack,
		TookOut:      report.TookOut,
	}
	if report.Completed > 0 {
		completedAt := report.CompletedAt.UTC().Format(time.RFC3339Nano)
		age := report.Age.Round(time.Millisecond).Seconds()
		health.CompletedAt, health.AgeSeconds = &completedAt, &age
	}
	return health
}

// healthzBudget bounds the bus checks of one /healthz answer, and /readyz's one check.
const healthzBudget = 3 * time.Second

// busAnswers is the bus check /healthz and /readyz share: a flush of the listener's connection, a
// core PING its server must answer within ctx. Its failure is what /healthz answers 503 "nats
// unavailable" for, first of its checks, and what /readyz answers 503 "unavailable" for once /v1
// is open, since then /v1's routes answer bus-unavailable 503s (readyzHandler).
func busAnswers(ctx context.Context, d *listenerDeps) error {
	return d.client.Conn.FlushWithContext(ctx)
}

// healthzPing is cache's Ping as a /healthz request holding ctx waits for it (bus.Bounded): nats.go's
// KV Status takes no context, so a ping that has not answered when ctx ends is answered false, with
// ctx's error, the "context deadline exceeded" a ping that waited out the JetStream MaxWait failed
// with. The abandoned ping runs on to its answer or that wait; what it can still do then - record a
// recreated bucket's error on its watcher, or prune the session cache's last-seen times - is what
// any later ping would do.
func healthzPing(ctx context.Context, cache kvCache) (answered bool, err error) {
	_, answered, err = bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, cache.Ping() })
	return answered, err
}

// dependencyHealth is /healthz's answer for a dependency that failed with err: 503 "unhealthy"
// when the failure is terminal, else 200 "degraded".
func dependencyHealth(dependency string, err error, terminal bool) healthAnswer {
	statusCode := http.StatusOK
	status := listenerwire.HealthDegraded
	if terminal {
		statusCode = http.StatusServiceUnavailable
		status = listenerwire.HealthUnhealthy
	}
	return healthAnswer{statusCode, listenerwire.Health{
		Status: status,
		Error:  dependencyUnavailable(dependency, err),
	}}
}

// okAnswer is /healthz's 200 answer over response, and the last thing the endpoint decides. Only
// the 200 answers come through here, below every 503 check, so no report it adds ever stands in
// for a worse fault.
//
// It reads the role claim view first (store.Registry.RoleViewErr): until that view answers, the
// two routes that report a session's roles - GET /v1/sessions and GET /v1/registry/<session_id> -
// answer 503 while every other check passes, and every pod of a machine serves them, so an active
// pod's answer and a standby's each carry it.
//
// Then the role claim copy's repair (store.Registry.RoleClaimRepair), read once and after the
// view, so the "role_claim_repair" field and its report are one reading, and a view that answers
// already has a completed whole read in that reading. Every answer through here carries the field;
// a repair past its bound is reported only while the view answers, since a view that cannot answer
// is what every read of the copy is refused by.
//
// Then the rewatch: a rewatch of the KV caches that keeps failing after a reconnect ends no
// watcher and clears no cache, so every other check answers while the caches follow buckets they
// no longer watch (INFRA-743); this reads the client's HookStall for exactly that. A stall that
// has reached bus.RepairGrace names the "KV watch rebuild", and a shorter one is reported
// nowhere: the bus runs the hooks again within seconds of a failure (internal/bus/recovery.go,
// recoverUntilHealthy), so a rebuild under the bound is being repaired and a listener would
// otherwise read as degraded over every reconnect.
//
// Each report makes the answer "degraded" and is carried beside, not instead of, one the pod
// already has - a standby's failing takeover, an active pod's durable lag that could not be read,
// the role claim view or its late repair - in the order they are read here (carryReport): every
// one of them is this same 200 "degraded", and an operator reading the answer wants the whole of
// what is wrong, not whichever part the endpoint found last.
func okAnswer(response listenerwire.Health, d *listenerDeps) healthAnswer {
	viewErr := d.registry.RoleViewErr()
	repair := d.registry.RoleClaimRepair()
	response.RoleClaimRepair = roleClaimRepairHealthOf(repair)
	if viewErr != nil {
		carryReport(&response, "role claim view", viewErr)
	} else if err := repair.Err(); err != nil {
		carryReport(&response, "role claim repair", err)
	}
	stall, hookErr := d.client.HookStall()
	if stall < bus.RepairGrace {
		return healthAnswer{http.StatusOK, response}
	}
	rebuild := fmt.Errorf("no rebuild has succeeded for %s", stall.Round(time.Second))
	if hookErr != nil {
		// The last failure says what is broken; the stall, for how long. A run still in flight past
		// the grace has no error of its own, and a dependency report needs one, so then the stall
		// is the whole of it.
		rebuild = fmt.Errorf("%w (%w)", hookErr, rebuild)
	}
	carryReport(&response, "KV watch rebuild", rebuild)
	return healthAnswer{http.StatusOK, response}
}

// carryReport adds a dependency's failure to a 200 /healthz answer: the answer is "degraded", and
// a report it already carries keeps its place, with this one named after it.
func carryReport(response *listenerwire.Health, dependency string, err error) {
	reported := dependencyUnavailable(dependency, err)
	if response.Error != "" {
		reported = response.Error + "; " + reported
	}
	response.Status = listenerwire.HealthDegraded
	response.Error = reported
}

// writeHealth writes answer as one /healthz answer, with the fields issuers sets added
// (issuerHealthFields; nil sets neither, which leaves both null). Those fields are reports beside
// the answer and never decide its status, which the answer carries before they are set: a
// listener whose issuer cannot be read serves everything else, so a probe that failed on it would
// take that service away for an outage the listener serves through, as Dispatch's service_tokens
// field does not.
func writeHealth(w http.ResponseWriter, answer healthAnswer, issuers func(*listenerwire.Health)) {
	if issuers != nil {
		issuers(&answer.body)
	}
	w.WriteHeader(answer.code)
	_ = json.NewEncoder(w).Encode(answer.body)
}

// dependencyUnavailable is the `error` field a /healthz answer gives for a dependency, so the
// degraded answer that keeps the healthy body's fields reads exactly as dependencyHealth's does.
func dependencyUnavailable(dependency string, err error) string {
	return dependency + " unavailable: " + err.Error()
}

// webhookRoute is one configured webhook path and the handler it serves over the only
// dependencies a webhook uses: the publisher it publishes through and the CI store the GitHub
// route records checks in, which main opens only when that route is configured.
//
// The handler is built here, and not before webhookRoutes is called, because neither is open when
// main registers the paths: the routes answer 503 until they are.
type webhookRoute struct {
	path    string
	handler func(webhook.Publisher, *cistore.Store) http.Handler
}

// webhookBodyRefusedMetric counts the webhook deliveries the body budget turned away with a 503
// (webhook.BodyBudget), by route path, so a sustained fill of the budget shows as a rate.
const webhookBodyRefusedMetric = "envoy_webhook_body_refused_total"

// webhookRoutes lists the webhook routes the configuration enables. main registers the paths
// before NATS is up, so they answer 503 while it connects, and builds the handlers once NATS and,
// for the GitHub route, the CI store are open.
//
// Every route reads its bodies against one budget of cfg.BodyBudget bytes, made here once for the
// listener (webhook.BodyBudget), so what anonymous senders can make the listener hold before any
// signature is checked is bounded across every route at once, not per route.
//
// Every route publishes through the rename publisher, which costs the Slack and the Ghost Wispr
// routes nothing: a pair names a GitHub repository, and a subject that is not
// `notifications.github.<owner>.<name>.…` names none, so those two publish exactly as they did.
func webhookRoutes(cfg *webhook.WebhookConfig, refused *metrics.Counter) []webhookRoute {
	bodies := webhook.NewBodyBudget(cfg.BodyBudget, refused)
	var routes []webhookRoute
	if github := cfg.GitHub; github != nil {
		routes = append(routes, webhookRoute{"/webhook/github", func(publisher webhook.Publisher, ciStore *cistore.Store) http.Handler {
			return webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, github.VerdictAppID, publisher, ciStore, bodies)
		}})
	}
	if slack := cfg.Slack; slack != nil {
		routes = append(routes, webhookRoute{"/webhook/slack", func(publisher webhook.Publisher, _ *cistore.Store) http.Handler {
			return webhook.SlackHandler(slack.Secret, publisher, bodies)
		}})
	}
	if ghostWispr := cfg.GhostWispr; ghostWispr != nil {
		routes = append(routes, webhookRoute{"/webhook/ghostwispr", func(publisher webhook.Publisher, _ *cistore.Store) http.Handler {
			return webhook.GhostWisprHandler(ghostWispr.Secret, publisher, bodies)
		}})
	}
	return routes
}

// newHTTPServer is the listener's HTTP server over handler. Every route reads its header and body
// within ReadTimeout, 10 s from the request's start, and writes its answer within WriteTimeout,
// 10 s from the end of the header. The GitHub webhook route alone moves its own connection's
// deadlines (webhook.GitHubHandler), since a large delivery's body can take longer than that to
// arrive; every other route keeps these.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// openWebhooks builds the webhook routes over the publisher they publish through and the CI store
// (nil without the GitHub route) and opens the webhook gate onto them. A webhook waits on nothing
// else, not even the interest and session caches /v1 waits on, and GitHub does not redeliver a
// delivery refused while they warm.
func openWebhooks(gate *startingGate, hooks []webhookRoute, publisher webhook.Publisher, ciStore *cistore.Store) {
	routes := http.NewServeMux()
	for _, hook := range hooks {
		routes.Handle(hook.path, hook.handler(publisher, ciStore))
	}
	gate.open(routes)
}

// openV1Routes builds the /v1 routes over the stores, opens the /v1 gate onto them and publishes
// the stores to served, which the list gauges read (registerListGauges). No /v1 handler reads the
// durable consumer, so /v1 serves on every pod of a machine, the lease holder and each standby
// alike, and callers that resolve the listener's name reach any of them, so a list /v1 answers
// then has its sample.
func openV1Routes(gate *startingGate, ready *listenerDeps, served *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger, listeningAt time.Time) {
	routes := http.NewServeMux()
	registerV1Routes(routes, ready, machineID, logger)
	gate.open(routes)
	served.Store(ready)
	logger.Info("envoy-listener /v1 open", slog.Int64("since_listening_ms", time.Since(listeningAt).Milliseconds()))
}

// listenerInstance names this process among the listeners that run for one machine id: the
// ENVOY_INSTANCE_ID environment variable when it is set, else the host name, which in Kubernetes is
// the pod's name.
func listenerInstance() (string, error) {
	if instance := os.Getenv("ENVOY_INSTANCE_ID"); instance != "" {
		return instance, nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("instance: ENVOY_INSTANCE_ID is unset and the host name is unreadable: %w", err)
	}
	if hostname == "" {
		return "", errors.New("instance: ENVOY_INSTANCE_ID is unset and the host name is empty")
	}
	return hostname, nil
}

// startRetry is how long a start waits between attempts to reach NATS while it cannot answer: the
// connect (connectListener), the spent-proof bucket (openSpentProofBucket), each store's buckets
// (openStartStore), the listener state bucket (openDeliveryLease), and the connections the start
// opens once the stores are open (openStartConnection). It is the third group of the start rule
// (cmd/listener/README.md, "Start"); a server that answers from its cluster's state, a grant that
// refuses a step, or an account's limits or the server's state that refuse one, waits by
// nextRefusedWait instead (waitOutClusterState, waitOutGrantRefusal, waitOutAccountState).
const startRetry = 2 * time.Second

// clusterStateLine is the line a start step or the lease join (joinDeliveryLease) logs at each try
// of a request the NATS server answered from its cluster's state (bus.ClusterStateAnswer), the
// second group of the start rule (cmd/listener/README.md, "Start"), with what the request was for,
// the copies it asked, the answer's code, the server's text and the wait before the next try
// (logClusterState).
const clusterStateLine = "start refused by the NATS cluster's state; retrying"

// clusterUnavailableWarnFor is how long a start step logs clusterStateLine at WARN, not ERROR,
// for JetStream's two "temporarily unavailable" answers, the ones nats-server gives with HTTP
// status 503: the meta group with no leader (nats.JSErrCodeJetStreamNotAvailable, 10008) and
// insufficient resources (nats.JSErrCodeInsufficientResourcesErr, 10023). A routine JetStream
// leader change answers so until its election ends, and an alarm on that would call an operator
// for nothing; an answer that has lasted this long is logged at ERROR at each try after.
const clusterUnavailableWarnFor = 30 * time.Second

// clusterStateWait is what one start step, or the lease join, carries across the answers the NATS
// server gives it from its cluster's state (logClusterState).
type clusterStateWait struct {
	// next is the wait before the step's next try: durableBindPoll at first, then twice the last
	// up to durableRefusedRetryCap (nextRefusedWait).
	next time.Duration
	// unavailableSince is when the step got its first "temporarily unavailable" answer
	// (clusterUnavailableWarnFor), zero until it has.
	unavailableSince time.Time
}

// logClusterState logs clusterStateLine for err, a request of a start step or of the lease join
// (joinDeliveryLease) the NATS server answered from its cluster's state with the JetStream API
// error code code (bus.ClusterStateAnswer): what names what the step opens (the stream, a bucket
// or a store's buckets), replicas the copies the deployment asks for (bus.ReplicasEnvVar). The
// line is an ERROR, but a WARN for a "temporarily unavailable" answer while the step's first such
// answer came less than clusterUnavailableWarnFor ago. It returns wait.next, the wait before the
// step's next try, which the line names as retry_in, and doubles wait.next up to
// durableRefusedRetryCap for the step's next such answer (nextRefusedWait), as a refused
// durable's retries wait. It waits nothing itself: a start step sleeps the wait out
// (waitOutClusterState), and the lease join waits it out until its context ends (waitUnlessDone).
func logClusterState(logger *logging.Logger, wait *clusterStateWait, what slog.Attr, replicas, code int, err error) time.Duration {
	level := slog.LevelError
	if code == int(nats.JSErrCodeJetStreamNotAvailable) || code == int(nats.JSErrCodeInsufficientResourcesErr) {
		if wait.unavailableSince.IsZero() {
			wait.unavailableSince = time.Now()
		}
		if time.Since(wait.unavailableSince) < clusterUnavailableWarnFor {
			level = slog.LevelWarn
		}
	}
	logger.LogEvent(level, clusterStateLine,
		what,
		slog.String("setting", fmt.Sprintf("%s=%d", bus.ReplicasEnvVar, replicas)),
		slog.Int("code", code),
		slog.String("error", err.Error()),
		slog.String("retry_in", wait.next.String()),
	)
	next := wait.next
	wait.next = nextRefusedWait(wait.next)
	return next
}

// waitOutClusterState is logClusterState for a step of the start: it logs the line and sleeps
// the wait logClusterState returns before the step's next try.
func waitOutClusterState(logger *logging.Logger, wait *clusterStateWait, what slog.Attr, replicas, code int, err error) {
	time.Sleep(logClusterState(logger, wait, what, replicas, code, err))
}

// The ERRORs the start logs at each try of a step the NATS grant refused (waitOutGrantRefusal),
// each "<what the step opens> refused by the NATS grant; retrying", as a refused durable's
// (durableGrantRefusedLine) and the spent-proof bucket's (spentProofBucketRefusedLine) are.
const (
	// storeBucketRefusedLine is a store's bucket (openStartStore), with the bucket and the grant.
	storeBucketRefusedLine = "KV bucket refused by the NATS grant; retrying"
	// roleLaneRefusedLine is the role lane's subscription, refused in both of its queue groups
	// (bus.ConnectRoleLane).
	roleLaneRefusedLine = "role lane refused by the NATS grant; retrying"
	// relayRepliesRefusedLine is the session relay's reply subscription
	// (bus.SessionConsumers.SubscribeReplies, startSessionRelays).
	relayRepliesRefusedLine = "session relay reply subscription refused by the NATS grant; retrying"
	// relaySocketsRefusedLine is an edge listener's relay socket connection (relaySockets).
	relaySocketsRefusedLine = "relay socket connection refused by the NATS grant; retrying"
	// streamRefusedLine is the notification stream's read or create (connectListener,
	// bus.ConnectOwningStream), with the stream and the grant.
	streamRefusedLine = "notification stream refused by the NATS grant; retrying"
)

// logGrantRefusal logs line at ERROR for err, a request of a start step or of the lease join
// (joinDeliveryLease) the NATS grant refused: what names what the step opens, then the refused
// subject (refusedSubject), the server's text and *wait, the wait before the next try, as
// retry_in. It returns *wait and doubles *wait up to durableRefusedRetryCap for the step's next
// refusal (nextRefusedWait), as a refused durable's retries wait; a step of the start starts *wait
// at durableBindPoll, and the join of the listener state bucket at twice that, since the start's
// own ERROR named durableBindPoll as the wait before the join's first try. Every pod of the machine
// connects as the same NATS user and would be refused alike, and an exit would send each into a
// crash loop whose backoff delays the recovery by minutes once the grant is fixed. It waits
// nothing itself: a start step sleeps the wait out (waitOutGrantRefusal), and the lease join waits
// it out until its context ends (waitUnlessDone).
func logGrantRefusal(logger *logging.Logger, wait *time.Duration, line string, err error, what ...slog.Attr) time.Duration {
	logger.Error(line, append(what,
		slog.String("subject", refusedSubject(err)),
		slog.String("error", err.Error()),
		slog.String("retry_in", wait.String()),
	)...)
	next := *wait
	*wait = nextRefusedWait(*wait)
	return next
}

// waitOutGrantRefusal is logGrantRefusal for a step of the start: it logs line and sleeps the wait
// logGrantRefusal returns before the step's next try.
func waitOutGrantRefusal(logger *logging.Logger, wait *time.Duration, line string, err error, what ...slog.Attr) {
	time.Sleep(logGrantRefusal(logger, wait, line, err, what...))
}

// listenerGrant names the grant that gives the listener's NATS user its KV buckets and its
// notification stream, for the lines that report a bucket or the stream the grant refuses: the
// start's bucket and stream lines of every listener but an edge one (grantName), and the listener
// state bucket's lines, which an edge listener never logs, since it opens no lease.
const listenerGrant = "infra/core ListenerPermissions"

// edgeListenerGrant names an edge listener's grant (natsgrant.Edge) in the start's bucket and
// stream refusal lines (grantName): that grant creates and raises no bucket and creates no stream.
const edgeListenerGrant = "infra/core EdgePermissions"

// grantName is the grant the bucket and stream refusal lines of the listener's start name:
// edgeListenerGrant for an edge listener (edge set), listenerGrant for every other.
func grantName(edge *edgeGrant) string {
	if edge != nil {
		return edgeListenerGrant
	}
	return listenerGrant
}

// natsConnectionError reports whether err is a failure to reach NATS, or to get an answer from it,
// that a later attempt can get past: no server answered the dial or closed it during the
// handshake, the server ended the handshake for a CONNECT it did not take within its auth_timeout
// (errAuthenticationTimeout), a request ran out of time, found no responder or was lost with its
// connection (bus.ErrRequestLostWithConnection), the connection closed or is reconnecting, or
// JetStream answered that it is not available yet. Any other error, a credential or configuration
// the connect refuses before it dials among them, is not one. Each start step asks
// bus.ClusterStateAnswer first, so JetStream's "temporarily unavailable" there is waited out as
// the cluster's state it is (cmd/listener/README.md, "Start").
func natsConnectionError(err error) bool {
	for _, transient := range []error{
		io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, context.DeadlineExceeded,
		nats.ErrNoServers, nats.ErrTimeout, nats.ErrNoResponders, nats.ErrNoStreamResponse,
		nats.ErrConnectionClosed, nats.ErrDisconnected, nats.ErrConnectionReconnecting, nats.ErrStaleConnection,
		bus.ErrRequestLostWithConnection, errAuthenticationTimeout,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	if netErr := net.Error(nil); errors.As(err, &netErr) {
		return true
	}
	var jsErr nats.JetStreamError
	if errors.As(err, &jsErr) {
		api := jsErr.APIError()
		return api != nil && api.Code == http.StatusServiceUnavailable
	}
	return false
}

// errAuthenticationTimeout is the server ending a connect whose CONNECT it did not take within its
// auth_timeout, as the connect's handshake meets it: nats-server sends -ERR 'Authentication
// Timeout' and closes the connection (server/client.go authTimeout), and nats.go returns the
// protocol error it read in place of the handshake's PONG, "nats: Authentication Timeout", whose Is
// method matches a target by its own text lowered (natsProtoErr.Is in nats.go v1.55.0), so it
// matches this error, and nats.go names no error of its own for it. A slow link or a loaded server
// makes it, no setting of the listener's does, and the next dial gets past it.
var errAuthenticationTimeout = errors.New("nats: authentication timeout")

// connectListener connects to NATS and reconciles the stream the listener owns
// (bus.ConnectOwningStream), as instance, at the deployment's replica count and byte limit and
// with the buckets it keeps (listenerStreamOptions), by the start rule (cmd/listener/README.md,
// "Start"). Each try that fails is judged by its own error, in this order: a connect that waited
// out refusals of its credential and then ended on another error is judged by that error alone
// (bus.LastTryError), and the earlier refusal it names beside it stays in the error the lines
// log, so a setting alone that decides the last error ends the start though the grant refused
// the read before it.
//   - a credential the server still refuses at the try's end (startCredentialRefused) ends the
//     start. It is the listener's own configuration, and a restarted pod reads its credential
//     again, but each try's connect has first waited it out under start's bound
//     (connectListenerBus, natsStart.option, bus.WithCredentialWait), since a hub that reloads a
//     new nkey refuses it until its servers have reloaded; /healthz answers 503 naming the refusal
//     meanwhile (natsStart.healthz), and the last line names the credential (startFailureLine).
//     It is asked before every case that tries again, since an edge listener's read wraps the
//     refusal beside the read's own error, a timeout or a closed connection, which would read as
//     a NATS that did not answer;
//   - a stream the server cannot take in its cluster's present state is tried again after
//     waitOutClusterState;
//   - on an edge listener, a stream the server does not hold (errEdgeStreamMissing) is tried
//     again after the wait a grant's refusal makes, each try logging edgeStreamMissingLine at
//     ERROR, until a shared listener or Dispatch, whose grants may, creates it;
//   - a read or create of the stream the NATS grant refuses (bus.ErrPublishDenied, which the
//     stream's ensure returns as the server's refusal arrives, or an edge listener's read that met
//     the server's permissions violation, nats.ErrPermissionViolation) is tried again after
//     waitOutGrantRefusal, each try logging streamRefusedLine at ERROR with the refused subject and
//     the server's text, for as long as the refusal lasts: the connect got in
//     (natsStart.admitted), and the grant alone can lift it;
//   - a server that refuses the connection itself for a connection limit, its own or the
//     account's (connectionLimitRefusal), is tried again after the same wait, each try logging
//     connectionLimitLine at ERROR;
//   - a NATS that cannot be reached or did not answer (natsConnectionError, which reads
//     JetStream's 503 too) is tried again every startRetry for as long as it takes, so a listener
//     started before its NATS waits for it rather than exiting;
//   - a stream the account's JetStream limits or the server's own failure refuses
//     (bus.AccountStateAnswer) is tried again after the same wait as a grant's refusal, each try
//     logging streamAccountStateLine at ERROR with the answer's code and the server's text;
//   - any other failure ends the start with that same last line: a setting the connect refuses
//     before it dials, a server that does not take an nkey (nats.ErrNkeysNotSupported), and a
//     stream whose byte limit or copies a setting chose and the account has no room or tier for
//     (JetStream codes 10122, 10113 and 10120) among them.
//
// What is tried again is lifted by the cluster, the grant, another program or the account's or
// the server's state, and no setting of the listener changes it, while every pod of the machine
// connects as one NATS user and would be refused alike, so an exit would only send each pod into
// a crash loop. The pod is live and not ready meanwhile.
//
// An edge listener (edge) owns no stream (connectListenerBus): its connect follows the same rule,
// and its start reads the stream in place of the reconcile, inside the same credential wait,
// which waits out the grant's refusal of the read too until start's bound, since its grant may
// not create the stream and every reply it asks for comes under its own inbox prefix.
func connectListener(cfg config.Service, edge *edgeGrant, instance string, logger *logging.Logger, start *natsStart) *bus.Client {
	clusterWait, refusedWait := clusterStateWait{next: durableBindPoll}, durableBindPoll
	missingWait, stateWait := durableBindPoll, durableBindPoll
	for {
		client, err := connectListenerBus(cfg, edge, instance, start)
		if err == nil {
			return client
		}
		if startCredentialRefused(err) {
			endStartOnNATS(err, start)
		}
		last := bus.LastTryError(err)
		if code, ok := bus.ClusterStateAnswer(last); ok {
			waitOutClusterState(logger, &clusterWait, slog.String("stream", bus.Stream), cfg.NATSReplicas, code, err)
			continue
		}
		if errors.Is(last, errEdgeStreamMissing) {
			start.admitted()
			logger.Error(edgeStreamMissingLine, slog.String("stream", bus.Stream), slog.String("error", err.Error()), slog.String("retry_in", missingWait.String()))
			time.Sleep(missingWait)
			missingWait = nextRefusedWait(missingWait)
			continue
		}
		if errors.Is(last, bus.ErrPublishDenied) || errors.Is(last, nats.ErrPermissionViolation) {
			start.admitted()
			waitOutGrantRefusal(logger, &refusedWait, streamRefusedLine, err, slog.String("stream", bus.Stream), slog.String("grant", grantName(edge)))
			continue
		}
		if connectionLimitRefusal(last) {
			logger.Error(connectionLimitLine, slog.String("error", err.Error()), slog.String("retry_in", refusedWait.String()))
			time.Sleep(refusedWait)
			refusedWait = nextRefusedWait(refusedWait)
			continue
		}
		if natsConnectionError(last) {
			logger.Warn("NATS connect failed; retrying", slog.String("retry_in", startRetry.String()), slog.String("error", err.Error()))
			time.Sleep(startRetry)
			continue
		}
		if code, ok := bus.AccountStateAnswer(last, false); ok {
			start.admitted()
			waitOutAccountState(logger, &stateWait, streamAccountStateLine, code, err, slog.String("stream", bus.Stream))
			continue
		}
		// The line keeps the words it had before it named the credential, which a reader matches
		// (the natslive edge test reads "connect to NATS and reconcile the ... stream: "), and
		// names the credential the hub refused, an nkey user by its public key, which is what an
		// operator looks for in the hub's authorization; err names the step.
		endStartOnNATS(err, start)
	}
}

// startCredentialRefused reports whether err, the error a try of the main connect ended with
// (connectListenerBus), is the server refusing the listener's credential at that try, which ends
// the start (connectListener): the connect's last try refused (bus.FinalTryRefused), or an edge
// listener's read of the stream at that try ending on a connection whose last error is the
// refusal, which the read's error wraps beside the read's own (edgeStreamPresent,
// edgeStreamReadFailed). Both read err as bus.FinalTryRefused reads it, following only the first
// error of an error that wraps several, so a refusal an earlier try met, which a start that waited
// it out names after its last try's error (bus.WithCredentialWait), is not one, and that last
// error picks connectListener's case; a credential wait that ended on a refusal of the dial or of
// the read is one.
func startCredentialRefused(err error) bool {
	if bus.FinalTryRefused(err) {
		return true
	}
	for err != nil {
		if read, ok := err.(edgeStreamReadFailed); ok {
			return bus.AuthorizationRefused(read.err)
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

// connectionLimitLine is the line a main connect the server refused for a connection limit logs at
// each try (connectListener).
const connectionLimitLine = "NATS connect refused by a connection limit; retrying"

// connectionLimitRefusal reports whether err is a server refusing a connection for a connection
// limit: its own (errServerConnectionLimit, as the connect's handshake meets it, or
// nats.ErrMaxConnectionsExceeded, as nats.go names it when the refusal reaches a connection's read
// loop after its handshake) or the account's (nats.ErrMaxAccountConnectionsExceeded). Either is
// the server's state, which a connection that closes frees, and never the listener's
// configuration (connectListener).
func connectionLimitRefusal(err error) bool {
	return errors.Is(err, errServerConnectionLimit) || errors.Is(err, nats.ErrMaxConnectionsExceeded) ||
		errors.Is(err, nats.ErrMaxAccountConnectionsExceeded)
}

// errServerConnectionLimit is the server's own connection limit as the connect's handshake meets
// it: nats-server sends -ERR 'maximum connections exceeded' (its ErrTooManyConnections,
// server/client.go maxConnExceeded) in place of the PONG the handshake waits for, and nats.go
// returns that as its protocol error, "nats: maximum connections exceeded", whose Is method matches
// a target by its own text lowered (natsProtoErr.Is in nats.go v1.55.0). So the refusal matches
// this error and never nats.ErrMaxConnectionsExceeded, whose text is "nats: server maximum
// connections exceeded" and which nats.go sets only when the refusal reaches a connection's read
// loop after its handshake (processErr). The account's limit reaches the handshake in
// nats.ErrMaxAccountConnectionsExceeded's own words, so that error matches it.
var errServerConnectionLimit = errors.New("nats: maximum connections exceeded")

// endStartOnNATS ends a start whose main connect failed with err: it exits with
// startFailureLine's line. A connect begun once the start's bound had passed waits nothing and
// tells the start of no refusal (natsStart.option), so a refusal of the credential err carries
// (bus.AuthorizationRefused) is recorded first (natsStart.refused), and the line names the
// credential all the same.
func endStartOnNATS(err error, start *natsStart) {
	if bus.AuthorizationRefused(err) {
		start.refused(err)
	}
	log.Fatal(startFailureLine(err, start, os.LookupEnv))
}

// The ERRORs the start logs at each try of a step that the account's JetStream limits or the
// server's own failure refused (bus.AccountStateAnswer, waitOutAccountState), each "<what the step
// opens> refused by the NATS account's limits or the server's state; retrying".
const (
	// streamAccountStateLine is the notification stream's create or update (connectListener),
	// with the stream.
	streamAccountStateLine = "notification stream refused by the NATS account's limits or the server's state; retrying"
	// storeBucketAccountStateLine is a store's buckets (openStartStore), with the buckets.
	storeBucketAccountStateLine = "KV bucket refused by the NATS account's limits or the server's state; retrying"
	// spentProofBucketAccountStateLine is the spent-proof bucket (openSpentProofBucket), with the
	// bucket.
	spentProofBucketAccountStateLine = "spent-proof bucket refused by the NATS account's limits or the server's state; retrying"
)

// waitOutAccountState logs line at ERROR for err, a start step's request that the account's
// JetStream limits or the server's own failure refused with the JetStream API error code code
// (bus.AccountStateAnswer): what names what the step opens, then the code, the server's text and
// the wait before the next try. It then sleeps *wait, and doubles *wait up to
// durableRefusedRetryCap for the step's next such refusal (nextRefusedWait), as a grant's refusal
// waits (waitOutGrantRefusal); a step starts *wait at durableBindPoll. An operator who raises the
// account's limit or frees what it counts, or the server's own recovery, lifts the refusal with
// no change to the listener, and every pod of the machine shares the account and would be refused
// alike.
func waitOutAccountState(logger *logging.Logger, wait *time.Duration, line string, code int, err error, what ...slog.Attr) {
	logger.Error(line, append(what,
		slog.Int("code", code),
		slog.String("error", err.Error()),
		slog.String("retry_in", wait.String()),
	)...)
	time.Sleep(*wait)
	*wait = nextRefusedWait(*wait)
}

// openStartStore opens one of the listener's stores on client's connection with open, which opens
// buckets at replicas copies, until it succeeds, by the start rule (cmd/listener/README.md,
// "Start"). An open the server cannot take in its cluster's present state is tried again after
// waitOutClusterState, and one that fails for want of NATS (natsConnectionError) every startRetry.
// An open the NATS user's grant refuses is tried again after waitOutGrantRefusal, which logs
// storeBucketRefusedLine naming the bucket and grant, the name of that user's grant (grantName):
// nats-server drops a refused request and reports the refusal only as the connection's last
// error, so the open runs out of time, and a last error that changed across the open to a refusal
// of one of buckets' subjects is that refusal (bucketRefusal). An open the account's JetStream
// limits or the server's own failure refuses (bus.AccountStateAnswer, where a bucket's byte limit
// is the listener's code and no setting) is tried again after waitOutAccountState, which logs
// storeBucketAccountStateLine naming the buckets, the code and the server's text.
//
// Any other failure ends the process after one ERROR naming it: the server's refusal of the open
// as it was asked, a configuration the server finds invalid (nats.JSStreamInvalidConfig, which an
// ENVOY_SESSION_MAX_AGE below the session bucket's two-minute duplicate window draws, at
// bus.SetKeyValueMaxAge) or another JetStream answer to the request itself, and an error the open
// makes before it asks, such as a bucket configuration nats.go refuses. Each is the listener's own
// setting or code, which no try gets past.
func openStartStore[T any](client *bus.Client, logger *logging.Logger, grant, what string, buckets []string, replicas int, open func(*nats.Conn) (T, error)) T {
	clusterWait, refusedWait, stateWait := clusterStateWait{next: durableBindPoll}, durableBindPoll, durableBindPoll
	for {
		conn := client.Conn
		before := conn.LastError()
		store, err := open(conn)
		if err == nil {
			return store
		}
		if bucket, violation := bucketRefusal(buckets, err, before, conn.LastError()); violation != nil {
			waitOutGrantRefusal(logger, &refusedWait, storeBucketRefusedLine, violation, slog.String("bucket", bucket), slog.String("grant", grant))
			continue
		}
		if code, ok := bus.ClusterStateAnswer(err); ok {
			waitOutClusterState(logger, &clusterWait, slog.String("buckets", strings.Join(buckets, ",")), replicas, code, err)
			continue
		}
		if natsConnectionError(err) {
			logger.Warn(what+" open failed; retrying",
				slog.String("buckets", strings.Join(buckets, ",")),
				slog.String("retry_in", startRetry.String()),
				slog.String("error", err.Error()),
			)
			time.Sleep(startRetry)
			continue
		}
		if code, ok := bus.AccountStateAnswer(err, true); ok {
			waitOutAccountState(logger, &stateWait, storeBucketAccountStateLine, code, err, slog.String("buckets", strings.Join(buckets, ",")))
			continue
		}
		logger.Error(what+" open failed", slog.String("buckets", strings.Join(buckets, ",")), slog.String("error", err.Error()))
		conn.Close()
		os.Exit(1)
	}
}

// spentProofBucketRefusedLine is the ERROR the start logs at each try of the spent-proof bucket's
// open that the NATS grant refused (openSpentProofBucket), with the refused subject and the
// server's text.
const spentProofBucketRefusedLine = "spent-proof bucket refused by the NATS grant; retrying"

// openSpentProofBucket opens the spent-proof bucket on client's connection at replicas copies, and
// on a listener that takes broker tokens builds the credential's verifier over it (tokens,
// brokerTokens.open), and returns once it is open. Every listener opens it, nil tokens, a listener
// that takes no broker token, among them, so an edge listener, whose grant creates no bucket
// (natsgrant.Edge), finds the one a full listener of this build made. It follows the start rule
// (cmd/listener/README.md, "Start"): the start waits here, so /healthz answers 503 "starting",
// /livez 204 and /v1 stays unopened, and every broker-token presentation is answered 503 by the
// closed /v1 gate before its credential is read (v1API: "service starting", or "refilling" to the
// two list reads), so no token is accepted without the check that refuses a proof another replica
// already spent.
//
// An open the server cannot take in its cluster's present state is tried again after
// waitOutClusterState, and one that fails for want of NATS (natsConnectionError) every startRetry
// with a WARN. An open the NATS grant refuses (bus.ErrPublishDenied, which bus.EnsureKeyValue
// returns as the server's refusal arrives) is tried again after waitOutGrantRefusal, each try
// logging spentProofBucketRefusedLine at ERROR with grant, the name of the NATS user's grant
// (grantName), the refused subject and the server's text, as a refused durable's tries do
// (durableReceiver.prepare): an edge listener started before any full listener made the bucket
// waits so for one. An open the account's JetStream limits or the server's own failure refuses
// (bus.AccountStateAnswer, where the bucket's byte limit is the listener's code and no setting) is
// tried again after waitOutAccountState, each try logging spentProofBucketAccountStateLine at
// ERROR with the code and the server's text. Any other failure is one no try gets past, an origin
// ciphertoken.New refuses or a JetStream answer to the request itself among them, and ends the
// process after one ERROR naming it.
func openSpentProofBucket(ctx context.Context, client *bus.Client, logger *logging.Logger, grant string, tokens *brokerTokens, replicas int) {
	refusedWait, clusterWait, stateWait := durableBindPoll, clusterStateWait{next: durableBindPoll}, durableBindPoll
	for {
		conn := client.Conn
		err := tokens.open(ctx, conn, replicas)
		if err == nil {
			return
		}
		if code, ok := bus.ClusterStateAnswer(err); ok {
			waitOutClusterState(logger, &clusterWait, slog.String("bucket", bus.DPoPProofBucket), replicas, code, err)
			continue
		}
		code, accountState := bus.AccountStateAnswer(err, true)
		switch {
		case errors.Is(err, bus.ErrPublishDenied):
			waitOutGrantRefusal(logger, &refusedWait, spentProofBucketRefusedLine, err, slog.String("bucket", bus.DPoPProofBucket), slog.String("grant", grant))
		case natsConnectionError(err):
			logger.Warn("spent-proof bucket open failed; retrying",
				slog.String("bucket", bus.DPoPProofBucket),
				slog.String("retry_in", startRetry.String()),
				slog.String("error", err.Error()),
			)
			time.Sleep(startRetry)
		case accountState:
			waitOutAccountState(logger, &stateWait, spentProofBucketAccountStateLine, code, err, slog.String("bucket", bus.DPoPProofBucket))
		default:
			logger.Error("spent-proof bucket open failed", slog.String("bucket", bus.DPoPProofBucket), slog.String("error", err.Error()))
			conn.Close()
			os.Exit(1)
		}
	}
}

// bucketRefusal returns the bucket of buckets whose subjects the server refused a failed open, and
// the refusal: err when it is itself such a refusal, else after, the connection's last error once
// the open failed, when it differs from before, the last error before it. nats.go keeps one last
// error per connection, so a refusal another goroutine causes in between hides this one, and the
// open reads as a connection error that is retried and refused again.
func bucketRefusal(buckets []string, err, before, after error) (string, error) {
	for _, bucket := range buckets {
		if refusesBucket(err, bucket) {
			return bucket, err
		}
		if !sameLastError(before, after) && refusesBucket(after, bucket) {
			return bucket, after
		}
	}
	return "", nil
}

// refusesBucket reports whether err is the server's refusal of a publish to a subject of KV bucket
// bucket: a JetStream API call on its stream KV_<bucket>, or a write to one of its keys,
// $KV.<bucket>.<key>.
func refusesBucket(err error, bucket string) bool {
	if err == nil || !errors.Is(err, nats.ErrPermissionViolation) {
		return false
	}
	match := publishViolation.FindStringSubmatch(err.Error())
	if match == nil {
		return false
	}
	subject, stream := match[1], ".KV_"+bucket
	return strings.HasSuffix(subject, stream) || strings.Contains(subject, stream+".") ||
		strings.HasPrefix(subject, "$KV."+bucket+".")
}

// openDeliveryLease opens the listener state bucket and makes machineID's delivery lease over it
// for this process, instance, and returns it with the connection the bucket is on. The bucket is
// opened on a NATS connection of the lease's own (bus.Dial, as the user and through the proxy the
// environment names), which nats.go reconnects in place for the life of the process: the bus client
// replaces its connection after a close, and a handle on the replaced one would never renew the
// lease again. By the start rule (cmd/listener/README.md, "Start"), an open the server cannot
// take in its cluster's present state is tried again after waitOutClusterState, and an open that
// fails for want of a connection, or for any other cause, every startRetry, so a NATS that answers
// late does not end the start. An open the NATS user's grant refuses (errListenerStateRefused)
// returns no lease, and the connection, after one ERROR naming the bucket, the missing grant and
// the wait before the next try: the listener then delivers without a lease, as the machine's one
// deliverer receiving only while no other subscriber holds the durable, says so in /healthz, and
// tries the bucket again on that connection until a grant lets it open it (joinDeliveryLease). ctx
// is the start's (openListenerState).
func openDeliveryLease(ctx context.Context, urls []string, replicas int, machineID, instance string, logger *logging.Logger) (*lease, *nats.Conn) {
	var conn *nats.Conn
	clusterWait := clusterStateWait{next: durableBindPoll}
	for {
		var err error
		if conn == nil {
			conn, err = bus.Dial("envoy-listener-lease "+instance, urls, os.LookupEnv)
		}
		if err == nil {
			var kv bus.KeyValue
			if kv, err = openListenerState(ctx, conn, replicas, listenerStateRequestWait); err == nil {
				return newLease(kv, machineID, instance, productionLeaseConfig(), time.Now, logger), conn
			}
		}
		if errors.Is(err, errListenerStateRefused) {
			logger.Error("listener lease unavailable: the NATS user lacks the grant for KV bucket "+bus.ListenerStateBucket+"; delivering without a lease",
				slog.String("bucket", bus.ListenerStateBucket),
				slog.String("grant", listenerGrant),
				slog.String("error", err.Error()),
				slog.String("retry_in", durableBindPoll.String()),
			)
			return nil, conn
		}
		if code, ok := bus.ClusterStateAnswer(err); ok {
			waitOutClusterState(logger, &clusterWait, slog.String("bucket", bus.ListenerStateBucket), replicas, code, err)
			continue
		}
		logger.Warn("listener state bucket open failed; retrying",
			slog.String("bucket", bus.ListenerStateBucket),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(startRetry)
	}
}

// listenerStateRefusedLine is the ERROR a listener delivering without a lease logs at each try of
// the listener state bucket its NATS grant refuses again (joinDeliveryLease), with the bucket, the
// grant it lacks, the refused subject, the server's text and the wait before the next try, as the
// start's refused steps log theirs (logGrantRefusal).
const listenerStateRefusedLine = "listener state bucket refused by the NATS grant; delivering without a lease and retrying"

// joinDeliveryLease opens the listener state bucket on conn, the lease connection of a listener
// whose start the NATS grant refused that bucket (openDeliveryLease), and returns machineID's
// delivery lease over it once it opens: a grant applied after the pod started is picked up without
// a restart, as the start rule has it for every dependency that answers late (cmd/listener/README.md,
// "Start"). It tries again after the start's ERROR's wait (durableBindPoll), and each refusal after
// the wait logGrantRefusal returns, which logs listenerStateRefusedLine with the wait before the
// next try, twice durableBindPoll after the first refusal, doubling up to durableRefusedRetryCap;
// an open the cluster's state answers after the wait logClusterState returns, and any other
// failure after startRetry, as the start's open does. The pod delivers without a lease meanwhile.
//
// Every wait between two tries is the loop's waitUnlessDone, which ends when ctx ends, and the
// open itself runs under ctx (openListenerState), so the join returns nil as soon as ctx has
// ended, wherever it is. The shutdown ends ctx and waits for the try the join runs in, within
// stopHTTPGrace (durableReceiver.startJoin, durableReceiver.Stop): a join that slept its retry_in
// out, up to durableRefusedRetryCap, would spend that budget, and a lease it made after waking
// would take the record and renew it until the process exits. An open that succeeds once ctx has
// ended returns nil too, with no lease made over it.
func joinDeliveryLease(ctx context.Context, conn *nats.Conn, replicas int, machineID, instance string, logger *logging.Logger) *lease {
	wait, refusedWait := durableBindPoll, nextRefusedWait(durableBindPoll)
	clusterWait := clusterStateWait{next: durableBindPoll}
	for waitUnlessDone(ctx, wait) == nil {
		kv, err := openListenerState(ctx, conn, replicas, listenerStateRequestWait)
		if err == nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Info("listener state bucket opened: joining the machine's delivery lease", slog.String("bucket", bus.ListenerStateBucket))
			return newLease(kv, machineID, instance, productionLeaseConfig(), time.Now, logger)
		}
		if code, ok := bus.ClusterStateAnswer(err); ok {
			wait = logClusterState(logger, &clusterWait, slog.String("bucket", bus.ListenerStateBucket), replicas, code, err)
			continue
		}
		if errors.Is(err, errListenerStateRefused) {
			wait = logGrantRefusal(logger, &refusedWait, listenerStateRefusedLine, err, slog.String("bucket", bus.ListenerStateBucket), slog.String("grant", listenerGrant))
			continue
		}
		logger.Warn("listener state bucket open failed; retrying",
			slog.String("bucket", bus.ListenerStateBucket),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		wait = startRetry
	}
	return nil
}

// natsReleaseRefusedLine is the ERROR the start logs at each try of the session consumers'
// connection whose server is a nats-server release the listener refuses (natsversion's floor,
// asked as bus.ConnectSessionConsumers opens the connection), with the server's release, the
// floor the listener needs (natsversion.FixedReleases), the refusal's text and the wait before the
// next try (openStartConnection).
const natsReleaseRefusedLine = "nats-server release below the listener's floor; retrying"

// openStartConnection runs open, a step that opens a NATS connection of its own once the stores
// are open (the session consumers' with its relay's reply subscription, an edge listener's relay
// sockets', the role lane's), until it succeeds, by the start rule (cmd/listener/README.md,
// "Start"). Each such connection reads the settings the start's own connect (connectListener) has
// already taken - its servers, its credential, its proxy and the remote-NATS rule - and each
// connect waits out a refusal of the credential under start's bound (natsStart.option, which open
// passes), so only the credential ends the process here:
//   - a refusal under the NATS grant (nats.ErrPermissionViolation) is tried again after
//     waitOutGrantRefusal, which logs refusedLine;
//   - a server whose release the listener refuses (natsversion.RefusedRelease) after the same
//     wait, logging natsReleaseRefusedLine at ERROR with the server's release and the floor;
//   - a failure for want of NATS (natsConnectionError), among them a connection's ten dials
//     (bus.Connect) that no server answered, every startRetry, logging failedLine at WARN;
//   - a credential the server still refuses at the connect's last try once the connect has waited
//     it out (bus.FinalTryRefused) ends the start: endedLine at ERROR names the refusal and the
//     credential (natsStart.stepFailed), and the process exits 1, as connectListener ends it. A
//     connect that got past a refusal and then failed ten tries in a row otherwise ends with the
//     last of those errors, naming the earlier refusal beside it (bus.WithCredentialWait); that
//     last error, not the refusal, picks its case here;
//   - any other failure, a server that refuses the connection itself (an account at its
//     connection limit) among them, after the same wait as a grant's refusal, logging failedLine
//     at ERROR.
//
// /healthz answers 503 "starting", /livez 204, and /v1 stays closed meanwhile; /healthz names the
// refusal instead while a connect waits one out (natsStart.healthz).
func openStartConnection[T any](logger *logging.Logger, start *natsStart, failedLine, endedLine, refusedLine string, open func() (T, error)) T {
	wait := durableBindPoll
	for {
		opened, err := open()
		if err == nil {
			return opened
		}
		var release *natsversion.RefusedRelease
		switch {
		case errors.Is(err, nats.ErrPermissionViolation):
			waitOutGrantRefusal(logger, &wait, refusedLine, err)
		case errors.As(err, &release):
			logger.Error(natsReleaseRefusedLine,
				slog.String("release", release.Release),
				slog.String("floor", natsversion.FixedReleases),
				slog.String("error", err.Error()),
				slog.String("retry_in", wait.String()),
			)
			time.Sleep(wait)
			wait = nextRefusedWait(wait)
		case natsConnectionError(err):
			logger.Warn(failedLine, slog.String("retry_in", startRetry.String()), slog.String("error", err.Error()))
			time.Sleep(startRetry)
		case bus.FinalTryRefused(err):
			// A connect begun once the start's bound had passed waits nothing, and names no
			// refusal to the start on its own: the line names the credential all the same.
			start.refused(err)
			start.stepFailed(logger, endedLine, err, os.LookupEnv)
			os.Exit(1)
		default:
			logger.Error(failedLine, slog.String("error", err.Error()), slog.String("retry_in", wait.String()))
			time.Sleep(wait)
			wait = nextRefusedWait(wait)
		}
	}
}

// deliveryHandlingLimit is the longest one durable message's handling may take while it holds a
// delivery-ledger attempt: its session read, its ledger calls, and, on the role lane, the
// forward's publish and its wait for the stream's acknowledgement (roleForwardTimeout). The
// delivery ledger's take-over age and the unplaceable deferrals' expiry
// (unplaceableDeferralExpiry) count on it.
const deliveryHandlingLimit = 30 * time.Second

// ledgerTakeoverMargin is how much longer than one message's handling (deliveryHandlingLimit) a
// pod waits on another pod's unresolved delivery attempt before it takes the attempt over: the
// other pod may still be settling it until then, and after it, that pod died without resolving it.
const ledgerTakeoverMargin = 5 * time.Second

// ledgerReconnectWait bounds how long the durable lane waits for the delivery ledger's connection
// to come back, to ask the ledger once more, before it NAKs the message for durableRetryDelay.
const ledgerReconnectWait = 5 * time.Second

// ledgerConnected is the delivery handler's ledgerReconnected over conn, the lease's connection,
// the one the listener state bucket is on: it waits, polling, until conn is connected, for at most
// ledgerReconnectWait and while ctx lasts. A listener that started without the bucket keeps that
// connection to open the bucket on later (openDeliveryLease), and its ledger in process asks
// nothing of it until then. An edge listener dials no lease connection (conn nil), and its ledger
// stays in process, so it has none to wait for (nil).
func ledgerConnected(conn *nats.Conn) func(ctx context.Context) bool {
	if conn == nil {
		return nil
	}
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, ledgerReconnectWait)
		defer cancel()
		poll := time.NewTicker(50 * time.Millisecond)
		defer poll.Stop()
		for !conn.IsConnected() {
			select {
			case <-ctx.Done():
				return conn.IsConnected()
			case <-poll.C:
			}
		}
		return true
	}
}

// newDeliveryLedger makes the delivery ledger every lane asks before it forwards a role-lane
// message to a session, reports a refusal of one, or gives an unplaceable delivery up: the
// `delivery.` keys of the listener state bucket, shared by every pod of machineID, under the
// lease's boot (leaseLedger). A listener with no lease (its NATS user lacks the grant for the
// bucket) is the machine's one deliverer and keeps the ledger in process, which a restart forgets,
// until it joins its machine's lease once a grant lets it open the bucket (joinDeliveryLease): the
// ledger is the shared one from then on, but for the pairs it delivered or attempted before, which
// it still answers from its own records for as long as each stops a delivery (dedupe.Joining).
//
// A record is keyed by the envelope's dedupe key, not its event id: fan-out normalization can
// produce envelopes with one event id and distinct dedupe keys. Each forward is recorded as
// attempted before it is sent and as delivered once the stream acknowledges that it stored the
// forward, which is the whole of a forward's outcome (startRoleForward), so no pod of the machine
// forwards that pair again.
func newDeliveryLedger(deliveryLease *lease, machineID string, requestLimit time.Duration) dedupe.Ledger {
	if deliveryLease == nil {
		return dedupe.NewJoining(requestLimit + ledgerTakeoverMargin)
	}
	return leaseLedger(deliveryLease, machineID, requestLimit)
}

// leaseLedger is machineID's shared delivery ledger under l's boot (newDeliveryLedger), so a pod
// that takes the lease over neither forwards again what the pod before it delivered nor reports a
// refusal it already reported, and takes at once an attempt a former lease holder
// (lease.FormerHolder) only made. A former holder that released the lease is alive and its role
// lane is finishing the forward it had in flight, so its attempt is deferred until it has had
// ledgerFormerSettle to record, give up or report that forward, and only a holder taken over by
// force is taken over at once. Another pod's young attempt is otherwise deferred until it is
// requestLimit and ledgerTakeoverMargin old.
func leaseLedger(l *lease, machineID string, requestLimit time.Duration) *dedupe.KV {
	return dedupe.NewKV(l.kv, dedupe.KVConfig{
		Durable:       bus.ListenerDurable(machineID),
		Boot:          l.boot,
		TakeoverAfter: requestLimit + ledgerTakeoverMargin,
		GoneBoot:      l.FormerHolder,
	})
}

// listenerStreamOptions are the options envoy-listener starts the stream with
// (bus.ConnectOwningStream): the deployment's replica count and byte limit, from cfg, and the
// buckets the listener opens at that count (bus.ListenerBuckets: store.Open's interest and role
// buckets, session.OpenSessionRegistry's and cistore.Open's, the list its NATS grant is built
// from), which the client's keeper raises back to it when a concurrent start or an operator
// lowered one (bus.WithKeptBuckets). The CI bucket is named even on a listener with no GitHub
// route, which opens none: the keeper skips a bucket that does not exist. The listener state
// bucket is not named here but added once the lease has opened it (bus.Client.KeepBucket, main),
// so a pod whose NATS user lacks the grant for it is not refused on it every period. main and the
// tests that start the listener's stream both take them from here.
//
// Each connection the client makes declares itself lost two to three seconds after the hub stops
// answering it (bus.WithSilentCutDetection). A hub whose packets stop closes nothing. On nats.go's
// default pings (one every two minutes, two outstanding) the connection reads connected for four
// to six minutes, and the reconnect that finds a hub which answers starts only once it reads lost.
func listenerStreamOptions(cfg config.Service) []bus.ConnectOption {
	return []bus.ConnectOption{
		bus.WithReplicas(cfg.NATSReplicas),
		bus.WithStreamMaxBytes(cfg.StreamMaxBytes),
		bus.WithKeptBuckets(bus.ListenerBuckets()...),
		bus.WithSilentCutDetection(),
	}
}

// registerListGauges registers the two gauges that count a list /v1 answers: envoy_active_sessions
// counts listedSessions, the listing GET /v1/sessions answers, and envoy_active_interests the
// interest copy GET /v1/interests/ answers. served is the pointer openV1Routes publishes. Before
// /v1 opens, and while a list would answer the refilling 503, a gauge writes no sample: a count of
// a copy that cannot be served is no count of the live sessions, and a missing sample is not read
// as zero, which only a complete empty copy reads.
//
// Beside them it registers the three gauges of the role claim copy's repair, which report what
// /healthz's "role_claim_repair" reports (store.Registry.RoleClaimRepair), so an alert reads the
// copy's freshness without a person: envoy_role_claim_repair_completed_timestamp_seconds, the Unix
// time the role claim watcher's last whole read of the role bucket completed, whose age past
// store.RoleClaimRepairBound is the late repair /healthz reports degraded, and
// envoy_role_claim_repair_put_back and envoy_role_claim_repair_took_out, what that read put back
// into the copy and took out of it. Each writes no sample before /v1 opens or before the first
// read completes: a time of zero would read as a repair late by decades, and a count of zero as a
// read that found the copy right. After that each always has one, the copy's view serving or not,
// since a stale timestamp is what the alert is for.
func registerListGauges(met *metrics.Registry, served *atomic.Pointer[listenerDeps]) {
	met.NewOptionalGaugeFunc("envoy_active_sessions", "Number of active sessions", func() (int64, bool) {
		d := served.Load()
		if d == nil {
			return 0, false
		}
		count, err := listedSessionCount(d.registry, d.sessions)
		if err != nil {
			return 0, false
		}
		return int64(count), true
	})
	met.NewOptionalGaugeFunc("envoy_active_interests", "Number of active interest subscriptions", func() (int64, bool) {
		d := served.Load()
		if d == nil {
			return 0, false
		}
		interests, err := d.registry.List()
		if err != nil {
			return 0, false
		}
		return int64(len(interests)), true
	})
	roleClaimRepairGauge := func(name, help string, value func(store.RoleClaimRepairReport) int64) {
		met.NewOptionalGaugeFunc(name, help, func() (int64, bool) {
			d := served.Load()
			if d == nil {
				return 0, false
			}
			report := d.registry.RoleClaimRepair()
			if report.Completed == 0 {
				return 0, false
			}
			return value(report), true
		})
	}
	roleClaimRepairGauge("envoy_role_claim_repair_completed_timestamp_seconds",
		"Unix time the last whole read of the role claims completed, which repairs the listener's copy of them",
		func(report store.RoleClaimRepairReport) int64 { return report.CompletedAt.Unix() })
	roleClaimRepairGauge("envoy_role_claim_repair_put_back",
		"Role claims the last whole read of the role claims put back into the listener's copy",
		func(report store.RoleClaimRepairReport) int64 { return int64(report.PutBack) })
	roleClaimRepairGauge("envoy_role_claim_repair_took_out",
		"Role claims the last whole read of the role claims took out of the listener's copy",
		func(report store.RoleClaimRepairReport) int64 { return int64(report.TookOut) })
}

// inactiveThresholdFor is the threshold the server removes an idle session consumer after, as
// this deployment's own windows derive it (bus.InactiveThresholdFor): main reads it before it
// opens the session-consumer connection and gives it to that connection, so every consumer the
// listener creates carries it (connectSessionDelivery, bus.ConnectSessionConsumers's
// inactiveThreshold).
//
// It must stand above the sweep's worst bound - the session's record leaves the bucket, the
// sweep's next pass deletes the consumer - because a consumer the server removes first takes
// the session's position with it: the resume floor is recorded by the listener's own delete and
// by nothing else, so a session whose consumer the server removed is created again at the
// stream's end and loses every message stored while it was away.
//
// The record's life is the longer of the gone horizon and the bucket's maximum age, since
// either can be what drops the record: the reaper deletes it a pass after the horizon, and the
// bucket itself drops it at that age.
func inactiveThresholdFor(sessions *session.SessionRegistry, reapInterval, sweepInterval time.Duration) time.Duration {
	_, goneHorizon := sessions.Windows()
	return bus.InactiveThresholdFor(max(goneHorizon, sessions.TTL()), reapInterval, sweepInterval)
}

// processStart is when this process started, as near as the program can read it: Go initialises
// package variables before main runs. /metrics serves it as envoy_listener_start_time_seconds, and
// the listener logs it beside its old-form counter (startLegacySubjectsLog), so a reader of two
// readings of one counter can tell a counter a restart reset from one that did not move
// (packages/envoy/scripts/legacy-subjects-gate.sh compares envoy_listener_legacy_subjects_total
// across two readings 72 hours apart, and a listener outside a cluster has no pod to show it).
var processStart = time.Now()

// main runs the listener with its production credential wait, listenerCredentialWait.
func main() { runListener(listenerCredentialWait) }

// runListener is the listener, main's whole run, whose start on NATS waits out a refusal of its
// credential for up to credentialWait (newNATSStart, Phase 5). main passes listenerCredentialWait,
// and only a test passes another value, running its own binary again as the listener
// (TestTheListenerStartEndsAtTheCredentialBound). Every end of the run ends the process, as main's
// did.
func runListener(credentialWait time.Duration) {
	instance, err := listenerInstance()
	if err != nil {
		log.Fatal(err)
	}
	// Go's default logger, which carries internal/bus's lines and the webhooks' plain ones, gets no
	// instance prefix: the deployed CloudWatch text filters (infra/aws/rendering/metricfilters.go)
	// match fields by position after the date and time, and a word there would stop three alarms.
	// The bus's lifecycle lines name the instance in their client name (`envoy-listener <instance>`).

	// Phase 1: Load config (synchronous, fast). With ENVOY_LISTEN_FD, Load inspects the inherited
	// socket and takes its address as cfg's, so the API auth fence below judges the address that
	// serves; Phase 2 adopts it.
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	// An edge listener admits a broker token alone, so it refuses to start beside any other
	// credential, beside Dispatch's principal, beside webhooks, and without the broker-token
	// settings (validateEdgeMode), before any of them is read below.
	if err := validateEdgeMode(cfg.EdgeProjects, os.Environ()); err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID, logging.WithInstance(instance))
	// The first lines of the log belong to the settings a deployment still carries and this
	// listener does not read, before it does any other work, so an operator reading from the top
	// meets them.
	warnIgnoredEnv(logger, "ENVOY_HOST_BRIDGE", "the listener no longer pushes a message to a session's serve port, so it opens no connection this would address")
	warnIgnoredEnv(logger, "ENVOY_SUBSCRIBE_ALL_TOPICS", "the listener no longer holds a live topics subscription, so there is no such subscription to turn on")
	apiToken := os.Getenv("ENVOY_API_TOKEN")
	oidcIssuer, oidcAudience, err := resolveListenerOIDCConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	oidcAccounts, err := resolveListenerOIDCAccounts(os.Getenv, oidcIssuer)
	if err != nil {
		log.Fatal(err)
	}
	googleAudience, googleAccounts, err := resolveListenerGoogleConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	// Dispatch's own principal is read beside the bearers it is proved by, so a value no
	// credential this listener takes could prove refuses the start.
	dispatchPrincipal, err := resolveListenerDispatchPrincipal(os.Getenv, oidcIssuer, oidcAccounts, googleAccounts)
	if err != nil {
		log.Fatal(err)
	}
	// Neither issuer is read here. Each verifier reads its issuer's discovery document in the
	// background from now on (oidc.LazyVerifier.Run: at once, then again 5 to 30 s after each
	// failure, logging a failure at most once a minute) and when a token needs it, the request
	// waiting for it and then for the key set within identityBudget. So a listener started while
	// the cluster's issuer or Google's cannot be read serves everything that does not need that
	// issuer, the shared token,
	// an open /v1, /healthz, /metrics and the webhooks, and answers a token of that issuer 503
	// until it is read (refuseIdentityToken); /healthz's oidc and google fields and the
	// envoy_listener_oidc_issuer_discovered gauge report which issuers it has read
	// (issuer_health.go). A start that read them first exited when either read failed, and a
	// listener of one replica restarted during either outage then served nobody. Each Run ends
	// once its issuer is read; while it is not, it runs until the process ends, holding nothing
	// a shutdown has to stop.
	var apiVerifier *oidc.LazyVerifier
	if oidcIssuer != "" {
		apiVerifier = oidc.NewLazyVerifier(oidcIssuer, oidcAudience)
		go apiVerifier.Run(context.Background())
	}
	// A Google identity token's issuer is Google itself, so only its audience is a setting.
	var googleCallers *googleIdentities
	if googleAudience != "" {
		googleVerifier := oidc.NewLazyVerifier(oidc.GoogleIssuer, googleAudience)
		go googleVerifier.Run(context.Background())
		googleCallers = &googleIdentities{audience: googleAudience, verifier: googleVerifier, accounts: googleAccounts}
	}
	// The broker-token settings are read here, beside the bearers' own, so a
	// half-configured one refuses the start before anything is bound. Its verifier is built
	// later, once JetStream is open: the spent-proof bucket it records a proof in lives there
	// (brokerTokens.open).
	brokerConfig, err := resolveListenerBrokerConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	brokerTokenAuth := newBrokerTokens(brokerConfig)
	apiAuthConfig := listenerAPIAuth{
		token:        apiToken,
		verifier:     apiVerifier,
		accounts:     oidcAccounts,
		google:       googleCallers,
		brokers:      brokerTokenAuth,
		edgeProjects: cfg.EdgeProjects,
		allowedHosts: cfg.AllowedHosts,
		logger:       logger,
	}
	if err := validateListenerAPIAuth(cfg, apiAuthConfig, os.Getenv("ENVOY_API_ALLOW_UNAUTHENTICATED")); err != nil {
		log.Fatal(err)
	}
	logger.Info("listener API auth: " + describeListenerAPIAuth(apiAuthConfig, oidcIssuer))
	logger.Info("listener API hosts: " + describeListenerAPIHosts(apiAuthConfig))
	logger.Info("listener role marks: " + describeListenerDispatchPrincipal(dispatchPrincipal))
	// The operator's limit on the messages each agent session sends, read beside the API auth
	// settings so a value it refuses stops the start before the listener connects to NATS, and
	// stated in one line either way (sender_limit.go).
	senderMessageLimit, err := parseSenderLimit(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	logger.Info(describeSenderLimit(senderMessageLimit))

	// Load webhook config (fast — env var reads only).
	webhookCfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		log.Fatal(err)
	}
	logger.Info(webhook.DescribeBodyBudget(webhookCfg.BodyBudget))
	// The renames a deployment configures belong beside the other settings lines: an operator who
	// reads from the top sees from them alone whether ENVOY_GITHUB_REPOSITORY_RENAMES took, and a
	// value that configures none logs nothing, as every other repository's publishing is unchanged.
	// The line names the subject prefix each pair's copies go out on, not the pair alone, because
	// the former name is written into the subject as the value spells it while a NATS subject is
	// compared with case: an operator who spelled it in a case GitHub never used reads here the
	// subject nothing subscribes to, rather than meeting the silence later (Renames.String).
	if github := webhookCfg.GitHub; github != nil && !github.RepositoryRenames.IsZero() {
		logger.Info("github repository renames: every envelope of the current repository is published a second time under the former name",
			slog.String("renames", github.RepositoryRenames.String()))
	}

	// Phase 2: Serve on the socket the listener inherited (ENVOY_LISTEN_FD) or bind its address,
	// in the main goroutine before any NATS work begins, so /healthz is reachable as soon as Serve
	// starts, whatever the NATS connection's latency.
	var ln net.Listener
	if cfg.ListenSocket != nil {
		ln, err = listenfd.Adopt(*cfg.ListenSocket)
	} else {
		ln, err = net.Listen("tcp", cfg.ListenAddress())
	}
	if err != nil {
		log.Fatal(err)
	}

	// Shared state: deps stays nil until the durable consumer binds (/healthz reads it), v1Deps
	// until /v1 opens (the list gauges read it).
	var deps, v1Deps atomic.Pointer[listenerDeps]

	// Metrics registry — /metrics available during startup (like /healthz).
	met := metrics.New()
	// An edge listener (ENVOY_EDGE_PROJECTS) holds itself to its own NATS grant from its first
	// connection on (edgeGrant): every connection names its inboxes under its machine's prefix, the
	// main client reads the stream and owns none, no delivery lease and no role lane open, no wake
	// report is published, and its delivery handlers keep no role message and publish no exception
	// on a lane the grant leaves out. Every other listener has none (edge nil).
	var edge *edgeGrant
	if len(cfg.EdgeProjects) > 0 {
		grant, err := newEdgeGrant(cfg.MachineID, cfg.EdgeProjects, met)
		if err != nil {
			logger.Error("edge listener grant refused", slog.String("error", err.Error()))
			os.Exit(1)
		}
		edge = grant
	}
	messagesReceived := met.NewCounter("envoy_messages_received_total", "Total messages received by the listener")
	messagesDelivered := met.NewCounter("envoy_messages_delivered_total", "Total message delivery attempts")
	messagesNAKed := met.NewCounter("envoy_messages_naked_total", "Total messages NAK'd for retry")
	// What a box under load does to its sessions, counted where it happens: the records the
	// session reaper took away and the messages they still had stored, the sends this listener
	// took for a session that had stopped answering, and the fan-outs it skipped for one. A box
	// whose sessions keep stalling shows as skips and unresponsive sends long before a record
	// reaches the horizon.
	sessionsReaped := met.NewCounter("envoy_sessions_reaped_total", "Session records deleted at the gone horizon with no clean stop")
	sessionStoredAfterLastSeen := met.NewCounter("envoy_session_messages_stored_after_last_seen_total", "Messages the stream held for a reaped session, stored after its last registration; not a count of unread messages, since the listener keeps no read receipt")
	sendsToUnresponsive := met.NewCounter("envoy_sends_unresponsive_total", "Sends accepted and stored for a recipient past the live window")
	legacySubjects := met.NewCounter(legacySubjectsMetric, "Names a caller gave in their old form, without the project the rename put into them, by kind: an interest a registration named, a role a claim named, a role a publish, the role lane or a holder lookup named (role_read)")
	// The start-time gauge names this listener's machine (ENVOY_MACHINE_ID) in its one label, so a
	// reader holds the page it read to the machine it meant to read: legacy-subjects-gate.sh takes
	// an outside listener's machine from the operator's --listener <machine>=<url>, and a URL
	// serving another listener would otherwise answer for it. Both are this pod's own: the counter
	// counts the names this pod read, and the gauge is this process's start, so the active pod and
	// each standby of one machine serve pages of their own under the same machine label.
	startSample := []metrics.GaugeSample{{Labels: [][2]string{{"machine", cfg.MachineID}}, Value: processStart.Unix()}}
	met.NewGaugeSetFunc(startTimeMetric, "When this listener process started, in Unix seconds, under its machine: every counter on this page counts from that moment", func() []metrics.GaugeSample { return startSample })
	fanoutUnresponsiveSkips := met.NewCounter("envoy_fanout_unresponsive_skipped_total", "Topic fan-outs skipped for a session past the live window; the message is stored for the session either way")
	// The kept role lanes' outcomes, none labelled by role: a deployment sees unboundedly many
	// role tokens, and each would be a series of its own for the process's life.
	roleKeptExpired := met.NewCounter("envoy_role_kept_expired_total", "Kept role copies a walk dropped at the end of their hour, whether or not a session held the role, by who kept them (kept_by: listener or dispatch_outbox) and whether the hour had ended more than an hour before the drop with no walk's claim on the copy (stale)")
	roleDeadLetters := met.NewCounter("envoy_role_dead_letters_total", "Dead letters sent to the sender session of a kept role copy a walk dropped, by result: stored, or refused for good and not sent again")
	roleKeptUndecodable := met.NewCounter("envoy_role_kept_undecodable_total", "Kept role copies a walk ended because no read can decode them")
	deliveryDuration := met.NewHistogram("envoy_delivery_duration_seconds", "Duration of message delivery attempts", metrics.DefaultBuckets)
	// The CI summary loop runs only beside the GitHub webhook route (holderLoops starts it), so
	// only a listener that mounts it carries the gauge the loop sets.
	var ciLegacyHeld *metrics.Gauge
	if webhookCfg.GitHub != nil {
		ciLegacyHeld = met.NewGauge("envoy_ci_legacy_records_held", "CI records a head-gated listener left unsettled that the summary loop's last pass held back")
	}
	registerListGauges(met, &v1Deps)

	// Phase 3: Build HTTP mux.
	mux := http.NewServeMux()

	// /metrics is always reachable — NOT gated by readiness.
	mux.Handle("/metrics", met.Handler())

	// /healthz and /livez are always reachable, before NATS setup as well as after it. start is the
	// listener's start on NATS (Phase 5): /healthz answers 503 naming the last refusal of the
	// credential while the start waits it out, and otherwise 503 "starting" until the listener is
	// ready and its interest copy has completed its first read; /livez answers 204 throughout. Every
	// /healthz answer carries the identity token issuers' fields (issuerHealthFields), which never
	// decide its status (registerProbeRoutes).
	start := newNATSStart(credentialWait)
	registerProbeRoutes(mux, &deps, start, func(health *listenerwire.Health) { issuerHealthFields(apiAuthConfig, health) })

	// GaugeFunc for consumer pending — queries NATS at scrape time

	// The webhook and /v1 routes answer 503 "service starting" until their dependencies are open
	// (the two /v1 list reads the refilling answer, startingGate), each behind its own gate: the
	// webhooks once NATS and, for the GitHub route, the CI store are (Phase 5), /v1 once the
	// interest and session caches are too (Phase 6). Neither waits for the durable consumer, which
	// only /healthz reads. The webhook paths reach their gate bare. /v1 reaches its gate through
	// v1API, which answers the closed gate before apiAuth judges any credential, so a starting
	// listener spends no request's DPoP proof.
	webhookGate, v1Gate := newStartingGate("webhook", logger), newStartingGate("v1", logger)
	hooks := webhookRoutes(webhookCfg, met.NewCounter(webhookBodyRefusedMetric, "Webhook deliveries answered 503 because their body found no room in the listener's webhook body budget within the route's room wait, by route path"))
	for _, hook := range hooks {
		mux.Handle(hook.path, webhookGate)
	}
	// Serve /v1/* on the listener port for local plugin registration.
	apiAuthConfig.requests = newListenerRequests(met)
	registerIssuerGauge(met, apiAuthConfig)
	apiAuthConfig.route = v1Gate.route
	v1Handler := v1API(apiAuthConfig, v1Gate)
	mux.Handle("/v1", v1Handler)
	mux.Handle("/v1/", v1Handler)
	// /readyz says whether /v1 can serve: it is the path a load balancer's health check reads (the
	// edge's), so a starting pod takes no request before its /v1 can serve one, and a pod whose bus
	// is down takes none until it is back (readyzHandler). /healthz stays as it is for the probes
	// that read it.
	mux.HandleFunc("/readyz", readyzHandler(v1Gate, &v1Deps))

	// Phase 4: Start HTTP server on the listener Phase 2 holds.
	server := newHTTPServer(mux)
	fatal := make(chan error, 1)
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			fatal <- err
		}
	}()
	listeningAt := time.Now()
	// The address ln is bound to, whether inherited or bound here, so the line names the port a
	// kernel chose (PORT=0 or a socket a harness opened on port 0).
	logger.Info("envoy-listener listening", slog.String("addr", ln.Addr().String()))

	// Phase 5: Connect to NATS, for as long as NATS takes to answer. The listener and Dispatch's
	// server both own ENVOY_NOTIFICATIONS: this client ensures the stream on this connect and after
	// every reconnect, at the deployment's replica count and byte limit (listenerStreamOptions), and
	// leaves as it is a stream that a newer build stamped with a higher generation
	// (bus.ConnectOwningStream). The start waits out a refusal of its credential at the connect for
	// up to credentialWait (start), as a hub that has yet to reload a new nkey refuses it: /livez
	// answers 204 meanwhile and /healthz 503 naming the refusal. An edge listener owns no stream:
	// its start reads it in place of the reconcile, waits out a refusal of the connect or of that
	// read alike, and tries the read again, live and not ready, while the server holds no stream,
	// refuses the read under the grant past that wait or does not answer it (connectListener,
	// edgeStreamPresent). From here until /v1 serves, each failure of the start ends it, waits out
	// the cluster's state, the grant, the account's or the server's state or the credential, or
	// waits for NATS by the start rule (cmd/listener/README.md, "Start").
	client := connectListener(cfg, edge, instance, logger, start)
	// starting is the context the bucket opens below run under, and it is context.Background() on
	// purpose: do not replace it with a context a signal ends. Each open waits on the cluster until
	// it answers (a bucket's create, a raise's copies, a member applying a create, a stream's
	// leader: bus.EnsureKeyValue), and what bounds one open is the start's hang guard, 36 s per
	// bus.EnsureKeyValue call (bus's startHangGuard, three times the slowest wait measured under
	// load from a raise's answer until its stream had a leader and every copy was current). An open
	// the guard ends failed for want of NATS (natsConnectionError reads the guard's
	// context.DeadlineExceeded), so openSpentProofBucket, openStartStore and openDeliveryLease try
	// it again every startRetry while /healthz answers 503 "starting", and one the server answers
	// from its cluster's state after waitOutClusterState. A signal needs no context here: this
	// process installs no handler for SIGTERM or SIGINT until signal.NotifyContext makes stopping
	// below, so until then the signal's default action ends the process at once, wherever the
	// start is. Making stopping here instead would change that for the whole start, the NATS
	// connect and the cache warm-ups included, into an ordered shutdown of a start not yet serving.
	starting := context.Background()
	// The spent-proof bucket, before anything of /v1 can be served. Every listener opens it,
	// whether or not it takes broker tokens, so an edge listener, whose grant creates no bucket,
	// finds the one a full listener of this build made. The start waits here until the bucket is
	// open, and never exits while NATS does not answer its open, answers it from the cluster's
	// state or refuses it under the grant (openSpentProofBucket): /healthz answers 503 "starting"
	// and /livez 204 meanwhile, /v1 stays unopened, and every broker-token presentation is answered
	// 503 by the closed /v1 gate before its credential is read (v1API: "service starting", or
	// "refilling" to the two list reads), so no token is accepted without the check of its proof,
	// and no spent proof is recorded anywhere this listener's other replicas cannot see it.
	openSpentProofBucket(starting, client, logger, grantName(edge), brokerTokenAuth, cfg.NATSReplicas)
	consumer := bus.ListenerDurable(cfg.MachineID)
	// The durable is checked here, before the cache warm-ups below (up to 30 s each), so a refused
	// one ends the start within the NATS connect: no pod could receive from it, and nothing serves
	// yet, so the start ends at once. A lookup that fails is left to the receive loop, which retries
	// it, a read the NATS grant refuses included: that loop names the refusal at ERROR on each retry
	// and keeps the pod unready while it lasts (durableReceiver.prepare).
	if _, err := listenerDurable(context.Background(), client, consumer); errors.Is(err, errListenerDurableRefused) {
		logger.Error(durableRefusedLine, slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	} else if err != nil {
		logger.Warn("durable consumer check failed; the receive loop retries it", slog.String("consumer", consumer), slog.String("error", err.Error()))
	}
	// The delivery lease makes one pod of this machine id its deliverer and every other a standby
	// (lease.go). It is created here, after the durable check so a refused start opens no bucket,
	// and started once /v1 serves and the shutdown can release it. A pod that opened the bucket
	// has the grant for it, so its keeper raises that bucket to the deployment's copies every
	// period too (bus.Client.KeepBucket): naming it at the connect would have a pod whose grant
	// lacks it, which delivers without a lease, refused on it every period instead. Such a pod
	// keeps the lease's connection and joins the lease once a grant lets it open the bucket
	// (joinDeliveryLease, below); podLease is the lease from whichever of the two, and what reads
	// the lease outside the receive loop reads it there, so it follows the join.
	//
	// An edge listener opens no lease and dials no lease connection: its grant holds no call of the
	// listener state bucket (natsgrant.Edge), so every request of that connection would be refused,
	// and none is ever granted to join on. It delivers as a listener without a lease does, the
	// machine's one deliverer, receiving from the durable only while no other subscriber holds it,
	// with its ledger and last-seen times in process: one edge pod runs per machine id.
	var deliveryLease *lease
	var leaseConn *nats.Conn
	if edge == nil {
		deliveryLease, leaseConn = openDeliveryLease(starting, cfg.NATSURLs, cfg.NATSReplicas, cfg.MachineID, instance, logger)
	} else {
		logger.Info("delivery lease not opened: an edge listener's grant holds no listener state bucket; delivering without a lease",
			slog.String("bucket", bus.ListenerStateBucket))
	}
	var podLease atomic.Pointer[lease]
	if deliveryLease != nil {
		podLease.Store(deliveryLease)
		client.KeepBucket(bus.ListenerStateBucket)
	}
	// CI-summary aggregation state. Its WatchAll cache warms asynchronously like the registries
	// below; the summary loop tolerates an empty cache until it fills. It opens ahead of them, as the
	// one store the webhooks need, and after the durable check, so a refused start opens no bucket.
	//
	// Only the GitHub webhook route records checks in it, and the listeners that receive GitHub
	// webhooks publish every CI settlement, so a listener without the route opens no CI store and
	// runs no summary loop. It must not scan the bucket either: the watch delivers every record as
	// it starts (62 MB in production), and over a relayed link that burst holds up the replies the
	// stores below wait on past their 10 s deadline.
	var ciStore *cistore.Store
	var renames ghrename.Renames
	if webhookCfg.GitHub != nil {
		renames = webhookCfg.GitHub.RepositoryRenames
		verify, err := ciVerifySetting(os.Getenv(ciVerifyEnv))
		if err != nil {
			log.Fatal(err)
		}
		if verify {
			logger.Info(ciVerifyEnv + "=on: a CI settlement is published only after Dispatch's CI reconcile has read the head from GitHub after the listener asked")
		} else {
			logger.Info(ciVerifyEnv + "=off: a CI settlement is published from the webhooks alone, with no GitHub read before it")
		}
		ciStore = openStartStore(client, logger, grantName(edge), "CI store", []string{bus.CIStateBucket}, cfg.NATSReplicas, func(conn *nats.Conn) (*cistore.Store, error) {
			return cistore.Open(starting, conn, logger, cistore.WithReplicas(cfg.NATSReplicas), cistore.WithTTL(7*24*time.Hour), cistore.WithVerify(verify))
		})
	} else {
		logger.Info("CI store not opened: no GitHub webhook route, so this listener records and settles no checks")
	}
	// The one publisher every envelope goes out through: the webhook routes' deliveries and the CI
	// summary loop's settlements alike, so a renamed repository's subscribers of the former name
	// hear both. It is built here, over the open client, because it publishes through it, and it
	// carries the configured pairs, which the start-up line above names; a listener with no GitHub
	// route configures none and publishes exactly what the client publishes.
	publisher := ghrename.Publishing(client, renames, logger)
	openWebhooks(webhookGate, hooks, publisher, ciStore)
	logger.Info("envoy-listener webhooks open (NATS connected)")

	// The registry and its cache watcher log through the listener's own handler, so its warm-up
	// and its reaper cycles are JSON records with this machine's id. Never slog.SetDefault
	// instead: see store.WithLogger for the CloudWatch filters that breaks. A role claim's grace
	// counts from the machine's service start, the delivery lease record's service_since, which
	// every pod reads alike; a listener with no lease takes its own start (store.WithServiceSince),
	// until it joins the lease. Each old-form name a caller gives the registry is counted on
	// legacySubjects (store.WithLegacySubjectCounter).
	registryOptions := []store.OpenOption{store.WithReplicas(cfg.NATSReplicas), store.WithLogger(logger.Slog()), store.WithLegacySubjectCounter(legacySubjects),
		store.WithServiceSince(func() time.Time {
			if l := podLease.Load(); l != nil {
				return l.Status().ServiceSince
			}
			return time.Time{}
		})}
	registry := openStartStore(client, logger, grantName(edge), "interest registry", []string{bus.InterestBucket, bus.RoleClaimBucket}, cfg.NATSReplicas, func(conn *nats.Conn) (*store.Registry, error) {
		return store.Open(starting, conn, registryOptions...)
	})

	// Wait for both of the registry's views before accepting NATS messages or HTTP requests.
	// Without this gate, real events that arrive in the warm-up window get "no matching interests"
	// even when the durable KV entry has subscribers, and the two routes that report a session's
	// roles - GET /v1/sessions and GET /v1/registry/<session_id> - answer a live client the
	// bus-unavailable 503 while the bus is healthy, because the role claim view refuses every read
	// until its watcher has delivered every claim. So the gate waits for that view to be complete,
	// not merely ready: a first scan that its idle wait ended (kvwatch.ScanIdleTimeout) releases
	// readiness, and the watcher's own re-read of the bucket ends this wait.
	//
	// Bounded at 30s: a healthy NATS cluster completes the scan in milliseconds. The wait also
	// ends before that bound with the role claim watcher's terminal error - a first start that
	// failed, a bucket recreated under it, the watcher's own end - or its stop, none of which the
	// watcher completes a scan after.
	// A warm-up that does not complete still serves, fail open: the warn below names the cache and
	// carries the cause, whichever of those it was, the registry keeps answering from whatever it
	// holds, and /v1 opens for the routes that do not need the interest copy. /healthz answers
	// "starting" until that copy's first read completes (interestCopyRead), so the pod is not Ready
	// meanwhile, and then reports the role claim view as degraded and the buckets themselves while
	// NATS recovers.
	cacheReadyCtx, cacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := registry.WaitForCacheReady(cacheReadyCtx); err != nil {
		logger.Warn("interest or role claim cache warm-up did not complete; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	cacheReadyCancel()

	// A session's last-seen time lives in the listener state bucket beside the lease, so every pod
	// answers it alike for a lapsed session; a listener with no lease keeps it in process
	// (session.WithLastSeenBucket) until it joins the lease (session.SessionRegistry.SetLastSeenBucket,
	// joinDeliveryLease below). The registry would answer a departed session's last heartbeat
	// for its own session.LastSeenMemory (2 hours), which this listener does not shorten, but the
	// state bucket drops the seen. record listenerStateMaxAge (20 minutes) after its last write,
	// and with the bucket that ends the answer (internal/session/lastseen.go).
	//
	// The windows every reader of the registry judges a record's age against. Each defaults to
	// the compiled constant (session.LiveWindow, session.GoneHorizon). The live window's is the
	// declaration contracts.SessionLiveWindow, which the Go test
	// TestDAEMONTESTTheSessionTimingRuleHoldsAtTheValuesTheBuildUses (packages/daemon/internal/daemon)
	// weighs to prove that three registration refreshes fit inside it; a deployment
	// that moves one moves what this listener runs with and nothing else. A value that is not a
	// positive duration is refused by name here rather than taken. Dispatch reads the same two
	// windows through the same helper and judges a role route's holder by them, so a deployment
	// gives both processes the same values (envconf.SessionWindows, cmd/dispatch's
	// roleHolderWindows).
	sessionLiveWindow, sessionGoneHorizon, err := envconf.SessionWindows(logger.Slog(), os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	// The two reaper loops below run on periods of their own: what the session reaper decides is
	// what the role claim reaper waits for, but each pass costs something different and a gone
	// holder's role is not freed sooner than the deployment means it to be (the two defaults say
	// why). Both run on the pod holding the delivery lease alone (holderLoops).
	sessionReapInterval, err := envconf.PositiveDuration(logger.Slog(), os.LookupEnv, "ENVOY_SESSION_REAP_INTERVAL", defaultSessionReapInterval)
	if err != nil {
		log.Fatal(err)
	}
	roleClaimReapInterval, err := envconf.PositiveDuration(logger.Slog(), os.LookupEnv, "ENVOY_ROLE_CLAIM_REAP_INTERVAL", defaultRoleClaimReapInterval)
	if err != nil {
		log.Fatal(err)
	}
	// The session consumer sweep's period, the bound on how long a session that ended without
	// deregistering keeps its consumer (startSessionConsumerSweep, session_consumer_sweep.go).
	// It is neither reaper's: what it waits for is the state the session registry answers, and a
	// pass of it frees a slot of the stream's consumer cap rather than taking anything away from
	// a session. Like them it runs on the pod holding the delivery lease alone (holderLoops).
	sessionConsumerSweepInterval, err := envconf.PositiveDuration(logger.Slog(), os.LookupEnv, "ENVOY_SESSION_CONSUMER_SWEEP_INTERVAL", defaultSessionConsumerSweepInterval)
	if err != nil {
		log.Fatal(err)
	}
	sessionOptions := []session.SessionRegistryOption{
		session.WithSessionReplicas(cfg.NATSReplicas),
		session.WithSessionLogger(logger.Slog()),
		session.WithSessionMaxAge(cfg.SessionMaxAge),
		session.WithSessionLiveWindow(sessionLiveWindow),
		session.WithSessionGoneHorizon(sessionGoneHorizon),
	}
	if deliveryLease != nil {
		sessionOptions = append(sessionOptions, session.WithLastSeenBucket(deliveryLease.kv))
	}
	sessions := openStartStore(client, logger, grantName(edge), "session registry", []string{bus.SessionBucket}, cfg.NATSReplicas, func(conn *nats.Conn) (*session.SessionRegistry, error) {
		return session.OpenSessionRegistry(starting, conn, sessionOptions...)
	})

	// The margin between a client's registration refresh and this lifetime can only be checked
	// against the defaults in the source, while the live value is whatever MaxAge the bucket
	// carries once ENVOY_SESSION_MAX_AGE has been applied to it. Say it at boot, with the windows
	// beside it, so an operator can read the values in force: a maximum age below the gone
	// horizon drops a stalled session's record before the horizon can be reached, and the horizon
	// then means nothing.
	logger.Info("session record lifetime resolved; a session record is dropped this long after its last write",
		slog.String("ttl", sessions.TTL().String()),
		slog.String("live_window", sessionLiveWindow.String()),
		slog.String("gone_horizon", sessionGoneHorizon.String()),
		slog.String("reap_interval", sessionReapInterval.String()),
		slog.String("role_claim_reap_interval", roleClaimReapInterval.String()))

	// Wait for the session cache to finish its initial scan before we accept
	// traffic, mirroring the interest-cache gate above. Bounded at 30s: a healthy
	// cluster completes in milliseconds. On timeout we fail open — Put
	// write-through keeps locally-registered sessions visible, while /healthz
	// reports a persistently unavailable registry.
	sessionCacheReadyCtx, sessionCacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := sessions.WaitForCacheReady(sessionCacheReadyCtx); err != nil {
		logger.Warn("session cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	sessionCacheReadyCancel()

	// The Legion agent registry (internal/legionagents) keeps no cache: every read is a read of its
	// bucket, so it joins no warm-up or self-health. A listener that cannot open the bucket (a
	// missing grant is the usual cause) logs one ERROR and serves on, its registry routes answering
	// 503 while it retries the open in the background, with a backoff and at once after a reconnect
	// (legionAgentRegistry). Its reconnect hook runs after the caches' rewatch, and moves its bucket
	// handle onto a connection the bus dialled to replace a closed one; its drain hook ends the retry.
	legionAgents := openLegionAgentRegistry(client.Conn, logger, legionagents.WithReplicas(cfg.NATSReplicas), legionagents.WithLogger(logger.Slog()))

	// The known-session store answers which sessions registered in the last 30 days, whether or
	// not they are registered now; no delivery depends on it, so it never stops a start, and it is
	// opened the way the Legion agent registry above is. A listener whose NATS user may not open
	// the bucket - on a hub whose grant does not carry it - keeps the store, which logs the bucket
	// it could not open once, answers no record and writes none, and retries the open in the
	// background with a backoff and at once after each connection event
	// (session.OpenKnownSessions), so a grant applied after this start is picked up with no
	// restart. This line is the one failure that leaves no store at all, a connection that gives
	// no JetStream context, which answers as an empty store does.
	known, knownErr := session.OpenKnownSessions(
		starting,
		client.Conn,
		session.WithKnownSessionReplicas(cfg.NATSReplicas),
		session.WithKnownSessionLogger(logger.Slog()),
	)
	if knownErr != nil {
		logger.Error("the known-session store was not built; no session's 30-day record is written or read for the life of this listener",
			slog.String("bucket", bus.KnownSessionBucket), slog.String("error", knownErr.Error()))
	}

	// The store's watcher fills the records in the background, so wait for a read that delivered
	// every one of them before serving, as the caches above are waited for (the state
	// session.KnownSessions.Opened reads). Until that read this listener knows no stopped session
	// beyond the live entries: a send to a session with no live entry is the 404 this store exists
	// to stop answering, GET /v1/registry/<id>?include=unresponsive,stopped is that 404 too, and a
	// publish's recipient counts are left out.
	//
	// Bounded at 30s, as its neighbours are, and it fails open: the store answers from whatever the
	// watcher has delivered once a read completes, the counts stay out until Opened says the view
	// is whole, and the warn below says so. Nothing here stops a start. A store whose bucket is
	// not open, and a nil one, wait for nothing: neither has a cache to fill, and a start must not
	// spend this budget on a bucket it may not open.
	knownCacheReadyCtx, knownCacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := known.WaitForCacheReady(knownCacheReadyCtx); err != nil {
		logger.Warn("known-session cache warm-up did not complete; until it does a send to a session with no live entry answers 404 and a publish carries no recipient counts",
			slog.String("bucket", bus.KnownSessionBucket), slog.String("error", err.Error()))
	}
	knownCacheReadyCancel()

	// KV watchers are not bus subscriptions: tie them to the connection, so a disconnect stops each
	// copy following its bucket, every recovered connection reads each bucket again, and the drain
	// ends them before the connection's own.
	caches := listenerCaches(registry, sessions, ciStore)
	hookListenerKVWatchers(client, caches)
	client.AddReconnectHook(legionAgents.reconnect)
	client.AddDrainHook(legionAgents.close)
	// The known-session store's watcher is tied to the connection the same way, outside
	// listenerCaches: the self-health monitor restarts a listener whose cache does not answer, and
	// on a hub without the bucket's grant this one never answers, so a listener there would restart
	// in a loop over a cache no delivery reads. Every hook is safe on a store whose bucket is not
	// open and on no store at all.
	//
	// A failed move is logged and the hook answers nil, for the same reason: a hook's error fails
	// the whole hook run, which the bus runs again on backoff with every other hook, reports
	// /healthz degraded after its repair grace and writes the ERROR a metric filter alarms on
	// (internal/bus/recovery.go) - over a bucket whose grant was rolled back or which was deleted
	// under its watcher, neither of which a delivery reads. What repairs it instead is the store
	// itself: a move that failed is retried in the background on the store's own backoff until
	// one succeeds, the store is stopped, or a later connection event's move makes it
	// (session.KnownSessions.Rewatch). Until a move lands, the store serves its copy for the stale
	// bound and then answers nothing (Opened), so a publish leaves its counts out rather than
	// answer from a copy that stopped following the bucket. A move whose run is over (bus.RunEnded:
	// its context ended, or the connection left CONNECTED) is no failure: this hook logs no WARN
	// for it and the store arms no retry for it, since the run is superseded and the run the next
	// connection event brings moves the store again (bus.errRunSuperseded). The store still counts
	// it a failed move, so the move that succeeds after it logs the store's INFO "back" line
	// (attemptRepair).
	client.AddDisconnectHook(known.Disconnected)
	client.AddReconnectHook(knownSessionMoveHook(known, logger))
	client.AddDrainHook(known.EndWatch)

	ledger := newDeliveryLedger(deliveryLease, cfg.MachineID, deliveryHandlingLimit)

	// The window an agent delivery this listener cannot place is retried within before it is
	// given up with a record (unplaceable_delivery.go). Zero is a value an operator may mean
	// here - retry nothing - so it is read through envconf.Duration and not refused.
	unplaceableRetryWindow := envconf.Duration(logger.Slog(), os.LookupEnv, "ENVOY_UNPLACEABLE_RETRY_WINDOW", defaultUnplaceableRetryWindow)

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Wake reports are published off the paths that raise them, under a lifetime of the
	// listener's own — the shutdown ends every retry still running — and a cap on how many
	// retry at once (wakeReporter). It is built here because the delivery handlers below copy
	// this configuration by value, so every seam of it must be set before they are built. An edge
	// listener reports none: its grant may not publish the wake topic (natsgrant.Edge), and the
	// shared listeners report every wake.
	wakesCtx, stopWakeReports := context.WithCancel(context.Background())
	var wakes *wakeReporter
	if edge == nil {
		wakes = newWakeReporter(wakesCtx, client)
	}

	// Phase 6: Open the role lane and /v1 onto the stores, then start the lease and the durable's
	// receive loop.
	deliveryConfig := listenerDeliveryHandlerConfig{
		client:            client,
		wakes:             wakes,
		edge:              edge,
		forwardRole:       startRoleForward(client),
		registry:          registry,
		sessions:          sessions,
		machineID:         cfg.MachineID,
		ledger:            ledger,
		ledgerReconnected: ledgerConnected(leaseConn),
		logger:            logger,
		messagesReceived:  messagesReceived,
		messagesDelivered: messagesDelivered,
		messagesNAKed:     messagesNAKed,
		deliveryDuration:  deliveryDuration,
		roleKeptExpired:   roleKeptExpired,
		// The walk's other two outcomes beside the drop, and the one publish a walk makes that
		// is not a forward: a dropped copy's dead letter, stored on its sender's agent subject
		// under the message id every listener gives it (endKeptCopyWithDeadLetter).
		roleDeadLetters:     roleDeadLetters,
		roleKeptUndecodable: roleKeptUndecodable,
		publishDeadLetter:   client.PublishStoredAs,
		keepPublish:         client.PublishStoredAck,

		fanoutUnresponsiveSkips: fanoutUnresponsiveSkips,

		unplaceableRetryWindow: unplaceableRetryWindow,
		// One deferral set for the life of the process, shared by construction by every handler
		// built from this one value below - the role lane's and the durable's,
		// which the receive loop keeps across every resubscribe and every take-over of the lease -
		// so no handler opens a second budget beside the first (unplaceableDeferrals,
		// unplaceable_delivery.go).
		deferredUnplaceable: newUnplaceableDeferrals(),
		// One guard per role for the life of the process, held for a whole handover of that
		// role's kept lane, so the handovers the claim route, a keep, a registration that makes a
		// holder live again and the sweep start never walk the lane together, and the sweep can
		// tell a lane this listener is draining already (roleKeepGuards, role_keep.go). Every
		// handler and route built from this one value shares it, as they share the deferral set
		// above.
		roleKeepDrains: newRoleKeepGuards(),
		// The listener's one clock for a kept copy's hour: the keep's answer, and the
		// handovers the claim route, the keep, the registration and the sweep start
		// (role_keep.go).
		now: time.Now,
		// Whether this listener's NATS user holds the session consumers' calls, which a
		// takeover pass needs and Dispatch's user and a host's do not: the one answer every copy
		// of this configuration reads, given its source below once the session delivery is open
		// (consumerGrantAnswer).
		consumerGrant: &consumerGrantAnswer{},
	}
	// The role lane's forwards run off its handler (roleForwarder): the handler resolves each
	// role message's holder and hands the forward over, and the forwards to one holder are
	// published in order while each waits for its outcome on its own. It is wired after every
	// seam of the configuration it copies is set (wireRoleForwarder); the consumer grant's answer,
	// which needs the session delivery opened below, is a holder the copy shares and is filled in
	// there (consumerGrantAnswer).
	deliveryConfig = wireRoleForwarder(deliveryConfig)
	roleForwards := deliveryConfig.roleForwards

	// Every session takes its messages from a session consumer this listener manages on a
	// connection of its own (bus.ConnectSessionConsumers), and its relay needs a reply inbox to
	// answer frames on. There is nothing left to fall back to, so the start waits until both are
	// open, by the start rule (openStartConnection): a listener that served without them would
	// refuse every registration. They open before the role lane, so a nats-server release the
	// listener refuses, which the connection asks of its server as it opens, leaves the pod
	// delivering nothing at all: no role message is forwarded, no frame relayed, and /v1 stays
	// closed. An edge listener's connection names its inboxes under the edge's prefix and reads
	// its buckets by stream message gets (edgeGrant.connectOptions).
	//
	// The threshold the server removes an idle session consumer after is derived from this
	// deployment's own windows first and given to that connection, so every consumer the
	// listener creates carries one that stands above its own sweep's worst bound: the session's
	// record leaving the bucket, then the sweep's next pass. A deployment that keeps session
	// records for longer than the default windows would otherwise have the server remove a
	// consumer the listener's own delete had not reached, and with it the only record of where
	// that session's delivery stopped (inactiveThresholdFor, bus.ConnectSessionConsumers).
	inactiveThreshold := inactiveThresholdFor(sessions, sessionReapInterval, sessionConsumerSweepInterval)
	// Each try opens a connection of its own and subscribes its relay's replies on it, and a try
	// that fails closes it: the connection holds one reply subscription, so a later try's relay,
	// whose inbox is new, needs a new connection. Each try's connect waits out a refusal of the
	// credential under the start's bound (startSessionDelivery, natsStart.option), and a refusal
	// that outlasts it ends the start (openStartConnection).
	var delivery *sessionDelivery
	// The relay hears registrations from the moment /v1 opens, and relays once this task serves
	// its machine's receive term (below). An edge listener's relay takes its receipts under its
	// machine's receipt family (natsgrant.EdgeRelayReplyPrefix), on which the replica serving the
	// session's relay socket publishes them.
	relays := openStartConnection(logger, start, "session consumer connection failed; retrying",
		"session consumer connection failed: every session is delivered through a consumer of its own", relayRepliesRefusedLine,
		func() (*sessionRelays, error) {
			var err error
			if delivery, err = startSessionDelivery(cfg, inactiveThreshold, client, logger, edge, start); err != nil {
				return nil, err
			}
			relays, err := startSessionRelays(delivery, deliveryConfig)
			if err != nil {
				delivery.close()
			}
			return relays, err
		})
	// The answer to whether a takeover pass may run here is that of the session delivery the try
	// that succeeded opened, which remembers a refusal for its episode
	// (sessionDelivery.holdsConsumerGrant). It is set before /v1 opens and before any receive term
	// starts the sweep, the two callers (passRecords); the configuration and each copy made of it
	// already hold the answer this sets.
	deliveryConfig.consumerGrant.set(delivery.holdsConsumerGrant)
	// A relay of this pod restarts a session's consumer at its ack floor only once a former holder
	// that released the lease has had its settle bound (lease.FormerSettleWait): until then that pod
	// may still be acking a frame the session answered, which this one would otherwise publish
	// again. A pod without a lease waits for nothing until it joins one.
	relays.formerSettle = func() time.Duration {
		if l := podLease.Load(); l != nil {
			return l.FormerSettleWait()
		}
		return 0
	}
	// An edge listener serves its sessions' relay sockets on a NATS connection of the sockets'
	// own, as the main client's but for its name, whose slow consumers stay off the main
	// connection's last error (confirmPublished reads it), opened by the start rule
	// (openStartConnection) as the session consumers' is; every other listener serves none.
	var sockets *relaySockets
	if edge != nil {
		socketsClient := openStartConnection(logger, start, "relay socket connection failed; retrying", "relay socket connection failed", relaySocketsRefusedLine, func() (*bus.Client, error) {
			return bus.Connect(cfg.NATSURLs, append(edge.connectOptions(), bus.WithName("envoy-relay-sockets"), bus.WithSilentCutDetection(), start.option())...)
		})
		sockets = newRelaySockets(socketsClient, brokerTokenAuth, logger)
	}
	// An edge listener reads the repositories its projects' issues resolve to from the broker's
	// GET /v1/project-repositories, with its own broker account, and its relay holds every GitHub
	// frame to them. It is set before the relay relays anything, which it does only once this task
	// serves its machine's receive term (below).
	var repositories *edgeRepositories
	if len(cfg.EdgeProjects) > 0 {
		repositories = newEdgeRepositories(brokerConfig.BrokerURL, brokerConfig.account(), cfg.EdgeProjects, newEdgeWithheldCounter(met), logger)
		relays.repositories = repositories
		sockets.repositories = repositories
		// The first read of every project's answer is made before /v1 serves, so a relay socket
		// this replica opens judges a GitHub frame by a fresh answer from its first frame, rather
		// than waiting for the refresh loop, which starts only with this task's receive term
		// (holderLoops). A read that fails keeps no answer, each read is bounded
		// (edgeRepositoryReadTimeout), and a socket answers a GitHub frame it cannot judge nak
		// (relaySocket.withhold), so the relay sends it again.
		repositories.refresh(context.Background())
	}

	// The role lane (startRoleLane), whose connection opens by the start rule too, its connect
	// waiting out a refusal of the credential under the start's bound; an edge listener opens none.
	lane := startRoleLane(cfg, edge, coreNATSDeliveryHandler(deliveryConfig), roleForwards, met, logger, start)
	met.NewGaugeFunc("envoy_role_forwards_outstanding", "Role forwards queued behind their holder or waiting for their outcome", func() int64 {
		return int64(roleForwards.outstanding())
	})

	// The parts a shutdown ends, gathered once: every end of a start that serves /v1 passes them
	// to shutdownListener.
	parts := listenerParts{
		server:    server,
		client:    client,
		lane:      lane,
		delivery:  delivery,
		relays:    relays,
		sockets:   sockets,
		caches:    caches,
		known:     known,
		stopWakes: stopWakeReports,
	}

	// From here a SIGTERM or SIGINT is an ordered shutdown: /v1 serves from the next step, and the
	// lease is released on the way out. Before here, during the NATS connect and the cache
	// warm-ups, a signal ends the process at once, as it always has.
	stopping, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The live window the session registry runs with: what the role claim reaper weighs a
	// standing claim's renewal against, read from the registry itself so the two can never be
	// configured apart. The role routes read it from that same registry when they weigh a claim
	// (roleSetHandler, through session.SessionRegistry.Windows).
	sessionLive, _ := sessions.Windows()

	// The loops only the deliverer runs start when this pod starts receiving the durable and stop
	// when it stops: it lost or released the lease. A standby runs none of them. Each loop that
	// lists a bucket does so through watchers whose consumers nats.go creates and deletes, so the
	// end of receiving waits for a pass in flight, and the shutdown ends receiving before it
	// drains NATS.
	ciDebounce := envconf.Duration(logger.Slog(), os.LookupEnv, "ENVOY_CI_DEBOUNCE", 5*time.Second)
	// summaryDone closes once the last term's CI summary loop has detached from the CI store. A
	// store feeds one summary loop at a time, and a loop's pass in flight can outlast the end of
	// its term's wait (holderLoopsStopWait), so each term's loop starts only after it.
	summaryDone := make(chan struct{})
	close(summaryDone)
	// ready is the deps the routes and the holder-only loops below read. It is built further
	// down, once every store and the receiver exist, and assigned before the receiver starts, so
	// a term's loops - which begin with that receiver - never read it unset.
	var ready *listenerDeps
	holderLoops := func(ctx context.Context) []<-chan struct{} {
		// Relay ownership is no loop of this term's: the receive term itself sets and clears it
		// (durableReceiver.begin and durableReceiver.end, sessionRelays.setOwned), so among the
		// pods of one machine id only the one receiving relays its sessions' consumers.
		//
		// The interest reaper cross-references envoy_sessions with envoy_interests (permanent) to
		// prune orphaned interests from gone sessions, each kept until more than
		// store.InterestReapGrace plus store.InterestRefreshAge (15 minutes) has passed since its
		// last write; the role-claim reaper ends the claims of holders whose sessions are gone.
		//
		// The live window is what the role claim reaper weighs a claim against twice, because the
		// two grants it makes are both that long. A restored claim's holder gets one live window
		// from this listener's start to register again, not one bucket maximum age: the window is
		// what makes a session live to every other reader, and with a maximum age raised past the
		// gone horizon the age would hold a dead holder's claim for an hour after the reaper had
		// already found it gone. A claim the hand-back marked takes none of that grace: the grace
		// is for a holder to register again, no registration renews a marked claim, and so over
		// such a claim it could only hold the role past the window its own renewal is weighed
		// against. And every claim, restored or not, is kept while its own holder answered for it
		// inside one live window, so a holder that goes on renewing its claim keeps its role even
		// where the session id it registered under has left the bucket - which is what a second
		// process that resumes that id and then deregisters it leaves behind
		// (store.Registry.ReapRoleClaims).
		//
		// The role claim reaper runs at ENVOY_ROLE_CLAIM_REAP_INTERVAL, its own period and not
		// the session reaper's: what it waits for is what that reaper decides, but a pass of it
		// is what takes a gone holder's role away, and an operator who shortens the session
		// reaper's period to settle the bucket's maximum age sooner is not asking for that (the
		// two defaults say why). Each pass is judged against this listener's own live window,
		// not counted, so the period moves when a gone holder's role is freed and nothing else.
		done := []<-chan struct{}{
			registry.StartReaper(ctx, func(sessionID string) (bool, error) { return sessionMayBeLive(sessions, sessionID) }, 5*time.Minute, store.InterestReapGrace),
			registry.StartRoleClaimReaper(ctx, func(sessionID string) bool { return roleHolderMayBeLive(logger, sessions, sessionID) }, roleClaimReapInterval, sessionLive, sessionLive),
			// The session reaper is the one loop that deletes a session's record, and the one
			// place the bucket's maximum age is settled while the listener runs
			// (session.ReapSessions). It runs here, on the lease holder alone, as every other
			// reaper of this machine does: a pass deletes each record at the revision it judged
			// it on and writes the same maximum age every pod would write, so two pods running
			// it would be safe, but each pass also walks the stream for every record it reaps,
			// and one pass per machine is what that is worth.
			sessions.StartSessionReaper(ctx, sessionReapInterval, session.SessionReaper{
				Client:              client,
				Reaped:              sessionsReaped,
				StoredAfterLastSeen: sessionStoredAfterLastSeen,
				// The pass runs in a loop of its own, and the delete confirms the session is
				// gone against the leader of the session bucket's stream before it removes
				// anything, so a session that registered between the pass's decision and its
				// delete keeps the consumer that registration was answered with; one that
				// registered on another listener between that read and the delete is given its
				// consumer back, at the floor the delete recorded, from the record and the
				// interest row the bucket then holds (sessionDelivery.deleteConsumer,
				// repairRegistered). The delete is held to this receive term and to the
				// lease's loss deadline (the relays' term, sessionRelays.termContext), so a
				// pass that reaches it once the lease has stepped down or another pod may hold
				// it sends nothing. That body logs whatever became of the consumer and answers
				// no error, which is why the field carries none: the pass deletes the records
				// behind this one either way, and the reaper's own log line would say no more
				// than the delete's.
				DeleteConsumer: delivery.deleteReaped(ready, relays),
			}),
			// The collector removes the interest bucket's delete markers, which the reaper leaves
			// one of per dead session on a key that is never reused and which nothing else removes,
			// so every restart's cache warm-up replays them before it serves (LEGION-374). Its first
			// pass runs as receiving starts, off the readiness path; then on the reapers' cadence.
			// The purge is idempotent. The JetStream context comes from the client at each pass,
			// since a replaced connection reassigns it.
			registry.StartInterestMarkerCollector(ctx, client.JS, 5*time.Minute),
			// The sweep walks every kept role lane on the stream. A handover of a role's kept
			// lane runs in a goroutine of the pod that started it, and only a claim the claim
			// route grants, that route's refresh of a claimant that was not live, a keep or a
			// registration that makes a holder live again starts one, so a pod restarted or
			// replaced mid-handover leaves the copies behind its cursor for one of those, which
			// seldom comes inside roleKeepWindow: a live holder claims no second time and its
			// heartbeats start none. A role nobody holds is handed nothing at all, and its copies
			// must still end at the end of their hour, each with a dead letter to its sender,
			// while the copies still young wake the role again. This is the backstop over them,
			// on the reapers' cadence, with its first pass inside the term's first interval, at
			// the phase its pod name gives it (sweepPhaseSeed), so the shared listeners' passes
			// stay apart however their restarts and terms line up. The lanes and their cursors
			// are the stream's, so the pod receiving the durable sweeps for the machine and its
			// standby runs no pass of its own; whichever pod a claim, a keep or a registration
			// reaches still starts that role's handover itself. It costs one stream read a pass
			// while no role has a kept lane (startKeptRoleSweep, role_keep_sweep.go), and its
			// cadence is the interval the repeated wake report is named by (roleWakeInterval).
			// It also runs the passes of every takeover record that is not finished
			// (handOverTakenRole), on a listener whose NATS user holds the session consumers'
			// calls (passRecords), each copy and record write of them held to this receive term
			// and to the lease's loss deadline, so none is sent once the lease has stepped down
			// or another pod may hold it (keptSweepConfig, takeoverWriteContext). An edge
			// listener runs none, since its grant writes no kept lane: the shared listeners'
			// sweeps hand over the lanes of roles held at the edge too.
			startKeptRoleSweep(ctx, keptSweepConfig(deliveryConfig, relays), roleWakeInterval,
				sweepPhase(sweepPhaseSeed("kept role sweep"), roleWakeInterval)),
			// The session consumer sweep frees the slot every session consumer whose session
			// ended without deregistering holds. The deregistration route deletes the consumer
			// of a registration it removed, and a session whose pane closed or whose box went
			// deregisters nothing: this is the bound under which such a consumer goes, one pass
			// after the session's record leaves the envoy_sessions bucket - the session reaper's
			// first pass past the gone horizon, or the bucket's maximum age, whichever comes
			// first - since a pass deletes only on a bucket that answers for the session no
			// longer. It is also what retires the consumers a hub already holds from before the
			// sweep existed. A pass that found a cold or stale copy of the session bucket would
			// read the whole fleet as gone, so it runs here, on the pod holding the delivery
			// lease alone, and only over a copy that is ready, servable and needs no read. It
			// runs under this receive term's context, which the term's end cancels before it
			// clears the relays' ownership (receive.go, stopLoops then end), so a term that
			// ends between a tick and its pass sweeps nothing and among the pods of one machine
			// id only the one receiving deletes a consumer (startSessionConsumerSweep,
			// session_consumer_sweep.go, whose trusted also reads that ownership for a caller
			// that starts it anywhere else). The lease ends the term a moment before that
			// context ends, so each delete is held to the relays' term and to the lease's loss
			// deadline besides, and one reached once the lease has stepped down or another pod
			// may hold it sends nothing (sessionDelivery.deleteConsumer).
			startSessionConsumerSweep(ctx, &sessionConsumerSweep{
				delivery: delivery,
				sessions: sessions,
				deps:     ready,
				relays:   relays,
				logger:   logger,
			}, sessionConsumerSweepInterval, sweepPhase(sweepPhaseSeed("session consumer sweep"), sessionConsumerSweepInterval)),
		}
		// An edge listener reads its projects' repositories again every edgeRepositoryRefresh and
		// removes the interests of a repository that left them (runEdgeRepositories). It lists
		// and writes the interest bucket, so it runs with the reapers, under this receive term,
		// which is also the term whose relays hold each GitHub frame to a fresh answer
		// (sessionRelays.edgeWithholds). An edge listener opens no lease, so its one pod receives
		// whenever it receives at all.
		if repositories != nil {
			edgeDone := make(chan struct{})
			go func() {
				defer close(edgeDone)
				ticker := time.NewTicker(edgeRepositoryRefresh)
				defer ticker.Stop()
				runEdgeRepositories(ctx, ready, cfg.MachineID, ticker.C)
			}()
			done = append(done, edgeDone)
		}
		// The CI summary loop, on a listener that opened the CI store, emits one pr.<n>.checks
		// event once a commit's checks settle, for every commit of a pull request, its head or
		// not; new runs re-arm settlement. The debounce window is ENVOY_CI_DEBOUNCE (default 5s).
		// The loop runs a pass only on a one-second tick by which a cached record changed or fell
		// due. It publishes through the same publisher the webhook routes do: a settlement is a
		// GitHub envelope like any other, and a subscriber of a renamed repository's former name
		// waits on its checks as much as on its comments.
		if ciStore != nil {
			previous, loopDone := summaryDone, make(chan struct{})
			summaryDone = loopDone
			go func() {
				defer close(loopDone)
				<-previous
				if ctx.Err() == nil {
					<-cistore.StartSummaryLoop(ctx, ciStore, publisher, ciDebounce, 1*time.Second, ciLegacyHeld, logger)
				}
			}()
			done = append(done, loopDone)
		}
		return done
	}
	receiver := newDurableReceiver(durableReceiverOptions{
		client:   client,
		consumer: consumer,
		lease:    deliveryLease,
		handle:   jetStreamDeliveryHandler(deliveryConfig),
		loops:    holderLoops,
		// The term elects the pod that relays its machine's session consumers: that ends with it.
		relays: relays,
		// A durable refused after the check above (an operator changed it since) can never be
		// received from, so the listener stops, as that check stops a start.
		refused: func(err error) {
			logger.Error(durableRefusedLine, slog.String("error", err.Error()))
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		},
		logger: logger,
		timing: productionReceiveConfig(),
	})
	// A replaced connection invalidates the subscription; the receive loop subscribes again on the
	// connection the client now holds.
	client.AddReconnectHook(receiver.reconnected)
	// The lease judges this pod's receiving by that same connection: a holder whose receive
	// connection has been down for leaseReceiveLost releases the record, so a standby delivers
	// while this pod's lease connection, which is another connection, still renews; and no pod
	// takes the record while its receive connection is down (lease.SetReceiveSignal).
	if deliveryLease != nil {
		deliveryLease.SetReceiveSignal(receiver.receiveUp)
	}

	ready = &listenerDeps{
		client:       client,
		roleLane:     lane.client,
		consumers:    delivery,
		registry:     registry,
		sessions:     sessions,
		known:        known,
		legionAgents: legionAgents,
		ciStore:      ciStore,
		caches:       caches,
		receiver:     receiver,
		streamName:   bus.Stream,
		wakes:        wakes,
		delivery:     &deliveryConfig,
		confirmed:    dedupe.New(10 * time.Minute),

		sendsToUnresponsive: sendsToUnresponsive,
		senderLimit:         senderMessageLimit,
		dispatchPrincipal:   dispatchPrincipal,
		relaySockets:        sockets,
		repositories:        repositories,
	}
	openV1Routes(v1Gate, ready, &v1Deps, cfg.MachineID, logger, listeningAt)

	// The lease runs until the shutdown releases it (durableReceiver.Stop). Its loop runs under a
	// context nothing cancels: the shutdown ends in os.Exit (shutdownListener), so a lease the
	// release did not stop renews until the exit, and a standby takes its record once it expires.
	if deliveryLease != nil {
		deliveryLease.Start(context.Background())
	}
	receiver.Start()
	// A pod whose grant refused the listener state bucket at its start delivers without a lease,
	// and joins its machine's lease once a grant lets it open the bucket, with no restart
	// (joinDeliveryLease). What the start built without the lease follows the join here: the keeper
	// keeps the bucket, the role claim's grace and the relays' wait for a former holder read the
	// lease (podLease), the delivery ledger becomes the machine's shared one (dedupe.Joining), the
	// session registry records last-seen times in the bucket, and the lease judges this pod's
	// receiving by the receive loop's signal. The lease's first rounds then take the record or
	// read another pod's (lease.Contend), before anything this pod delivers is given up; its loop
	// starts, and the receive loop hands the term it receives on to it, which takes that term as
	// it stands or, when another pod holds the lease, ends it (durableReceiver.join). An edge
	// listener, which dialled no lease connection, joins none. The join runs under a context of
	// the receiver's own, which the shutdown ends and waits out before it stops receiving
	// (durableReceiver.startJoin, durableReceiver.Stop), so a join that completes as the pod stops
	// is handed over or released by that stop rather than left renewing until the exit; the
	// joined lease's own loop runs under a context nothing cancels, as a lease opened at the start
	// does.
	if deliveryLease == nil && leaseConn != nil {
		receiver.startJoin(func(ctx context.Context) *lease {
			joined := joinDeliveryLease(ctx, leaseConn, cfg.NATSReplicas, cfg.MachineID, instance, logger)
			if joined == nil {
				return nil
			}
			podLease.Store(joined)
			client.KeepBucket(bus.ListenerStateBucket)
			ledger.(*dedupe.Joining).Join(leaseLedger(joined, cfg.MachineID, deliveryHandlingLimit))
			sessions.SetLastSeenBucket(joined.kv)
			joined.SetReceiveSignal(receiver.receiveUp)
			joined.Contend(ctx)
			joined.Start(context.Background())
			return joined
		})
	}
	deps.Store(ready)
	logger.Info("envoy-listener ready (NATS connected)")

	// Phase 6b: Say we are alive and reporting, now and every listenerReadyInterval, on the one
	// topic the Legion daemons read wakes of released issue pods from. Without it a daemon
	// cannot tell "the listener sends no reports" from "the listener has nothing to report", so
	// it releases nothing: this is the positive signal that lets release run at all. Every pod
	// of the machine runs it, the standby included, because every pod serves the publish route
	// and so raises role wakes of its own; a report says one listener is alive and reporting,
	// which is what the daemon's window asks, and two pods saying it is the same answer twice.
	// It runs under the wake reports' own lifetime, which the shutdown ends (parts.stopWakes). An
	// edge listener reports no wake (its grant may not publish the wake topic, natsgrant.Edge),
	// so it says nothing here either.
	if edge == nil {
		startListenerReadyReports(wakesCtx, client, logger, listenerReadyInterval)
	}

	// Phase 6b4: Log the old-form counter and the start-time gauge, now and every
	// legacySubjectsLogInterval, for legacy-subjects-gate.sh's reader of a listener in a cluster,
	// who may read this pod's log and not its /metrics (startLegacySubjectsLog). Every pod of the
	// machine logs its own, the standby included, since each counts only the names it read. It
	// runs under the wake reports' lifetime, which the shutdown ends (parts.stopWakes).
	startLegacySubjectsLog(wakesCtx, logger, met, legacySubjectsLogInterval)

	// Phase 6c: Keep transient JetStream timeouts observable without restarting
	// the listener. Terminal watcher/consumer failures are rebuilt immediately;
	// after three failed recovery intervals, SIGTERM enters the bounded shutdown
	// path and exits non-zero for Docker to replace the listener. Only the pod receiving the
	// durable checks it. Every pod's probe checks its caches and its own role lane
	// (checkSelfHealth; an edge listener holds none), so a standby runs on for as long as its
	// caches answer: a role lane fault restarts no pod. The durable probe and a rebuild's
	// resubscribe run under monitorCtx, each JetStream read included, so the shutdown's
	// monitorCancel cuts a read the server has not answered and the 10 s shutdown budget is held
	// by neither.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	probeDurable := func() error { return receiver.probe(monitorCtx) }
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		runSelfHealthMonitor(
			monitorCtx,
			logger,
			func() error {
				return checkSelfHealth(caches, lane.client, probeDurable)
			},
			func(err error) bool {
				return isUnrecoverableSelfHealthFailure(err, client, caches)
			},
			client.Connected,
			func() error {
				return rebuildListenerDependencies(monitorCtx, client, caches, probeDurable, receiver.resubscribe)
			},
			func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) },
			30*time.Second,
			3,
		)
	}()

	// Phase 7: Block until SIGTERM/SIGINT or fatal error.
	select {
	case err := <-fatal:
		logger.Error("fatal error", slog.String("error", err.Error()))
	case <-stopping.Done():
		logger.Info("received signal, shutting down", slog.String("signal", context.Cause(stopping).Error()))
	}
	shutdownListener(logger, parts, listenerLoops{
		stop:        monitorCancel,
		monitorDone: monitorDone,
		receiver:    receiver,
	})
}

// startSweep runs pass at each instant whose time since the epoch, less phase, is a whole number
// of intervals (untilPhase), which is the loop every backstop sweep of the listener runs: the kept
// role lanes' (startKeptRoleSweep) and the session consumers' (startSessionConsumerSweep). phase,
// below interval (sweepPhase), is where in each interval of the clock the sweep's passes fall, so
// two listeners pass at their own phases whenever each started, and one that restarted, or whose
// receive term began again, passes where it passed before; its first pass is inside its first
// interval. A pass that outlasts an interval is followed by the next instant of its phase, and the
// ones it overran are not made up. It runs under the context the holder-only loops run under and
// returns a channel closed once its goroutine has returned, as they do (store.Registry.StartReaper),
// so the end of this pod's receive term ends it and waits for a pass in flight (holderLoops,
// waitLoops); a term that ends between passes runs no other. A pass that is still running when ctx
// ends is not cut short by this loop: each pass watches ctx itself and the term's end waits for it,
// up to holderLoopsStopWait, so a sweep ends between two of its own calls rather than inside one.
func startSweep(ctx context.Context, interval, phase time.Duration, pass func(context.Context)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			wait := time.NewTimer(untilPhase(time.Now(), interval, phase))
			select {
			case <-ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
			pass(ctx)
		}
	}()
	return done
}

// untilPhase is how long after now the next instant comes whose time since the epoch, less phase,
// is a whole number of intervals: more than zero and at most interval. interval is above zero, as
// every sweep's is (roleWakeInterval, and envconf.PositiveDuration for the session consumers'),
// and phase is below it (sweepPhase), so one interval added makes any wait that is not above zero
// one that is.
func untilPhase(now time.Time, interval, phase time.Duration) time.Duration {
	wait := phase - time.Duration(now.UnixNano()%int64(interval))
	if wait <= 0 {
		wait += interval
	}
	return wait
}

// sweepPhase is a sweep's phase in interval (startSweep), an interval above zero as untilPhase's
// is: a share of interval read from the FNV-1a hash of seed, the listener's pod name and the
// sweep's name (sweepPhaseSeed). Two listeners' phases differ by what their names hash to,
// whenever each started, so a restart of both at the same instant leaves them as far apart as
// before; and one listener's two sweeps fall apart in its interval too.
func sweepPhase(seed string, interval time.Duration) time.Duration {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(seed))
	return time.Duration(hash.Sum64() % uint64(interval))
}

// sweepPhaseSeed is the seed of sweep's phase (sweepPhase): the host name, which in a Kubernetes
// pod is the pod's name and so new at each pod a Deployment makes, then the sweep's name. A host
// whose name cannot be read seeds from a random id instead, never from the start time.
func sweepPhaseSeed(sweep string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = id.New()
	}
	return host + " " + sweep
}

// listenerLoops are what main started that the shutdown ends before it drains NATS: stop ends the
// self-health monitor, monitorDone is closed once it has returned, and receiver is the durable's
// receive loop, which stops the holder-only loops itself.
type listenerLoops struct {
	stop        func()
	monitorDone <-chan struct{}
	receiver    *durableReceiver
}

// listenerParts are the parts of a started listener that shut down: server is the /v1 HTTP server,
// client the main bus connection, lane the role lane, delivery the session-consumer connection,
// relays the relay of its consumers (nil when it does not run), sockets an edge listener's relay
// sockets (nil on every other listener), caches the KV caches whose watchers are retired last,
// and stopWakes ends the wake reports still retrying, the liveness report and the old-form
// counter's log line (wakeReporter, startListenerReadyReports, startLegacySubjectsLog).
type listenerParts struct {
	server   *http.Server
	client   *bus.Client
	lane     roleLane
	delivery *sessionDelivery
	relays   *sessionRelays
	sockets  *relaySockets
	caches   []listenerCache
	// known is the known-session store, whose watcher is retired with the caches' but which is
	// none of them (listenerDeps.known); nil on a listener with no store, which retires nothing.
	known     *session.KnownSessions
	stopWakes func()
}

// The listener's stop budget: the deadlines shutdownListener spends after SIGTERM, one after
// another. A runtime's termination grace period, less the pre-stop wait it spends before SIGTERM,
// has to cover stopBudget, and a pod of this listener grants listenerGraceMargin above it;
// TestListenerPodGracesCoverTheStopBudget holds the rendered pods to both.
const (
	// stopHTTPGrace bounds the first part of the stop: six waits run under the one context it
	// opens, in this order - the receive stop (loops.receiver.Stop), the relays' stop, the HTTP
	// server's shutdown, the consumer deletes still in flight, the session-consumer drain and the
	// wait for a self-health rebuild still running (loops.monitorDone). Between the relays' stop
	// and the HTTP server's shutdown an edge listener's relay sockets write their close frames
	// (relaySockets.stop, every socket at once) under no context, each write bounded at
	// relaySocketCloseGrace alone, so where the receive stop and the relays' stop spend the whole
	// of this grace, an edge listener's first part runs up to relaySocketCloseGrace (1 s) past it.
	// Its whole stop stays inside stopBudget all the same: its role lane has no client of its own,
	// so its drain spends none of stopLaneDrain (roleLane.drain). The receive stop is the one that
	// most plausibly spends the whole of this grace: step 2 of shutdownListener below sizes its
	// hand-over at 11 s, and 12.25 s where the flush and the release both run to their bounds, so
	// whoever sizes this number reads that step too.
	stopHTTPGrace = 10 * time.Second
	// stopLaneDrain is what roleLane.drain is given for the role lane's own client.
	stopLaneDrain = 3 * time.Second
	// stopLaneGrace is the role lane's whole stop, and the invariant is the two waits the drain
	// makes in order: stopLaneDrain for the lane's own client, then the wait roleLane.drain gives
	// the handed-over forwards, which is roleForwardTimeout (role_lane.go). Every pod's grace is
	// rendered from stopBudget, so a change to either wait moves the rendered grace with it.
	stopLaneGrace = stopLaneDrain + roleForwardTimeout
	// stopClientDrain bounds the main client's drain: 10 s for the deliveries in their handlers,
	// as before the drain hooks, and the hooks' 3 s on top (bus's drainHookReserve).
	stopClientDrain = 13 * time.Second
	// stopBudget is the whole stop, the three deadlines added, and is inside every runtime's
	// 30 s after the pre-stop wait.
	stopBudget = stopHTTPGrace + stopLaneGrace + stopClientDrain
	// listenerGraceMargin is what a pod of this listener grants above its pre-stop wait and
	// stopBudget: it covers the kubelet's own latency between the pre-stop hook's return and
	// SIGTERM, the container's teardown and a late tick on a loaded node, and never a listener
	// that runs over its own deadlines, which it cannot. It is the one place this number is
	// decided: every sentence that states the margin cites this constant, the two listener pod
	// components render their grace as their pre-stop wait plus the budget plus it, and
	// TestListenerPodGracesCoverTheStopBudget holds each rendered pod's grace less its pre-stop
	// wait less stopBudget equal to it, so a pod that grants more or less reds.
	listenerGraceMargin = 7 * time.Second
)

// shutdownListener is the ordered shutdown every end of a start that serves /v1 comes to, a
// signal, a fatal error or a bind that did not complete, and exits non-zero so the runtime's
// restart policy restores the listener.
func shutdownListener(logger *logging.Logger, parts listenerParts, loops listenerLoops) {
	// 1. Stop the self-health monitor, so nothing re-subscribes or re-watches from here, and the
	// wake reports retrying off their callers' paths: each holds its goroutine for minutes
	// otherwise, and a report published into a draining connection reaches nothing. Both are a
	// context cancel and wait on nothing, so neither spends any of the budget below.
	loops.stop()
	parts.stopWakes()

	// 2. Stop receiving the durable, inside stopHTTPGrace, the budget the relays and the HTTP stop share:
	// the term's end stops this pod's session relays (durableReceiver.end), the holder-only
	// loops stop, the handler takes no more messages, the message in hand gets inflightGrace
	// (3 s) before its handling is cancelled and it is NAKed, with a second for the handler to
	// return, every message not handled is NAKed with a delay for the next holder, and the lease
	// is marked released, so a standby takes the lease at its next poll; the subscription then
	// stays until the standby receives, at least 1.25 s after the release (lingerUntaken and a
	// poll, the floor that holds the deliver subject while the standby subscribes) and up to
	// lingerMax (3 s) after the NAKs, then drains (2 s) and, when the whole hand-over went
	// through, writes the lease's clean-hand-back mark, one bucket call of 2 s at most, which
	// ends the standby's settle hold at 3 s (durableReceiver.handBack): 11 s at most, and
	// 12.25 s when the flush (1 s) and the release (2 s) both run to their bounds and leave that
	// floor past the cap. The relays' own settle is not in that sum: the term's end only cancels
	// them, and step 3 below waits for it. This budget bounds none of those waits: it is how
	// long this shutdown waits for the receive stop. Past it the shutdown goes on to the relays
	// and the HTTP stop with no time left, then to the NATS drains below and os.Exit(1), while
	// the hand-back may still be running. The listener's own shutdown, not the runtime's kill,
	// cuts that hand-back off: client.Drain ends the subscription of a hand-back still lingering
	// or draining, and os.Exit(1) ends only a mark write still in flight on the lease's own
	// connection. So a longer grace period in the runtime keeps no mark, and the next holder
	// then settles at its 7 s cap, as it does after any stop that wrote none. HTTP keeps serving
	// meanwhile.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), stopHTTPGrace)
	defer cancel()
	loops.receiver.Stop(shutdownCtx)

	// 3. The session relays: publish no frame and ack nothing more. Each relay the term's end stopped
	// first gives the frame it had unanswered relaySettleGrace (0.75 s) to be answered and acks it
	// (sessionRelays.settleStoppedFrames), so the pod taking over does not publish it again; this step
	// waits for that, out of what the receive stop left of the budget. Each relay then returns to the
	// server, with a nak, every message it still holds (sessionRelays.release), the frames the settle
	// left in doubt among them, so the machine's pod that receives the durable next is delivered them
	// at once, not after their ack wait. Past the budget the stop warns and the shutdown goes on, and
	// a run still going may publish, have a frame acked or nak what it holds (sessionRelays.stop).
	parts.relays.stop(shutdownCtx)
	// An edge listener's relay sockets close next, each with 1012, on which its client opens it
	// again at once on another replica, and every upgrade after it is refused. They stay open
	// through the relays' settle above, since an edge session answers its frames over its socket
	// alone, and close before the HTTP server's shutdown, which does not wait for a hijacked
	// connection, so nothing else would close them. The close takes no context: it waits up to
	// relaySocketCloseGrace whatever the steps above left of stopHTTPGrace (stopHTTPGrace says
	// what that costs).
	parts.sockets.stop()

	// 4. HTTP server — stop accepting new requests, drain in-flight, within what is left of the
	// budget.
	if err := parts.server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", slog.String("error", err.Error()))
	}
	// A deregistration answers before its session consumer is deleted and leaves that delete
	// running behind it (sessionDelivery.deleteAfterAnswer), so the stop waits for those still
	// in flight once the server has stopped serving: a delete cut off here would leave its
	// consumer for the sweep, and one started after the drain below would reach a closed
	// connection.
	parts.delivery.awaitDeletes(shutdownCtx)
	// The session-consumer connection carries the consumer calls registrations make, which the HTTP
	// server has stopped serving, and the relays' acks, which its drain lets reach the server,
	// within what is left of the HTTP stop's budget.
	parts.delivery.drain(shutdownCtx)

	// 5. NATS — wait (within the same deadline) for a self-health rebuild the monitor may still be
	// running, so nothing re-subscribes or re-watches during the drain. Retire every cache's KV
	// watchers, so their end reads as the shutdown it is rather than a watcher failure, and a
	// reconnect hook still running cannot arm a new one; then drain in-flight deliveries without
	// reconnecting, end the retired watchers and delete their consumers (endListenerKVWatchers, the
	// drain hook main adds), and drain the connection, but never let a blocked NATS request pin the
	// process after its HTTP listener is gone.
	if awaitDone(shutdownCtx, loops.monitorDone) != nil {
		logger.Warn("self-health monitor still running at shutdown")
	}
	// The role lane drains before the main client: a delivery still in its handler, and each role
	// forward, publishes through the main client and reads the caches stopped below. Core NATS
	// retries nothing, so a delivery still blocked at the deadline is lost, as one sent while no
	// listener runs is.
	parts.lane.drain(logger, stopLaneDrain)
	for _, c := range parts.caches {
		c.cache.StopWatch()
	}
	parts.known.StopWatch()
	// 10 s for the deliveries in their handlers, as before the drain hooks, and the hooks' 3 s on
	// top (bus's drainHookReserve): with the 10 s the receive stop, the relays' settle and the
	// HTTP stop share and the role lane's 5 s side by side, 28 s, inside every runtime's 30 s.
	if err := parts.client.Drain(stopClientDrain); err != nil {
		logger.Warn("nats drain error", slog.String("error", err.Error()))
	}

	logger.Info("envoy-listener shutdown complete")
	os.Exit(1)
}
