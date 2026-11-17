package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// onClosed is wired as the ClosedCB callback. It reports the connection event (ConnectionChanged)
// and launches recovery in a background goroutine so the NATS library callback returns
// immediately.
func (c *Client) onClosed() {
	c.connectionChanged()
	go c.recover()
}

// onConnect is the ConnectedCB of a client that connects in the background
// (WithConnectInBackground): nats.go has connected the connection the constructor returned for the
// first time, and has sent the core subscriptions made before it. It is a connection event as a
// reconnect in place is, so onReconnect binds the JetStream subscription made before it and runs
// the reconnect hooks, an owning client's ensure of the stream first (ensureStreamOn).
// nats.go runs it once per connection, and the client never installs a second connection before
// this one has connected (ensureConnWithContext), so it marks the end of the wait only for c.Conn.
func (c *Client) onConnect(nc *nats.Conn) {
	c.mu.Lock()
	if nc == c.Conn {
		c.awaitingConnection = false
	}
	c.mu.Unlock()
	c.onReconnect(nc)
}

// onReconnect is the ReconnectedCB, run on nats.go's one callback goroutine. It reports the
// reconnect (ConnectionChanged), marks the hooks due and hands the rest, the re-subscribe and the
// run of the reconnect hooks, to a goroutine of its own (afterReconnect), and returns. That rest
// asks the server, and a request on a connection that drops again waits out its whole bound (a
// JetStream context's MaxWait, 10 s, for most hooks): run here, it would hold every callback queued
// behind it, the next disconnect's among them, which reports the outage (ConnectionChanged, the
// outbox's wake that starts a holder's loss grace) and runs the disconnect hooks
// (TestAFlapDuringTheEnsuresRequestRunsTheSecondDisconnectHookAtOnce). The run off this goroutine is
// the one recover already makes after a failed run, and it takes turns with recover's (rewatch).
func (c *Client) onReconnect(nc *nats.Conn) {
	// The reconnect is reported first (ConnectionChanged), so a publish waiting for it goes on
	// without waiting for the re-subscribe and the hooks below.
	c.connectionChanged()
	// A reconnect after the client stopped belongs to a process that is shutting down: closeStopped
	// closes the connection, or leaves it to the drain when the reconnect came before Drain stopped
	// the client.
	if c.stopped() {
		c.closeStopped(nc)
		return
	}
	// nc is the connection already in c.Conn, reconnected in place, and the JetStream context taken
	// from it keeps working, so neither field is reassigned here. The reconnect is a connection
	// event, so the hooks are due. A callback nats.go delivers late for a connection the client has
	// since replaced is not: the replacement was its own event.
	c.mu.Lock()
	if nc == c.Conn {
		c.markHooksDueLocked()
		// The session this reconnect replaced is the one the server reported its publish refusals
		// on, and the publishes they answered are over; this session's grant is read again from
		// its own refusals.
		c.clearPublishDenials()
		// nats.go has re-sent every subscription, so a refusal the server made of the core one
		// before is past: this server may grant it. A refusal of the re-sent one is reported after
		// this callback, and marks it again (onAsyncError).
		c.subscriptionsMu.Lock()
		c.subscriptions[coreSubscription].refused = false
		c.subscriptionsMu.Unlock()
	}
	c.mu.Unlock()
	go c.afterReconnect(nc)
}

// afterReconnect re-subscribes what the reconnect left unbound and runs the reconnect hooks for the
// reconnect of nc, off nats.go's callback goroutine (onReconnect), and starts the recovery for what
// it could not finish.
func (c *Client) afterReconnect(nc *nats.Conn) {
	pending, restoreErr := c.restoreSubscriptions()
	if errors.Is(restoreErr, errStopped) {
		// Stopped between onReconnect's check and the re-subscribe: same as that branch.
		c.closeStopped(nc)
		return
	}
	if restoreErr != nil {
		slog.Error("envoy nats resubscribe failed", slog.String("name", c.name), slog.String("error", restoreErr.Error()))
	}
	// Drain stops the client before it takes the subscriptions to drain them, so a stop can land
	// while restoreSubscriptions runs. The drain then owns the subscriptions and the connection, and
	// a hook would only rewatch state the drain is about to close, as recover's attempt skips them.
	if c.stopped() {
		return
	}
	// The hooks run whatever the restore returned: the state they move holds no subscription, and
	// while a listener waits out the task that holds its durable, the durable's restore is refused
	// by design while /v1 and the role lane already read that state.
	hookErr := c.rewatch()
	if hookErr != nil {
		// A failure after the stop is the stop: a shutdown that began while a hook ran closed what
		// the hook was reading through. recover reports that case the same way, with the error kept
		// on the line in case a real failure coincided with the stop.
		if c.stopped() {
			slog.Info("envoy nats reconnect hooks cancelled", slog.String("name", c.name), slog.String("error", hookErr.Error()))
			return
		}
		// A run its connection's loss cancelled is superseded, not failed: the hooks stay due, and
		// the reconnect or the recovery that replaces the connection runs them again. It is still
		// the stall's ERROR once the hooks have been uncovered for RepairGrace (reportSuperseded).
		if errors.Is(hookErr, errRunSuperseded) {
			c.reportSuperseded(hookErr)
			hookErr = nil
		} else {
			c.reportHookFailure(hookErr)
		}
	}
	// The recovery binds what the restore left unbound, a registration it left to its bind in flight
	// should that bind fail, and runs the hooks again after a failed run; until the next connection
	// event nothing else does either. The restore's binds are never under the run's context
	// (whileConnected): a bind it cancelled would race the server's release of a push-bound durable,
	// which refuses the next bind with "consumer is already bound to a subscription".
	if pending || restoreErr != nil || hookErr != nil {
		c.recover()
	}
}

// closeStopped closes nc, the connection a reconnect callback reports once the client has stopped:
// nats.go has re-sent the subscriptions a drain is draining, and nothing will drain them now, so
// the connection is closed rather than hand deliveries to a process that is exiting. A reconnect
// that came before Drain stopped the client is the drain's: nats.go can run its callback only once
// the drain is under way, and by then the drain itself is draining what nats.go re-sent, so closing
// there would cut off the deliveries still in their handlers.
func (c *Client) closeStopped(nc *nats.Conn) {
	c.mu.Lock()
	// A callback reports at least one reconnect, and a client stopped by Close alone leaves the
	// count at 0, so only a reconnect Drain counted at its stop matches.
	reconnectedBeforeTheDrain := nc == c.Conn && nc.Stats().Reconnects == c.drainedReconnects
	c.mu.Unlock()
	if !reconnectedBeforeTheDrain {
		nc.Close()
	}
}

// AddReconnectHook registers recovery for state that is attached to NATS but not represented by
// Client subscriptions, such as KV watchers. After each connection event, a reconnect in place or a
// connection the client dials to replace a closed one, a run of the hooks on the client's connection
// begins that covers it, whatever restoring the subscriptions returned. A run covers every event
// counted before it began, so an event that lands between runs gets a run of its own and a burst of
// events that land before one begins gets one. Runs take turns, never two at once, and a run that
// fails is run again by a recovery, the only way an event gets a second run. No run is on nats.go's
// callback goroutine (afterReconnect, recover), so a hook may ask the server and wait for its answer;
// a disconnect's hooks may run while it waits, and a hook that then fails leaves the hooks due.
//
// A hook is given the run's context (whileConnected), which ends when conn leaves CONNECTED or the
// client stops, and waits on nothing past it: it makes each request it can under it, and waits for
// one nats.go gives no context (the legacy bucket lookup, a previous watcher's consumer delete)
// only until it ends. A request sent on a connection that then drops gets no answer, and one that
// waited out its own bound (10 s for most) there would keep the next reconnect's run behind this
// one (TestASecondReconnectRunsItsHooksWithoutWaitingOutTheFirstRunsRequest). A hook whose context
// has ended returns, with the context's error or its request's.
func (c *Client) AddReconnectHook(hook func(ctx context.Context, conn *nats.Conn) error) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.reconnectHooks = append(c.reconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// AddDisconnectHook registers what state attached to NATS, such as a KV watcher's cache, does when
// the client's connection is lost. nats.go reconnects forever and keeps a watcher's subscription
// open through the outage, so the watcher never ends and nothing else says its cache has stopped
// following the bucket. The hooks run on every disconnect of the client's connection, in the order
// they were added, on nats.go's callback goroutine and so before the reconnect hooks of the
// reconnect that follows, and at once: no reconnect hook holds that goroutine (onReconnect). A hook
// returns at once: it runs inside that callback.
func (c *Client) AddDisconnectHook(hook func()) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.disconnectHooks = append(c.disconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// onDisconnect is wired as the DisconnectedErrCB callback, after its log line: it reports the
// connection event (ConnectionChanged), so that a publish waiting for the connection reads it
// again, and then runs the disconnect hooks.
func (c *Client) onDisconnect() {
	c.connectionChanged()
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.disconnectHooks)
	c.reconnectHooksMu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// AddDrainHook registers an end Drain gives to state attached to NATS that the client does not
// subscribe for itself, such as KV watchers, while the connection still carries requests. Drain
// runs each hook once, in the order they were added, after the delivery subscriptions have drained
// or run out their share of the timeout, and before the connection's own drain, with ctx ending at
// Drain's deadline, or as soon as the connection is lost, with ErrConnectionLost as its cause. A
// drain of a connection that is reconnecting closes it at once and runs no hook, since nothing it
// sent would reach the server.
//
// The connection's own drain cannot end a KV watcher cleanly. nats.go deletes the ordered
// consumer it created for the watcher once it has removed the watcher's subscription
// (checkDrained), so the connection's drain, which waits only for its subscriptions, reaches its
// publishes, refuses the delete as "connection draining" or closes under it, depending on the
// scheduler. A hook that stops the watcher sends that delete itself, as a request it waits for.
func (c *Client) AddDrainHook(hook func(ctx context.Context)) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.drainHooks = append(c.drainHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// ErrConnectionLost is the cause of a drain hook's context (context.Cause) when the connection
// was lost while the hooks ran. nats.go then refuses a request with the reconnect buffer off
// ("outbound buffer limit exceeded"), and a request already sent gets no answer, so what a hook
// had still to ask the server is left to the server: an ordered consumer whose interest is gone is
// deleted by its inactive threshold.
var ErrConnectionLost = errors.New("bus: the connection was lost during the drain")

// runDrainHooks runs the drain hooks one after another on conn, each bounded by deadline and by
// conn staying connected.
func (c *Client) runDrainHooks(conn *nats.Conn, deadline time.Time) {
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.drainHooks)
	c.reconnectHooksMu.Unlock()
	if len(hooks) == 0 {
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx, cancelDeadline := context.WithDeadline(ctx, deadline)
	defer cancelDeadline()
	lost := conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
	defer conn.RemoveStatusListener(lost)
	if !conn.IsConnected() {
		cancel(ErrConnectionLost)
	}
	go func() {
		select {
		case _, ok := <-lost:
			if ok {
				cancel(ErrConnectionLost)
			}
		case <-ctx.Done():
		}
	}()
	for _, hook := range hooks {
		hook(ctx)
	}
}

// rewatch runs the reconnect hooks on the current connection while they are due (hooksDue), one
// run at a time: a caller that finds a run in progress waits for it to end, then runs the hooks only
// if they are still due. A run that succeeds covers the connection events counted when it began, so
// an event during the run leaves the hooks due for one more. It keeps what HookStall reports: the
// error of a run that failed, and the clearing of the stall by a run that covered every event
// counted since. A run superseded by its connection's loss or the client's stop (errRunSuperseded)
// is neither: it keeps no error and covers nothing, so the hooks stay due and the run the next
// connection event brings moves every piece of state again, whatever this one moved before it was
// cancelled.
func (c *Client) rewatch() error {
	c.rewatchMu.Lock()
	defer c.rewatchMu.Unlock()
	c.mu.Lock()
	conn, events := c.Conn, c.connEvents
	due := events != c.hookedEvents
	c.mu.Unlock()
	if !due {
		return nil
	}
	if err := c.runReconnectHooks(conn); err != nil {
		if errors.Is(err, errRunSuperseded) {
			return err
		}
		c.mu.Lock()
		c.lastHookErr = err
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.hookedEvents = events
	// The run succeeded, so every failure before it is past, whether or not an event landed while
	// it was under way: an error kept here would name a fault this run repaired, and /healthz
	// reports it beside the stall.
	c.lastHookErr = nil
	if c.connEvents == events {
		c.hooksDueSince = time.Time{}
	} else {
		// An event landed while the run was under way, so the hooks are due again for it. Its
		// stall begins here rather than at the event, which this run was already past when it was
		// counted: that is within one run of the event's own time, which a bound of a minute
		// (RepairGrace) tolerates, and it keeps the stall measured from a point the hooks have
		// certainly not covered.
		c.hooksDueSince = c.now()
	}
	c.mu.Unlock()
	return nil
}

// hooksDue reports whether a connection event has yet to be covered by a run of the reconnect hooks
// that succeeded. It still reads true while the run covering the event is in progress, so a caller
// that acts on it goes through rewatch, which decides under its lock.
func (c *Client) hooksDue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connEvents != c.hookedEvents
}

// reportHookFailure reports err, the error of a run of the reconnect hooks that failed with the
// client still running, by how long the hooks have been uncovered rather than by the fact that one
// run failed: the recovery runs them again with backoff, so a failure the next run repairs is a
// WARN, and only a stall that has reached RepairGrace, where nothing is repairing the state behind
// the hooks, is an operator's ERROR. The stall is on the line either way, so the WARN says how
// close the outage is to that point.
func (c *Client) reportHookFailure(err error) {
	stall, _ := c.HookStall()
	// One call for both lines, so an attribute added later goes on the WARN and the ERROR alike.
	// The AWS log-metric filter reads the level and the words of the ERROR message alone
	// (infra/aws/rendering/metricfilters.go, reconnect-hooks-stalled): neither the WARN line nor
	// any attribute. An operator's own filter may match the rest, so neither line is rewritten here.
	level, msg := slog.LevelWarn, "envoy nats reconnect hook failed; it is retried"
	if stall >= RepairGrace {
		level, msg = slog.LevelError, "envoy nats reconnect hooks have not succeeded"
	}
	slog.Log(context.Background(), level, msg,
		slog.String("name", c.name),
		slog.String("error", err.Error()),
		slog.Int64("stall_ms", stall.Milliseconds()))
}

// reportSuperseded reports err, the error of a run its connection's loss or the client's stop
// superseded (errRunSuperseded). That is no failure of the hooks, so it is an INFO line, until the
// hooks have been uncovered for RepairGrace or longer: then it is the stall's ERROR all the same
// (reportHookFailure), since a link that drops during every run supersedes every run, and the
// paging alarm reads only that ERROR (infra/aws/rendering/metricfilters.go,
// reconnect-hooks-stalled). A stall reached by superseded runs alone pages as one reached by
// failed runs does (TestSupersededRunsPastTheRepairGraceRaiseTheStallError).
func (c *Client) reportSuperseded(err error) {
	if stall, _ := c.HookStall(); stall >= RepairGrace {
		c.reportHookFailure(err)
		return
	}
	slog.Info("envoy nats reconnect hooks superseded by a newer connection event", slog.String("name", c.name), slog.String("error", err.Error()))
}

// runReconnectHooks runs the reconnect hooks on conn, one after another, under a context that ends
// when conn leaves CONNECTED or the client stops (whileConnected). A run whose context ended, or
// whose connection is not CONNECTED when its hooks return, is superseded (errRunSuperseded),
// whatever its hooks returned: a hook may have moved its state before the cancel, or swallowed the
// cancel and returned nil, so the run covers nothing and every hook runs again on the connection
// that replaces this one (rewatch).
func (c *Client) runReconnectHooks(conn *nats.Conn) error {
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.reconnectHooks)
	c.reconnectHooksMu.Unlock()
	ctx, cancel := c.whileConnected(conn)
	defer cancel()
	var hookErr error
	for _, hook := range hooks {
		if hookErr = hook(ctx, conn); hookErr != nil {
			break
		}
	}
	if err := runSuperseded(ctx, conn); err != nil {
		return errors.Join(err, hookErr)
	}
	return hookErr
}

// RunEnded reports whether the reconnect hooks' run that gave a hook ctx and conn is over: its
// context (whileConnected) ended, or conn is not CONNECTED. It is the one rule for that, which
// runSuperseded and every hook that decides on its own whether a failure is a failure read, so the
// two cannot disagree. The status is read beside the context because the context is cancelled by
// a goroutine of its own: nats.go sets RECONNECTING before that goroutine hears of it, and a
// request made in between fails at once ("outbound buffer limit exceeded", the bus buffers nothing
// while it reconnects), so a hook can see that failure while the context still reads live
// (TestARunWhoseConnectionLeftConnectedIsSuperseded).
func RunEnded(ctx context.Context, conn *nats.Conn) bool {
	return ctx.Err() != nil || conn == nil || conn.Status() != nats.CONNECTED
}

// runSuperseded is the errRunSuperseded of a run that is over (RunEnded), naming why, and nil for
// a run that is not.
func runSuperseded(ctx context.Context, conn *nats.Conn) error {
	if !RunEnded(ctx, conn) {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errRunSuperseded, context.Cause(ctx))
	}
	return fmt.Errorf("%w: %w", errRunSuperseded, errRunConnectionLost)
}

// errRunConnectionLost is the cause of a run's context (context.Cause) when its connection left
// CONNECTED during the run, and errRunStopped when the client stopped. errRunSuperseded marks a
// run either one ended: no failure of the hooks, so it keeps no error, logs no WARN and leaves the
// hooks due for the run that replaces it (rewatch, afterReconnect, recoverUntilHealthy).
var (
	errRunConnectionLost = errors.New("bus: the connection the reconnect hooks ran on left CONNECTED")
	errRunStopped        = errors.New("bus: the client stopped while the reconnect hooks ran")
	errRunSuperseded     = errors.New("bus: the run of the reconnect hooks was superseded")
)

// whileConnected is the context of one run of the reconnect hooks on conn: it ends when conn
// leaves CONNECTED (a disconnect, a reconnect after it, a drain or a close), at once when conn is
// not CONNECTED as the run begins, or when the client stops. nats.go answers no request sent on a
// connection that then drops, and fails none of them at the drop (it clears its waiting requests
// only at a close), so a request under this context is what lets a run end when its connection is
// gone rather than at the request's own bound.
func (c *Client) whileConnected(conn *nats.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(context.Background())
	if conn == nil {
		cancel(errRunConnectionLost)
		return ctx, func() {}
	}
	lost := conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED, nats.DRAINING_SUBS, nats.DRAINING_PUBS)
	if conn.Status() != nats.CONNECTED {
		cancel(errRunConnectionLost)
	}
	go func() {
		defer conn.RemoveStatusListener(lost)
		select {
		case _, ok := <-lost:
			if ok {
				cancel(errRunConnectionLost)
			}
		case <-c.stopCh:
			cancel(errRunStopped)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

// recover attempts to restore the NATS connection and every recoverable subscription after a
// CLOSED state, a failed re-subscribe or a failed run of the reconnect hooks. One runs at a time: a
// caller that finds one running starts none, so the running one looks again once it has cleared its
// flag, and a failure that lands as it ends is not left to the next connection event.
func (c *Client) recover() {
	for atomic.CompareAndSwapInt32(&c.recovering, 0, 1) {
		c.recoverUntilHealthy()
		atomic.StoreInt32(&c.recovering, 0)
		if c.stopped() || (c.subscriptionsHealthy() && !c.hooksDue()) {
			return
		}
	}
}

// recoverUntilHealthy retries, with backoff, until the connection, every subscription and the
// reconnect hooks are current, or the client stops.
func (c *Client) recoverUntilHealthy() {
	// The dial retries a lost server for up to ten attempts; the stop cancels it, so a recovery
	// ends at Close or Drain instead of outliving the client.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for attempt := 1; ; attempt++ {
		if c.stopped() {
			slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
			return
		}
		if c.subscriptionsHealthy() && !c.hooksDue() {
			slog.Info("envoy nats recovery: already healthy", slog.String("name", c.name))
			return
		}
		slog.Info("envoy nats recovery attempt", slog.String("name", c.name), slog.Int("attempt", attempt))
		// failure names what the attempt's own step broke on, the dial or the restore, and is
		// logged at ERROR with the attempt. A failed run of the hooks is reported by its stall
		// instead (reportHookFailure), so it is kept apart from err here and joined only for the
		// loop's control flow: hooks still due means the attempt was not healthy.
		failure := "envoy nats recovery reconnect failed"
		pending := false
		var hookErr error
		err := c.ensureConnWithContext(ctx)
		if err == nil {
			failure = "envoy nats recovery resubscribe failed"
			pending, err = c.restoreSubscriptions()
			// The hooks move onto a connection they have not reached whatever the restore returned,
			// as afterReconnect runs them, and only while they are due: a durable a listener waits
			// out keeps the restore failing, attempt after attempt, while the hooks' state is already
			// current. A run afterReconnect has in progress is waited for, never joined by a second.
			if !c.stopped() {
				hookErr = c.rewatch()
			}
		}
		attemptErr := errors.Join(err, hookErr)
		// A failure after the stop is the stop: the dial it cancelled, a subscription the drain
		// closed, a connection it refused to install. None of them is a recovery failure, but the
		// error stays on the line in case a real one coincided with the stop.
		if c.stopped() {
			if attemptErr != nil {
				slog.Info("envoy nats recovery cancelled", slog.String("name", c.name), slog.String("error", attemptErr.Error()))
			} else {
				slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
			}
			return
		}
		if attemptErr == nil && !pending {
			slog.Info("envoy nats recovery successful", slog.String("name", c.name), slog.Int("attempt", attempt))
			return
		}
		// A registration left to its bind in flight is no failure: the restore logged it at INFO,
		// and the next attempt finds it bound or binds it.
		if err != nil {
			slog.Error(failure, slog.String("name", c.name), slog.Int("attempt", attempt), slog.String("error", err.Error()))
		}
		if hookErr != nil {
			if errors.Is(hookErr, errRunSuperseded) {
				c.reportSuperseded(hookErr)
			} else {
				c.reportHookFailure(hookErr)
			}
		}
		select {
		case <-c.stopCh:
			slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) subscriptionsHealthy() bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	for _, subscription := range c.subscriptions {
		if subscription.handler != nil && (subscription.active == nil || !subscription.active.IsValid()) {
			return false
		}
	}
	return connOK
}

// restoreSubscriptions binds again every registered subscription the connection lost, and reports
// apart from its error whether it left one pending: a registration whose bind is in flight is that
// bind's, which installs it when it ends. Nothing failed there, so it is logged at INFO, and the
// caller's recovery binds the registration should that bind fail.
func (c *Client) restoreSubscriptions() (pending bool, err error) {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	// Drain collects the subscriptions to drain under subscriptionsMu after stopping the client, so
	// a recovery that reaches here after it re-subscribes nothing.
	if c.stopped() {
		c.subscriptionsMu.Unlock()
		return false, errStopped
	}
	// Every subscription is attempted, so one that cannot be restored leaves the others delivering: a
	// listener waiting out the task that holds its durable has an unbound JetStream subscription
	// beside a core role lane that is already a member of its machine's queue group.
	var errs []error
	var restoring []*recoverableSubscription
	var pendingSubjects []string
	for index := range c.subscriptions {
		subscription := &c.subscriptions[index]
		// A reconnect in place leaves a subscription valid: nats.go has already re-sent its SUB, so
		// it delivers as it did. Unsubscribing it to bind again would race the server's release of
		// the consumer's push binding, which refuses the new bind while it still sees the old one.
		// One on a replaced connection is invalid and is bound again. A registration its caller
		// ended (DrainSubscription) is no longer here to bind, which is how a caller that must not
		// be re-attached says so: Dispatch's delivery intake gives its registration up at a
		// connection event, since neither branch here can tell a holder that has lost its durable
		// from one that should go on delivering through the outage.
		if subscription.handler == nil || subscription.active.IsValid() {
			continue
		}
		// A registration whose bind is in flight (the listener's bind attempt, whose lookup a
		// reconnect can lose, to be waited out for JetStream's 10 s) is that bind's: a second bind
		// beside it could leave the registration bound twice.
		if subscription.binding != nil {
			pendingSubjects = append(pendingSubjects, subscription.subject)
			continue
		}
		subscription.binding = make(chan struct{})
		// The bind sends the subscription anew, on a connection that may grant it.
		subscription.refused = false
		restoring = append(restoring, subscription)
	}
	registrations := make([]recoverableSubscription, len(restoring))
	for index, subscription := range restoring {
		registrations[index] = *subscription
	}
	c.subscriptionsMu.Unlock()
	for _, subject := range pendingSubjects {
		slog.Info("envoy nats resubscribe left to the bind in flight", slog.String("name", c.name), slog.String("subject", subject))
	}
	for index, subscription := range restoring {
		slog.Info("envoy nats resubscribing", slog.String("name", c.name), slog.String("subject", registrations[index].subject))
		sub, err := bind(registrations[index], conn, js)
		if err := c.endBind(subscription, sub, err, false); err != nil {
			errs = append(errs, err)
		}
	}
	return len(pendingSubjects) > 0, errors.Join(errs...)
}
