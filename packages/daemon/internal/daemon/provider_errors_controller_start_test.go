package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// controllerStartSession is the session the error-run rig registers as the project's controller
// (errorRunRig.recordRegistration), which the stand-in below is at the Envoy listener.
const controllerStartSession = "ses-controller"

// A Legion controller starts in this order (packages/pi-legion's controller-session.ts): its
// session is registered with Envoy when Oh My Pi starts, then it registers with the daemon, and
// only once that answers does it claim the controller role and register the controller topic. The
// daemon writes the provider-errors notice of a run of six as soon as the registration is
// recorded, so the notice waits until the registered session holds the role live: the role's live
// holder is reached on its own agent subject, which its consumer has followed since its first
// registration, and a role no session holds stores the notice for nobody.
//
// The stand-in follows that order against the real Envoy listener; the notice of the run of six
// must reach it once, and once only. A message kept for the role while no session held it reaches
// it too, which shows the role's claim is the one the listener hands the role's lane to.
func TestAProviderErrorsNoticeReachesAControllerThatClaimsItsRoleAfterTheRegistration(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	listener, controller, runner := r.controllerStart(t)
	role, topic := claim.ControllerToken(r.project), notify.ControllerTopic(r.project)

	// A message for the controller role while no session holds it, which the listener keeps for
	// the role's next holder.
	status, body := listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + string(role), "message": controllerKeptMessage,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "controller-kept-1",
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish to the controller role no session holds = %d %s, want 202 kept", status, body)
	}

	r.registerController()
	r.wantOneNotice("after six turns and the controller's registration,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatal("the provider-errors row finished while no session held the controller role, want it waiting")
	}

	controller.claimRole(role)
	testwait.Eventually(t, "the controller to be handed the message kept for its role", func() bool {
		return controller.holding(controllerKeptMessage) == 1
	})
	controller.register(topic)
	runner.drain()

	controller.wantProviderErrorsOnce(runner, "after it claimed the controller role,")
	// Told once: the next admission pass and the next attempts publish nothing more.
	r.offered.Add(1)
	r.runtime.controllerRegistered()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("after the notice reached the controller, the next admission pass left %d provider-errors notices in the outbox, want none: %+v", len(notices), notices)
	}
	runner.drain()
	controller.wantProviderErrorsOnce(runner, "after another admission pass,")
}

// The control: a controller that holds its role before it registers with the daemon is told of
// the run once, by the notice's first attempt, whether or not it follows the controller topic.
func TestAProviderErrorsNoticeReachesAControllerThatAlreadyHoldsItsRole(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	_, controller, runner := r.controllerStart(t)
	controller.claimRole(claim.ControllerToken(r.project))

	r.registerController()
	r.wantOneNotice("after six turns and the controller's registration,", 6)
	runner.drain()
	controller.wantProviderErrorsOnce(runner, "after the notice's first attempt,")
	r.offered.Add(1)
	r.runtime.controllerRegistered()
	runner.drain()
	controller.wantProviderErrorsOnce(runner, "after another admission pass,")
}

// A registered controller whose session does not hold the controller role for longer than the
// outbox's longest retry period. The notice's row waits on the outbox's backoff
// (record.RetryBackoff): each attempt is due one backoff after the last, a drain before then runs
// nothing, and from the seventh attempt on every attempt is RetryBackoffMost after the last, so
// the row never spins. The wait is logged as settle logs a wait that is no routine one, and never
// as a failure: INFO at the first attempt, DEBUG at the next six, and WARN from the eighth on, once
// seven attempts have waited (waitWarnAttempts), which is the operator's sign that a registered
// controller does not hold its role. The row holds nothing else of its issue behind it: a notice
// of another kind written for the issue after it runs at once. Once the session holds the role,
// the next attempt tells it once.
func TestAProviderErrorsNoticeWaitsOnItsBackoffUntilTheControllerHoldsItsRoleAndHoldsNothingBehindIt(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	_, controller, runner := r.controllerStart(t)
	logs := &syncBuffer{}
	runner.outbox.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.registerController()
	r.wantOneNotice("after six turns and the controller's registration,", 6)
	tick, err := record.NewOutboxRow(errorRunIssue, record.ControllerNotice{Kind: record.TickNotice}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return r.runtime.records.Enqueue(context.Background(), tx, tick)
	}); err != nil {
		t.Fatalf("queue a tick notice of %s behind the provider-errors notice: %v", errorRunIssue, err)
	}

	runner.drainAfter(time.Minute)
	var ticks int
	if err := r.pool.QueryRow(context.Background(), "select count(*) from outbox where kind = $1 and payload->>'kind' = $2",
		string(record.OutboxKindControllerNotice), string(record.TickNotice)).Scan(&ticks); err != nil {
		t.Fatal(err)
	}
	if ticks != 0 {
		t.Fatalf("the tick notice of %s written after the provider-errors notice is still in the outbox, want it run while the provider-errors notice waits", errorRunIssue)
	}
	const attempts = 9
	waited := time.Duration(0)
	for attempt := 1; attempt <= attempts; attempt++ {
		ran, dueIn, held := runner.providerErrorsRow()
		if !held || ran != attempt {
			t.Fatalf("after attempt %d the provider-errors row is held %t with %d attempts, want it waiting with %d", attempt, held, ran, attempt)
		}
		if want := record.RetryBackoff(attempt - 1); dueIn != want {
			t.Fatalf("after attempt %d the provider-errors row is due in %s, want its backoff of %s", attempt, dueIn, want)
		}
		runner.drainAfter(dueIn / 2)
		if again, _, _ := runner.providerErrorsRow(); again != attempt {
			t.Fatalf("a drain half way through the backoff after attempt %d ran the row: %d attempts", attempt, again)
		}
		if attempt < attempts {
			runner.drainAfter(dueIn - dueIn/2)
		}
		waited += dueIn
	}
	if waited <= record.RetryBackoffMost {
		t.Fatalf("the case waited %s, want longer than the longest retry period, %s", waited, record.RetryBackoffMost)
	}
	want := []string{"INFO", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "WARN", "WARN"}
	if levels := providerErrorsWaits(t, logs.String()); !slices.Equal(levels, want) {
		t.Fatalf("the provider-errors row's %d attempts logged its wait at %v, want %v, and never as a failure:\n%s", attempts, levels, want, logs.String())
	}

	controller.claimRole(claim.ControllerToken(r.project))
	runner.drainAfter(record.RetryBackoffMost)
	if _, _, held := runner.providerErrorsRow(); held {
		t.Fatal("the provider-errors row is still in the outbox after the controller claimed its role and the row's next attempt ran")
	}
	controller.wantProviderErrorsOnce(runner, "after it claimed the controller role past the longest retry period,")
	runner.drain()
	controller.wantProviderErrorsOnce(runner, "after one more drain,")
}

// providerErrorsWaits is the level of each line settle logged for the provider-errors row, in
// order, read off its text log: an `outbox row waits` line is its level, and an `outbox row
// failed` line, which a wait the row returns as a failure logs at every attempt, is FAILED. A line
// with no level is a failure of the reader's own.
func providerErrorsWaits(t *testing.T, logged string) []string {
	t.Helper()
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if !strings.Contains(line, "provider-errors notice") {
			continue
		}
		var waits, failed bool
		level := ""
		for _, field := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(field, "level="); ok {
				level = value
			}
		}
		waits, failed = strings.Contains(line, `msg="outbox row waits"`), strings.Contains(line, `msg="outbox row failed"`)
		switch {
		case failed:
			levels = append(levels, "FAILED")
		case waits && level != "":
			levels = append(levels, level)
		case waits:
			t.Fatalf("an `outbox row waits` line carries no level: %s", line)
		}
	}
	return levels
}

// A controller restarts after its registration and before it claims its role: the first session
// never claims it, and a new session registers with Envoy, registers with the daemon as the
// controller and claims the role. The notice reaches the new session once, and the first session
// nothing; the new registration's admission pass writes no second notice.
func TestAProviderErrorsNoticeReachesTheSessionAControllerRestartsAsBeforeItClaimsItsRole(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	listener, first, runner := r.controllerStart(t)
	r.registerController()
	r.wantOneNotice("after six turns and the controller's registration,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatal("the provider-errors row finished while no session held the controller role, want it waiting")
	}

	restarted := newControllerStandin(t, listener, runner.natsURL, controllerStartSession+"-restarted")
	restarted.register()
	r.replaceController(restarted.session)
	if notices := r.notices(); len(notices) != 1 {
		t.Fatalf("after the restarted controller's registration the outbox holds %d provider-errors notices, want the one still waiting: %+v", len(notices), notices)
	}
	restarted.claimRole(claim.ControllerToken(r.project))
	restarted.register(notify.ControllerTopic(r.project))
	runner.drain()
	restarted.wantProviderErrorsOnce(runner, "after the restarted session claimed the controller role,")
	if n := first.holding(noticeSummary(record.ProviderErrorsNotice, errorRunIssue)); n != 0 {
		t.Fatalf("the first session, which never claimed the role, was delivered the notice %d times, want none", n)
	}
}

// An operator starts a new controller while the one before it still runs: the daemon's
// registration names the new session, and the old one still holds the controller role live until
// the new one claims it. The old session is no longer the project's controller — the new
// capability revoked its registration and its grants — so the notice waits rather than reach it,
// and reaches the new session once it holds the role.
func TestAProviderErrorsNoticeWaitsWhileASessionOtherThanTheRegisteredControllerHoldsItsRole(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	listener, old, runner := r.controllerStart(t)
	role := claim.ControllerToken(r.project)
	old.claimRole(role)

	next := newControllerStandin(t, listener, runner.natsURL, controllerStartSession+"-next")
	next.register()
	r.replaceController(next.session)
	r.wantOneNotice("after six turns and the new controller's registration,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatalf("the provider-errors row finished while %s, not the registered %s, held the controller role, want it waiting", old.session, next.session)
	}
	if n := old.holding(noticeSummary(record.ProviderErrorsNotice, errorRunIssue)); n != 0 {
		t.Fatalf("the controller whose registration was replaced was delivered the notice %d times, want none", n)
	}

	next.claimRole(role)
	runner.drain()
	next.wantProviderErrorsOnce(runner, "after the registered controller claimed the controller role,")
	if n := old.holding(noticeSummary(record.ProviderErrorsNotice, errorRunIssue)); n != 0 {
		t.Fatalf("the controller whose registration was replaced was delivered the notice %d times, want none", n)
	}
}

// controllerAwayWindow is the listener's live window in the cases whose controller stops
// registering (ENVOY_SESSION_LIVE_WINDOW, which the rig's listener takes from the test's
// environment) in place of the shipped five minutes, so such a controller reads unresponsive —
// past its live window and inside the gone horizon, which stays the shipped hour — within the
// case. A session the case keeps live registers every second (controllerStandin.heartbeat).
const controllerAwayWindow = 15 * time.Second

// The registered controller held its role and followed the controller topic, and then its
// process died: it registers no more, and the listener reads it unresponsive, not gone, for up to
// an hour. A run of six reaches its sixth turn in that hour. An operator then starts a
// replacement controller: a new session that registers with Envoy and with the daemon and claims
// the role. The replacement is the project's controller, and it is told of the run once; the dead
// session's consumer is stored nothing.
func TestAProviderErrorsNoticeDueWhileTheControllerIsDeadReachesItsReplacement(t *testing.T) {
	t.Setenv("ENVOY_SESSION_LIVE_WINDOW", controllerAwayWindow.String())
	r := newErrorRunRig(t)
	listener, dead, runner := r.controllerStart(t)
	role, topic := claim.ControllerToken(r.project), notify.ControllerTopic(r.project)
	dead.claimRole(role)
	dead.register(topic)
	r.registerController()
	runner.drain()
	dead.awaitUnresponsive()

	for range 6 {
		r.errorTurn()
	}
	r.wantOneNotice("after six turns with the dead controller registered,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatalf("the provider-errors row finished while the registered controller session %s read unresponsive, want it waiting", dead.session)
	}

	replacement := newControllerStandin(t, listener, runner.natsURL, controllerStartSession+"-replacement")
	replacement.register()
	replacement.heartbeat()
	r.replaceController(replacement.session)
	replacement.claimRole(role)
	replacement.register(topic)
	runner.drain()
	replacement.wantProviderErrorsOnce(runner, "after the replacement controller claimed the controller role,")
	r.offered.Add(1)
	r.runtime.controllerRegistered()
	runner.drain()
	replacement.wantProviderErrorsOnce(runner, "after another admission pass,")
	if n := dead.holding(noticeSummary(record.ProviderErrorsNotice, errorRunIssue)); n != 0 {
		t.Fatalf("the dead controller session was delivered the notice %d times, want none", n)
	}
}

// The control: the registered controller stops registering and then comes back, the same session
// registering again and taking its role again as its next heartbeat does. It is told of the run
// once, after it is live again, and not a second time.
func TestAProviderErrorsNoticeDueWhileTheControllerIsAwayReachesItOnceItComesBack(t *testing.T) {
	t.Setenv("ENVOY_SESSION_LIVE_WINDOW", controllerAwayWindow.String())
	r := newErrorRunRig(t)
	_, away, runner := r.controllerStart(t)
	role, topic := claim.ControllerToken(r.project), notify.ControllerTopic(r.project)
	away.claimRole(role)
	away.register(topic)
	r.registerController()
	runner.drain()
	away.awaitUnresponsive()

	for range 6 {
		r.errorTurn()
	}
	r.wantOneNotice("after six turns with the controller away,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatalf("the provider-errors row finished while the registered controller session %s read unresponsive, want it waiting", away.session)
	}

	away.register(topic)
	away.heartbeat(topic)
	away.claimRole(role)
	runner.drain()
	away.wantProviderErrorsOnce(runner, "after the controller that was away came back,")
	r.offered.Add(1)
	r.runtime.controllerRegistered()
	runner.drain()
	away.wantProviderErrorsOnce(runner, "after another admission pass,")
	if n := len(r.notices()); n != 0 {
		t.Fatalf("after the notice reached the controller, the outbox holds %d provider-errors notices, want none", n)
	}
}

// The registered controller holds its role live when the outbox reads it, and its registration
// passes the live window before the publish: its process died between the read and the publish.
// The listener resolves the role's holder again at the publish, stores the notice for nobody, and
// the row waits; the replacement that claims the role is told once. A notice published to the
// controller topic after the same read would be stored for the dead session's consumer instead,
// and its replacement would never be handed it.
func TestAProviderErrorsNoticeWhoseControllerDiesAfterTheReadReachesItsReplacement(t *testing.T) {
	t.Setenv("ENVOY_SESSION_LIVE_WINDOW", controllerAwayWindow.String())
	r := newErrorRunRig(t)
	listener, dead, runner := r.controllerStart(t)
	role, topic := claim.ControllerToken(r.project), notify.ControllerTopic(r.project)
	dead.claimRole(role)
	dead.register(topic)
	stop := dead.heartbeat(topic)
	runner.outbox.notices = diesAfterTheRead{outboxNotices: runner.outbox.notices, session: dead, stop: stop}
	for range 6 {
		r.errorTurn()
	}
	r.registerController()
	r.wantOneNotice("after six turns with the controller registered,", 6)
	runner.drain()
	if _, _, held := runner.providerErrorsRow(); !held {
		t.Fatalf("the provider-errors row finished after its publish found the registered session %s unresponsive, want it waiting", dead.session)
	}

	replacement := newControllerStandin(t, listener, runner.natsURL, controllerStartSession+"-replacement")
	replacement.register()
	replacement.heartbeat()
	r.replaceController(replacement.session)
	replacement.claimRole(role)
	replacement.register(topic)
	runner.drain()
	replacement.wantProviderErrorsOnce(runner, "after the replacement controller claimed the controller role,")
	if n := dead.holding(noticeSummary(record.ProviderErrorsNotice, errorRunIssue)); n != 0 {
		t.Fatalf("the controller that died after the read was delivered the notice %d times, want none", n)
	}
}

// diesAfterTheRead is the listener as the outbox meets it when the controller's process dies
// between the outbox's read of the role's holder and its publish: the read is answered as the
// listener answers it, and then, the first time the read names the session live, the session's
// heartbeat stops and the read returns only once the listener reads the session unresponsive.
type diesAfterTheRead struct {
	outboxNotices
	session *controllerStandin
	stop    func()
}

func (d diesAfterTheRead) RoleHolder(ctx context.Context, role string) (string, string, error) {
	holder, state, err := d.outboxNotices.RoleHolder(ctx, role)
	if err == nil && holder == d.session.session && state == notify.StateLive {
		d.stop()
		d.session.awaitUnresponsive()
	}
	return holder, state, err
}

// controllerKeptMessage is the message the listener keeps for the controller role while no
// session holds it.
const controllerKeptMessage = "a message for the controller role, kept while no session held it"

// controllerStart is the Envoy side of a controller's start: a real listener on the package's
// NATS server, the controller session's stand-in, registered with its own subject alone as its
// plugin registers it when Oh My Pi starts, and the daemon's outbox publishing through that
// listener.
func (r *errorRunRig) controllerStart(t *testing.T) (*idleRigListener, *controllerStandin, *controllerStartOutbox) {
	t.Helper()
	natsURL := testnats.URL(t)
	listener := startIdleRigListener(t, natsURL, "controller-start")
	controller := newControllerStandin(t, listener, natsURL, controllerStartSession)
	controller.register()
	// The store keeps a row's due moment to the microsecond, so the rig's clock is held to it,
	// and a backoff read back off the row is the span the outbox added.
	clock := time.Now().UTC().Truncate(time.Microsecond)
	runner := &controllerStartOutbox{t: t, at: &clock, natsURL: natsURL, outbox: &outbox{log: quietLogger(), pool: r.pool,
		dispatchProject: errorRunProject, records: r.runtime.records, notices: notify.New(listener.url, ""),
		project: r.project, now: func() time.Time { return clock }}}
	runner.publisher = notify.New(listener.url, "")
	runner.topic = notify.RoleTopicPrefix + string(claim.ControllerToken(r.project))
	return listener, controller, runner
}

// replaceController registers session as the project's controller in place of the one before
// it, as `legion controller start` does for a controller it starts again: a new capability, the
// session's registration under it, and the daemon told of the registration.
func (r *errorRunRig) replaceController(session string) {
	r.t.Helper()
	generation, err := r.st.MintController(r.ctx, r.project, []byte("capability-"+session))
	if err != nil {
		r.t.Fatalf("mint the capability of the controller %s: %v", session, err)
	}
	if ok, err := r.st.RegisterController(r.ctx, r.project, generation, session, []byte("secret-"+session), time.Now(), 0); err != nil || !ok {
		r.t.Fatalf("register the controller %s = %t, %v", session, ok, err)
	}
	r.offered.Add(1)
	r.runtime.controllerRegistered()
}

// controllerStartOutbox is the daemon's outbox over the rig's database, drained by hand on a clock
// of its own. drain runs it an hour after the last drain, past any backoff a waiting row was
// given, so a row that waits runs again at the next drain; drainAfter runs it a given span later.
type controllerStartOutbox struct {
	t         *testing.T
	at        *time.Time
	natsURL   string
	outbox    *outbox
	publisher *notify.HTTPPublisher
	topic     string
	markers   int
}

func (o *controllerStartOutbox) drain() {
	o.t.Helper()
	o.drainAfter(time.Hour)
}

func (o *controllerStartOutbox) drainAfter(span time.Duration) {
	o.t.Helper()
	*o.at = o.at.Add(span)
	if err := o.outbox.RunOnce(context.Background()); err != nil {
		o.t.Fatalf("drain the outbox: %v", err)
	}
}

// providerErrorsRow is the outbox's provider-errors notice row: its attempts and how long after
// the rig's clock it is due, and false when the outbox holds none.
func (o *controllerStartOutbox) providerErrorsRow() (attempts int, dueIn time.Duration, held bool) {
	o.t.Helper()
	var next time.Time
	err := o.outbox.pool.QueryRow(context.Background(), `select attempts, next_at from outbox
		where kind = $1 and payload->>'kind' = $2`, string(record.OutboxKindControllerNotice), string(record.ProviderErrorsNotice)).Scan(&attempts, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false
	}
	if err != nil {
		o.t.Fatalf("read the provider-errors row: %v", err)
	}
	return attempts, next.Sub(*o.at), true
}

// marker publishes one more message to the controller's role and answers its words: the stream
// delivers a session's messages in the order it stored them, so once the controller holds the
// marker it holds every earlier message it will ever be delivered.
func (o *controllerStartOutbox) marker() string {
	o.t.Helper()
	o.markers++
	words := "marker " + strconv.Itoa(o.markers)
	if err := o.publisher.Publish(context.Background(), o.topic, words, record.Notice{Kind: record.TickNotice},
		"controller-start-marker-"+strconv.Itoa(o.markers)); err != nil {
		o.t.Fatalf("publish %s to the controller's role: %v", words, err)
	}
	return words
}

// controllerStandin is a Legion controller's Oh My Pi session as the Envoy listener sees it: a
// session registered with consumer delivery, whose consumer the listener relays to the session's
// own NATS connection, which takes each frame as a plugin does (idleRigRelay) and keeps the
// messages it was delivered.
type controllerStandin struct {
	t        *testing.T
	listener *idleRigListener
	session  string
	mu       sync.Mutex
	messages []idleRigEnvelope
}

func newControllerStandin(t *testing.T, listener *idleRigListener, natsURL, session string) *controllerStandin {
	t.Helper()
	c := &controllerStandin{t: t, listener: listener, session: session}
	bus := testnats.Connect(t, natsURL)
	relay := newIdleRigRelay(session, bus, func(item idleRigEnvelope) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.messages = append(c.messages, item)
		return nil
	}, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "controller stand-in %s: %s\n", session, fmt.Sprintf(format, args...))
	})
	if _, err := bus.Subscribe(idleRigDeliverSubjectPrefix+session, relay.relayed); err != nil {
		t.Fatalf("subscribe to the relay subject of %s: %v", session, err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("flush the relay subscription of %s: %v", session, err)
	}
	return c
}

// registration is the body the session registers with: topics beside its own subject, as the
// plugin registers every subject it follows, with its own consumer relayed to it.
func (c *controllerStandin) registration(topics []string) map[string]any {
	if topics == nil {
		topics = []string{}
	}
	return map[string]any{
		"session_id": c.session, "dir": "/legion/controller", "topics": topics, "host": "127.0.0.1",
		"title": "controller", "self_subscribed": true, "capabilities": []string{"prompt_async"},
		"plugin": "controller-standin", "plugin_version": "0", "delivery": map[string]any{"consumer": true, "epoch": "e1"},
	}
}

// register registers the session with topics beside its own subject.
func (c *controllerStandin) register(topics ...string) {
	c.t.Helper()
	status, body := c.listener.call("/v1/interests/subscribe", c.registration(topics))
	if status != http.StatusOK {
		c.t.Fatalf("register %s with %v = %d %s", c.session, topics, status, body)
	}
	var answer struct {
		Delivery *struct {
			Mode string `json:"mode"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Delivery == nil || answer.Delivery.Mode != idleRigDeliveryConsumer {
		c.t.Fatalf("register %s answered %s, want its own consumer's delivery", c.session, body)
	}
}

// heartbeat registers the session with topics every second until the case ends or the answer
// is called, as the plugin's heartbeat registers it, so it stays live under the case's short live
// window (controllerAwayWindow). The answer stops it, and returns once no registration of it is in
// flight.
func (c *controllerStandin) heartbeat(topics ...string) (stop func()) {
	c.t.Helper()
	body, err := json.Marshal(c.registration(topics))
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	c.t.Cleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.listener.url+"/v1/interests/subscribe", bytes.NewReader(body))
			if err != nil {
				return
			}
			request.Header.Set("Content-Type", "application/json")
			if response, err := client.Do(request); err == nil {
				response.Body.Close()
			}
		}
	}()
	return stop
}

// awaitUnresponsive waits until the listener reads the session unresponsive: it answers the
// session only when asked for the unresponsive ones, past its live window and inside the gone
// horizon.
func (c *controllerStandin) awaitUnresponsive() {
	c.t.Helper()
	status := func(query string) int {
		response, err := c.listener.client.Get(c.listener.url + "/v1/registry/" + c.session + query)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	idleRigEventually(c.t, 4*controllerAwayWindow, "the listener reading "+c.session+" unresponsive", func() bool {
		return status("") == http.StatusNotFound && status("?include=unresponsive") == http.StatusOK
	})
}

// claimRole claims role for the session, as the plugin's role claim does.
func (c *controllerStandin) claimRole(role claim.Token) {
	c.t.Helper()
	if status, body := c.listener.call("/v1/roles/set", map[string]any{"role": string(role), "session_id": c.session}); status != http.StatusOK {
		c.t.Fatalf("claim %s for %s = %d %s", role, c.session, status, body)
	}
}

// holding is how many of the messages the session was delivered say words, in their summary or
// their payload.
func (c *controllerStandin) holding(words string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, item := range c.messages {
		if strings.Contains(item.PayloadSummary, words) || strings.Contains(item.Payload, words) {
			n++
		}
	}
	return n
}

// wantProviderErrorsOnce fails the case unless the session was delivered the provider-errors
// notice of the issue exactly once, read once a marker published to the role after it has
// arrived.
func (c *controllerStandin) wantProviderErrorsOnce(runner *controllerStartOutbox, when string) {
	c.t.Helper()
	marker := runner.marker()
	testwait.Eventually(c.t, "the controller to be delivered "+marker, func() bool { return c.holding(marker) == 1 })
	notice := noticeSummary(record.ProviderErrorsNotice, errorRunIssue)
	if n := c.holding(notice); n != 1 {
		c.mu.Lock()
		delivered := append([]idleRigEnvelope(nil), c.messages...)
		c.mu.Unlock()
		c.t.Fatalf("%s the controller session was delivered %q %d times, want once: %+v", when, notice, n, delivered)
	}
}
