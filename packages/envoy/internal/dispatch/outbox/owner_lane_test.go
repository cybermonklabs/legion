package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// laterEvent appends another human message on an issue routedEvent already seeded, so that an
// owner's second event commits while its first is still being sent.
func (r *roleRig) laterEvent(t *testing.T, key string, actor model.Actor) model.Event {
	t.Helper()
	return appendEvent(t, r.database, r.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: actor,
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new(key), Body: "the later message"},
	})
}

// An owner is given back as its own sends settle, not when its batch ends: an issue read in the
// same batch as a stalled holder's, whose own forward the stream acknowledged, has its later
// event - one committed while the stalled holder's forward is still waiting for an
// acknowledgement that never comes - forwarded while that wait is still running.
//
// The verdict is the order of those two facts rather than a clock, because a clock measures the
// machine as much as the outbox: this row bounded the later forward at 1 s, and under 48 busy
// loops on this 2-CPU box that bound failed 10 of 20 runs with nothing wrong in any of them, the
// slowest at 3.86 s.
//
// TestAForwardTheStreamDoesNotAcknowledgeDoesNotDelayAnotherIssuesEvents covers two owners in
// two batches; this covers two owners in one, which only a per-owner release reaches. Both
// issues' events are committed before the outbox runs, so its first batch reads both owners.
func TestAnOwnerOfTheStalledHoldersBatchIsReleasedBeforeTheBatchEnds(t *testing.T) {
	t.Parallel()
	const (
		stalledRole, stalledHolder = "stalled-reviewer", "ses_never_acknowledged"
		liveRole, liveHolder       = "live-reviewer", "ses_acknowledged"
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRigWithAckClock(t, clocktest.New(time.Time{}))
	rig.claim(t, stalledRole, stalledHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	// The stream stores and acknowledges the live holder's forwards alone, so the stalled
	// holder's wait out their whole acknowledgement wait.
	rig.narrowAgentCapture(t, liveHolder)
	stalled := rig.receiver(t, stalledHolder)
	live := rig.receiver(t, liveHolder)
	held := rig.routedEvent(t, "T-1", stalledRole, alice)
	first := rig.routedEvent(t, "T-2", liveRole, alice)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps)
	defer stop()

	testwait.Receive(t, "the stalled holder's forward", stalled.arrived)
	testwait.Receive(t, "the live holder's first forward of its issue", live.arrived)

	// The stalled holder's forward is still waiting for an acknowledgement, and every send of the
	// other owner of its batch has been answered. That owner's next event is committed in it.
	waitFor(t, "the live owner's first event published while the stalled forward still waits", func() bool {
		return publishedAt(t, rig.database, first.ID) != nil
	})
	rig.laterEvent(t, "T-2", alice)
	testwait.Receive(t, "the live owner's later forward before the stalled holder's batch ends", live.arrived)
	if settled, state := settledState(t, rig.database, held.ID); settled {
		t.Fatalf("the stalled holder's event %d had been settled when the live holder's later "+
			"forward arrived (%s), so the later forward followed its batch rather than overtaking it: "+
			"the acknowledgement wait should still have been running", held.ID, state)
	}
}

// One owner's order holds across the batches running side by side: while its first event waits for
// the stream's acknowledgement of a forward it never gets, the batch keeps that owner in flight,
// so no batch beside it reads the owner's second event and the holder receives the first forward
// alone.
func TestAnOwnersSecondEventDoesNotOvertakeItsFirstWhileItsForwardIsUnacknowledged(t *testing.T) {
	t.Parallel()
	const stalledRole, stalledHolder = "stalled-reviewer", "ses_never_acknowledged"
	alice := model.Actor{Kind: "user", ID: "alice"}
	ack := clocktest.New(time.Time{})
	rig := newRoleRigWithAckClock(t, ack)
	rig.claim(t, stalledRole, stalledHolder, session.SessionEntry{SelfSubscribed: true})
	// The stream captures no agent subject, so no forward of this holder is ever acknowledged.
	rig.narrowAgentCapture(t)
	silent := rig.receiver(t, stalledHolder)
	topic := subscribe(t, rig.conn, "notifications.dispatch.issue.*.message.created")

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps)
	defer stop()

	first := rig.routedEvent(t, "T-1", stalledRole, alice)
	forward := testwait.Receive(t, "the stalled holder's forward", silent.arrived)
	if forward.SourceEventID != fmt.Sprint(first.ID) {
		t.Fatalf("the stalled holder received event %s first, want event %d", forward.SourceEventID, first.ID)
	}

	ack.AwaitArmedAt(t, "the stalled forward waiting for the stream's acknowledgement", ack.Now().Add(roleForwardAckTimeout))
	// The other owner's topic copy and the second event would use the same connection. If the
	// scan mistakenly reads both, it sends the second event's topic copy first.
	seedSecondIssue(t, rig.database, "T-2")
	message := func(key string) model.Event {
		return model.Event{IssueKey: new(key), Type: "message.created", Actor: alice,
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new(key), Body: "the later message"}}
	}
	committed := appendEvents(t, rig.database, rig.broker, message("T-1"), message("T-2"))
	for {
		msg := testwait.Receive(t, "the other owner's topic copy while the first forward is unacknowledged", topic)
		id := envelopeEventID(t, msg.Data)
		if id == first.ID {
			continue
		}
		if id != committed[1].ID {
			t.Fatalf("event %d reached its topic before the other owner's event %d while event %d's forward was unanswered",
				id, committed[1].ID, first.ID)
		}
		break
	}
	if got := silent.receivedNow(t, rig.client); len(got) != 1 {
		t.Fatalf("the stalled holder received events %v while event %d's forward was unanswered, want that forward alone",
			keysOf(got), first.ID)
	}
}

// backOffIn backs event id off as a failed attempt leaves it: attempts counted, and due again
// wait from now on the database's clock. attempts is what decides how long its next failure
// holds it (retry.Delay), so a test that must not read a prompt retry as its fix counts a few.
func backOffIn(t *testing.T, database *store.Store, id int64, attempts int, wait time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update events set attempt_count = $2, next_attempt_at = now() + ($3 * interval '1 millisecond') where id = $1
	`, id, attempts, wait.Milliseconds()); err != nil {
		t.Fatalf("back event %d off: %v", id, err)
	}
}

// An event committed while its owner is in flight is read as soon as that owner is given back,
// not at the next back-off: the scan reads again once every batch whose read left owners out has
// returned.
//
// The wake the second event gives is spent on a batch that skips the owner in flight and reads
// another issue, which it sends and marks nothing of, so neither a batch that published nor one
// that read nothing tells the scan to read once more. What does is that the read left an owner
// out (batchResult.excluded). Both issues are routed to a holder whose forwards the stream never
// acknowledges, so neither owner is settled before its batch ends and neither release wakes the
// scan; with the second event unread, the scan's retry timer is armed to the two back-offs
// alone, minutes out.
func TestAnEventCommittedWhileItsOwnerIsInFlightIsReadWhenThatOwnerIsBack(t *testing.T) {
	t.Parallel()
	const (
		stalledRole, stalledHolder = "stalled-reviewer", "ses_never_acknowledged"
		// Long back-offs after failure; neither timer is fired after the two forwards time out.
		attempts   = 6
		issueTopic = "notifications.dispatch.issue.T-1.message.created"
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	ack, timers := clocktest.New(time.Time{}), clocktest.New(time.Time{})
	rig := newRoleRigWithAckClock(t, ack)
	rig.claim(t, stalledRole, stalledHolder, session.SessionEntry{SelfSubscribed: true})
	// The stream captures no agent subject, so no forward of this holder is ever acknowledged.
	rig.narrowAgentCapture(t)
	silent := rig.receiver(t, stalledHolder)
	topic := subscribe(t, rig.conn, issueTopic)

	// Neither event is due when the outbox starts, so the batch that reads the first issue is the
	// one its retry timer starts, with the LISTEN connection's catch-up wake long spent and no
	// batch beside it. The second issue waits until that batch is on its forward.
	first := rig.routedEvent(t, "T-1", stalledRole, alice)
	other := rig.routedEvent(t, "T-2", stalledRole, alice)
	backOffIn(t, rig.database, first.ID, attempts, stagedAhead)
	backOffIn(t, rig.database, other.ID, attempts, 2*stagedAhead)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	outbox := New(deps)
	stop, atRest := runResting(t, outbox, timers)
	defer stop()
	atRest("the holder resting with the first issue backed off")
	plantRetry(t, rig.database, timers, first.ID)

	testwait.Receive(t, "the stalled holder's forward of the first issue", silent.arrived)
	// The first issue is in flight on its wait for an acknowledgement. The second issue falls
	// due, which wakes nothing - an update notifies no listener - and the first issue's second
	// event commits: its one wake is spent on the batch that reads the second issue and skips the
	// owner in flight.
	dueNow(t, rig.database, other.ID)
	firstForward := ack.Now().Add(roleForwardAckTimeout)
	second := rig.laterEvent(t, "T-1", alice)

	otherForward := testwait.Receive(t, "the stalled holder's forward of the second issue", silent.arrived)
	if otherForward.SourceEventID != fmt.Sprint(other.ID) {
		t.Fatalf("the scan read event %s while its owner was in flight; want only event %d", otherForward.SourceEventID, other.ID)
	}
	ack.AwaitArmedCountAt(t, "both forward acknowledgement deadlines", firstForward, 2)
	crossDeadline(ack, firstForward, func() {
		for _, event := range []model.Event{first, other} {
			if _, counted := publishedDestinations(t, rig.database, event.ID); counted != attempts {
				t.Fatalf("event %d counted another attempt before its forward acknowledgement deadline", event.ID)
			}
		}
	})
	for {
		message := testwait.Receive(t, "the second event's topic copy once its owner is back without firing the retry timer", topic)
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode %s: %v", issueTopic, err)
		}
		if item.SourceEventID == fmt.Sprint(second.ID) {
			break
		}
	}
}

// answeringTopics is a test publisher that answers each topic with the error answers holds for
// it, and every other topic with nothing. The map is written before the batch runs and only read
// while it does. It serves no Request: these batches route to no role.
type answeringTopics struct {
	publishOnly
	answers map[string]error
}

func (p *answeringTopics) Publish(item contracts.Envelope) error { return p.answers[item.Topic] }

func (p *answeringTopics) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

func (p *answeringTopics) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

// An event is settled once, so its failure is logged once. The owner here has every answer of its
// own in - one send denied, one reached - while another owner's unreachable NATS has stopped the
// batch, so the send its later event was owed is never made: that owner is left for the batch's
// end, where the connection decides what its sends came to, rather than settled now and settled
// again there.
func TestAnOwnerLeftForTheBatchsEndLogsItsFailureOnce(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "writer"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('T-2', 'TT', 2, 'The denied issue', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	// The owner whose unreachable send stops the batch is read first, so it is settled first.
	unreachable := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Unreachable"},
	})
	root := seedComment(t, database, "T-2", writer, "Draft", nil)
	denied := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: "reply", IssueKey: new("T-2"), Body: "Reviewed", ReplyTo: &root,
		}},
	})
	// T-2's own topic is owed this event once the denied event's copy of it is answered, and
	// that is the send the batch's stop leaves unmade.
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "c6f1bd4b-6d0e-4a53-9a2e-0c3d4fbb1b4a", IssueKey: new("T-2"), Body: "Next"},
	})

	publisher := &answeringTopics{answers: map[string]error{
		"notifications.dispatch.issue.T-1.message.created": fmt.Errorf("bus: %w", bus.ErrUnavailable),
		contracts.LegacyAgentSubject("writer"):             fmt.Errorf("bus: %w", bus.ErrPublishDenied),
	}}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}

	lines := 0
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: publish event") {
			lines++
			if !strings.Contains(line, fmt.Sprintf("event_id=%d ", denied.ID)) {
				t.Fatalf("the publish line %q names no denied event; event %d was denied", line, denied.ID)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d publish lines for one attempt at event %d, want one:\n%s", lines, denied.ID, logged.String())
	}
	if attempts, failure, _ := eventState(t, database, denied.ID); attempts != 1 || failure == nil {
		t.Fatalf("event %d has attempt_count %d and publish_error %v, want the one attempt its denial counts",
			denied.ID, attempts, failure)
	}
	if attempts, _, _ := eventState(t, database, unreachable.ID); attempts != 0 {
		t.Fatalf("event %d has attempt_count %d, want it released with no attempt counted", unreachable.ID, attempts)
	}
}

// A settle that waits on the comment owner's lock neither fails the scan nor the batch running
// beside it. mentionComment.settleAbandoned opens a transaction and takes that lock before any
// attempt row, so it waits for as long as whoever holds the lock takes; the batches of one scan
// share one context, and so one mark of the pool's one-connection-per-caller rule
// (store.ErrNestedAcquire), so a second batch reading while that transaction is open would be
// refused its connection - failing that batch, and with it the scan, which ends every batch still
// running - unless both run inside the scan's statement gate (lanes.statement). The other owner's
// event is published, and the settled mention's own event with it.
func TestASettleWaitingOnTheOwnerLockDoesNotFailTheBatchBesideIt(t *testing.T) {
	t.Parallel()
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	route := "session:s1"
	seedIssue(t, database, "T-1", &route)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Another owner', null, '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the other owner's issue: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, database, "T-1", alice, "@s1 are you there?", nil)
	mentioning := appendEvent(t, database, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
		},
	})
	seedMentionDelivery(t, database, commentID, "s1", "abandoned")

	// The comment owner's lock, held as the API's own claim of that attempt holds it, so the
	// outbox's settle of the abandoned attempt waits on it.
	claim, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the transaction holding the owner's lock: %v", err)
	}
	defer claim.Rollback(context.Background())
	if _, err := claim.Exec(context.Background(), `select 1 from issues where key = 'T-1' for no key update`); err != nil {
		t.Fatalf("take the owner's lock: %v", err)
	}

	settle := backendMatching("from issues where key = $1 for no key update")
	deps.Store = tracedStore(t, database, backendPIDTrace{settle})
	outbox := holding(t, deps)
	waitingAtGate := make(chan struct{}, 1)
	outbox.afterGateWait = notify(waitingAtGate)
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	ended := make(chan scanEnd, 1)
	go func() { ended <- outbox.scan(scanCtx, unfenced{}) }()
	settlePID := testwait.Until(t, "the backend taking the settle's owner lock", func() (uint32, bool) {
		pid := settle.pid.Load()
		return pid, pid != 0
	})
	storetest.WaitForQueuedBehind[scanEnd](t, claim, settlePID, nil)

	// Another owner's event, which a second batch of the same scan reads while the settle waits.
	otherComment := seedComment(t, database, "T-2", alice, "a comment of another owner", nil)
	other := appendEvent(t, database, deps.Broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: otherComment, IssueKey: new("T-2"), Body: "a comment of another owner"},
		},
	})
	outbox.signal()
	waitFor(t, "the second batch waiting at the scan's statement gate", func() bool {
		select {
		case <-waitingAtGate:
			return true
		case end := <-ended:
			t.Fatalf("the scan ended %+v before the second batch waited at the statement gate", end)
			return false
		default:
			return false
		}
	})
	if err := claim.Commit(context.Background()); err != nil {
		t.Fatalf("release the owner's lock: %v", err)
	}

	end := testwait.Receive(t, "the scan returning", ended)
	if end.failed || end.fenced || end.natsLost {
		t.Fatalf("the scan ended %+v, want it to end with nothing failed", end)
	}
	for _, event := range []model.Event{other, mentioning} {
		if publishedAt(t, database, event.ID) == nil {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("event %d was not published: attempt_count %d, publish_error %v", event.ID, attempts, failure)
		}
		if attempts, failure, _ := eventState(t, database, event.ID); attempts != 0 || failure != nil {
			t.Fatalf("event %d was published with attempt_count %d and publish_error %v, want neither", event.ID, attempts, failure)
		}
	}
}

// backendPID records the backend that starts a matching statement, including one in a batch.
type backendPID struct {
	matches func(string) bool
	pid     atomic.Uint32
}

func backendMatching(fragment string) *backendPID {
	return &backendPID{matches: func(sql string) bool { return strings.Contains(sql, fragment) }}
}

type backendPIDTrace []*backendPID

func (tr backendPIDTrace) record(conn *pgx.Conn, sql string) {
	for _, target := range tr {
		if target.matches(sql) {
			target.pid.Store(conn.PgConn().PID())
			return
		}
	}
}

func (tr backendPIDTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.record(conn, data.SQL)
	return ctx
}

func (backendPIDTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr backendPIDTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		tr.record(conn, query.SQL)
	}
	return ctx
}

func (backendPIDTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (backendPIDTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// A release blocks its batch whichever settle wrote it, and wakes nothing. An owner whose chains
// finish before the batch's end is settled, recorded and released there (settleOwner), and a
// release written there is the release the batch's final settle writes: nothing of the owner's
// events moved and they are due again at once. A batch that reported itself unblocked after such
// a settle, or that gave that owner's lane back with the wake an early release gives
// (lanes.releaseEarly), would have the scan read those events again at once, and again, as fast
// as the database answers, until the publisher hold lapsed - the spin this measures, one batch
// of which logs the released line.
//
// The staging is a due event held behind a blocker that names no retry, the shape holdBack
// releases on: the owner has no chain then, so it is settled before the window opens. Round 16's
// first fix closed the one route the outbox itself had to a release settled early - an event
// held by a blocker carrying itself, which the mention rule now leaves no copy to write
// (TestAWaitingRoleForwardHoldsNoCopyOfItsOwnEvent) - and what this covers is the batch's own
// result, which must carry a release whichever settle reached it.
func TestAnOwnerSettledEarlyOnAReleasedOutcomeBlocksItsBatch(t *testing.T) {
	const maxBatches = 3
	ctx := context.Background()
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	earlier := appendIssueMessage(t, database, broker)
	later := appendIssueMessage(t, database, broker)
	// The earlier event waits out a back-off far past this test, so every batch reads it as the
	// later event's blocker on their one shared destination key, the issue's topic family, and
	// sends nothing of it.
	backOffIn(t, database, earlier.ID, 3, time.Hour)

	wakes := 0
	deps := Deps{Store: database, Publisher: &recordingPublisher{}}
	// The lanes of a scan of one batch. Their wake is the scan's own, which a batch that came
	// back blocked must have left ungiven: the scan starts no batch of its own after one, and
	// the loop around it scans again at once on a wake (Outbox.scan, Outbox.serve).
	deps.lanes = newLanes(func() { wakes++ })
	holder := holding(t, deps).instance
	// stage reads the batch as the scan does and leaves its blocker naming no retry, which is
	// what has the later event settle released rather than blocked (holdBack).
	stage := func() []pendingEvent {
		t.Helper()
		pending, stopped, _, err := readBatch(ctx, deps)
		if err != nil {
			t.Fatalf("read the batch: %v", err)
		}
		if stopped {
			t.Fatal("the read stopped on a row it could not scan, which reports the batch blocked whatever its settles came to")
		}
		if len(pending) != 2 || pending[0].event.ID != earlier.ID || pending[1].event.ID != later.ID {
			t.Fatalf("the batch read %d events, want the blocker %d and the event %d held behind it",
				len(pending), earlier.ID, later.ID)
		}
		if pending[0].due || pending[0].nextAttempt == nil || !pending[1].due {
			t.Fatalf("event %d was read due = %v at %v and event %d due = %v, want a blocker that is not due and a due event behind it",
				earlier.ID, pending[0].due, pending[0].nextAttempt, later.ID, pending[1].due)
		}
		pending[0].nextAttempt = nil
		return pending
	}

	batches, blockedAt, woke := 0, 0, 0
	for range maxBatches {
		before := wakes
		result, err := publishBatch(ctx, deps, unfenced{}, holder, stage(), false)
		if err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		batches++
		if result.blocked && blockedAt == 0 {
			blockedAt, woke = batches, wakes-before
		}
		// A batch that blocked the scan without waking it is where the scan stops.
		if result.blocked && wakes == before {
			break
		}
	}

	// The staging settled the later event released and nothing else: no attempt counted, no
	// retry written, nothing sent, so it is due again at once.
	if row := readEventRow(t, database, later.ID); row.publishedAt != nil || row.attempts != 0 || row.nextAttempt != nil || len(row.destinations) != 0 {
		t.Fatalf("event %d has published_at %v, attempt_count %d, next_attempt_at %v and published_destinations %v, want it released: none, 0, none and none",
			later.ID, row.publishedAt, row.attempts, row.nextAttempt, row.destinations)
	}
	released := strings.Count(logged.String(), "NATS is unreachable")
	if blockedAt != 1 || woke != 0 {
		t.Fatalf("the batch settled owner T-1 early on a released outcome, blocked at batch %d and woke the scan %d times doing it; the scan it drives ran %d batches, logging %d released lines, want one batch, blocked, waking nothing. An owner settled early must carry its release into the batch's result and give its lane back as the batch's final settle does, so the scan backs off instead of reading the released events again at once",
			blockedAt, woke, batches, released)
	}
	if released != 1 {
		t.Fatalf("the one batch logged %d released lines, want one: a settle that released reports it once", released)
	}
}
