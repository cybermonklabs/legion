package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// tookHoldLine is the line publish logs as this process becomes the publisher.
const tookHoldLine = "dispatch outbox: took the publisher hold"

// sentBatchLine is the line scan logs for each batch that sent anything.
const sentBatchLine = "dispatch outbox: sent batch"

// lostHoldLine is the line publish logs when its turn as holder ended with the hold lost.
const lostHoldLine = "dispatch outbox: lost the publisher hold"

// The causes tryHold names on the late take-over's line, and endLateSessions on its failure line,
// written here as the words a reader of the log reads rather than taken from the code, so a change
// to them fails a row.
const (
	wantCauseTakeAfterDeadline = "the take came after the ending's deadline"
	wantCauseEndingTimedOut    = "the ending of its sessions did not answer by its deadline"
	wantCauseEndingFailed      = "the ending of its sessions failed"
	wantCauseOutboxStopped     = "the outbox stopped"
	wantCauseNotHolding        = "the publisher hold is no longer this process's"
	wantCauseFirstReads        = "the first batch's reads did not answer within their bound"
)

// standbyOf is a standby outbox of process publishing through publisher, built as cmd/dispatch
// builds a server's: on process's own outbox pool (store.Pool.Outbox), production's size, with its
// LISTEN on process's listener. A pool of any other size would let a row pass that production
// fails, so every standby of these rows is built here.
func standbyOf(t *testing.T, process *store.Store, publisher Publisher) *Outbox {
	t.Helper()
	pool, err := process.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the standby's outbox pool: %v", err)
	}
	return New(Deps{
		Store: &store.Store{Pool: pool}, Publisher: publisher,
		Broker: events.NewBroker(), Listen: process.Pool.Listener,
	})
}

// runStandby runs standby until t ends, and returns its stop and the channel Run's return closes.
func runStandby(t *testing.T, standby *Outbox) (stop context.CancelFunc, returned <-chan struct{}) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { standby.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel, done
}

// recordOf is the first record recorder kept whose message is message, and its index among the
// records, or false when it kept none yet.
func recordOf(recorder *logtest.Recorder, message string) (slog.Record, int, bool) {
	for index, record := range recorder.Records() {
		if record.Message == message {
			return record, index, true
		}
	}
	return slog.Record{}, -1, false
}

// stringAttribute is key's value on record, or "" when the record carries none.
func stringAttribute(record slog.Record, key string) string {
	value, found := attributeOf(record, key)
	if !found {
		return ""
	}
	return value.String()
}

// thirdSession is a connection of its own on database's database, named for an identity of no
// process, so no standby of these rows ends it: the rows read and write through it what no
// process of theirs would.
func thirdSession(t *testing.T, database *store.Store) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig(database.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["application_name"] = "dispatch " + uuid.NewString()
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a third session: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// A standby that takes over a publisher hold whose row lapsed ends every database session of the
// process the row named and logs each: a transaction that process held open is rolled back, so
// the lock it held is free, and its next statement answers SQLSTATE 57P01. Sessions of any other
// process are left alone, one whose name holds the stopped process's identity with one character
// more among them, and so are the standby's own. A row that lapses once the standby is up leaves
// the take all of lapseToServe, and the sessions end before the standby becomes the publisher. A
// row that lapsed longer ago than that leaves the take nothing of the six seconds: the standby
// logs its take-over late, with that cause, becomes the publisher at once, and ends the sessions
// beside its first batch, so after its took line.
func TestAStandbyThatReplacesALapsedHoldEndsThePreviousHoldersSessionsAlone(t *testing.T) {
	for _, row := range []struct {
		name string
		// lapsedAgo is how long before the standby starts the row lapsed, or zero for a row that
		// lapses once the standby listens (lapseOnceListening).
		lapsedAgo  time.Duration
		endedFirst bool
		lateCause  string
	}{
		{"a row that lapsed just now", 0, true, ""},
		{"a row that lapsed longer ago than the take-over leaves", 2 * lapseToServe, false, wantCauseTakeAfterDeadline},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := &logtest.Recorder{}
			logtest.SetDefault(t, recorder)
			ctx := context.Background()
			stopped := storetest.Open(t)
			identity := stopped.Pool.Identity()
			if row.lapsedAgo > 0 {
				if _, err := stopped.Pool.Exec(ctx, `insert into duty_holds (name, holder, token, expires_at) values ($1, $2, 'stopped', now() - make_interval(secs => $3))`,
					publisherHold, identity, row.lapsedAgo.Seconds()); err != nil {
					t.Fatalf("leave the stopped process's lapsed row: %v", err)
				}
			}
			tx, err := stopped.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			const key = int64(73548291)
			var holderPID int32
			if err := tx.QueryRow(ctx, `select pg_backend_pid() from (select pg_advisory_xact_lock($1)) locked`, key).Scan(&holderPID); err != nil {
				t.Fatalf("take a lock in the stopped process's transaction: %v", err)
			}
			planted := map[string]*pgx.Conn{}
			for _, name := range []string{"dispatch " + uuid.NewString(), "dispatch " + identity.String() + "0"} {
				config, err := pgx.ParseConfig(stopped.Pool.Config().ConnString())
				if err != nil {
					t.Fatal(err)
				}
				config.RuntimeParams["application_name"] = name
				conn, err := pgx.ConnectConfig(ctx, config)
				if err != nil {
					t.Fatalf("open the session %q: %v", name, err)
				}
				t.Cleanup(func() { _ = conn.Close(context.Background()) })
				planted[name] = conn
			}

			process := storetest.SecondProcess(t, stopped)
			standby := standbyOf(t, process, &recordingPublisher{})
			lapse := func() lapsed { return lapsed{} }
			if row.lapsedAgo == 0 {
				lapse = lapseOnceListening(t, stopped, standby)
			}
			runStandby(t, standby)
			lapse()
			waitFor(t, outboxHangGuard, "the standby ending the lapsed holder's sessions and becoming the publisher", func() bool {
				_, _, replaced := recordOf(recorder, replacedSessionsLine)
				_, _, took := recordOf(recorder, tookHoldLine)
				return replaced && took
			})

			var one int
			if err := tx.QueryRow(ctx, `select 1`).Scan(&one); !store.SessionEnded(err) {
				t.Errorf("the stopped process's transaction answered %v after the take, want SQLSTATE 57P01", err)
			}
			// The backend gives its locks up as it exits, a moment after pg_terminate_backend
			// signalled it, so the lock is tried until it is free, for as long as the test binary's
			// deadline leaves less the cleanup's stop of the standby, whose release of the hold
			// recordTimeout bounds.
			testwait.UntilDeadline(t, recordTimeout+time.Second, 50*time.Millisecond,
				fmt.Sprintf("backend %d, whose session the standby ended, to give up its advisory lock %d", holderPID, key), func() bool {
					var free bool
					if err := process.Pool.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, key).Scan(&free); err != nil {
						t.Fatalf("try the stopped process's lock: %v", err)
					}
					return free
				})
			endedHolder := false
			for _, record := range recorder.Records() {
				if record.Message == endedSessionLine && stringAttribute(record, "previous") == identity.String() && stringAttribute(record, "pid") == fmt.Sprint(holderPID) {
					endedHolder = true
				}
			}
			if !endedHolder {
				t.Errorf("the standby logged no end of the stopped process's transaction (backend %d): %q", holderPID, loggedMessages(recorder))
			}
			_, replacedAt, _ := recordOf(recorder, replacedSessionsLine)
			_, tookAt, _ := recordOf(recorder, tookHoldLine)
			if endedFirst := replacedAt < tookAt; endedFirst != row.endedFirst {
				t.Errorf("the sessions were ended before the standby became the publisher: %t, want %t: %q", endedFirst, row.endedFirst, loggedMessages(recorder))
			}
			late, _, _ := recordOf(recorder, lateTakeOverLine)
			if cause := stringAttribute(late, "cause"); cause != row.lateCause {
				t.Errorf("the standby logged its take-over late with the cause %q, want %q: %q", cause, row.lateCause, loggedMessages(recorder))
			}
			for name, conn := range planted {
				if err := conn.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
					t.Errorf("the session %q of another process no longer answers: %v", name, err)
				}
			}
			if err := standby.deps.Store.Pool.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
				t.Errorf("the standby's own outbox pool no longer answers: %v", err)
			}
			if !standby.Publishing() {
				t.Error("the standby stopped publishing")
			}
		})
	}
}

// lapsed is when the lapse lapseOnceListening made fell, bracketed on this process's clock: the
// lapse is the statement's now() on the database's clock, which came after before and before
// after. A holder stopped at the renewal that left that row has its take-over due lapseToServe
// after the lapse, so no later than after plus lapseToServe on this process's clock.
type lapsed struct {
	before, after time.Time
}

// takeOverDue is the latest moment the take-over of a holder stopped at the renewal that left
// the lapsed row may begin to publish by, on this process's clock: takeOverWithin after that
// renewal, which is lapseToServe after the lapse.
func (l lapsed) takeOverDue() time.Time { return l.after.Add(lapseToServe) }

// lapseOnceListening leaves a row of the publisher hold for the stopped process that lasts an
// hour, and returns the call that lapses it once standby's LISTEN connection is up and wakes the
// standby as a release would (duty.HoldReleasedChannel), and that says when the lapse fell. The
// standby then takes the row at its lapse, so what the take leaves of lapseToServe does not depend
// on how long the standby took to start, which a loaded runner can stretch past lapseToServe. It
// must be called before the standby's Run starts, and the call it returns after.
//
// The lapse and the wake are one statement, sent on a session of its own (thirdSession), whose
// name carries an identity of no process. As two, a standby that tried the hold between them took
// the lapsed row and ended the stopped process's sessions, the one about to send the wake among
// them, which then answered 57P01 (CI run 44762226200). In one, the take sees the lapse only once
// the statement has committed, the wake with it; on a session of its own, a standby whose take
// came while that statement was still answering ends none of it either.
func lapseOnceListening(t *testing.T, stopped *store.Store, standby *Outbox) func() lapsed {
	t.Helper()
	ctx := context.Background()
	conn := thirdSession(t, stopped)
	if _, err := conn.Exec(ctx, `insert into duty_holds (name, holder, token, expires_at) values ($1, $2, 'stopped', now() + interval '1 hour')`,
		publisherHold, stopped.Pool.Identity()); err != nil {
		t.Fatalf("leave the stopped process's row: %v", err)
	}
	listening := make(chan struct{})
	var once sync.Once
	standby.afterListening = func() { once.Do(func() { close(listening) }) }
	return func() lapsed {
		t.Helper()
		select {
		case <-listening:
		case <-time.After(outboxHangGuard):
			t.Fatalf("the standby's LISTEN connection did not come up within %v", outboxHangGuard)
		}
		before := time.Now()
		if _, err := conn.Exec(ctx, `
			with lapsed as (update duty_holds set expires_at = now() where name = $1 returning name)
			select pg_notify($2, name) from lapsed`, publisherHold, duty.HoldReleasedChannel); err != nil {
			t.Fatalf("lapse the stopped process's row and wake the standby: %v", err)
		}
		return lapsed{before: before, after: time.Now()}
	}
}

// heldEnding holds the statements that end sessions (store.Pool.EndSessionsOf), as a statement
// queued behind a slow database would, and records when each began, the deadline its context
// carried, and how it ended; begun counts the statements it has begun to hold. Each is held for
// hold, or until its context ends, whichever comes first, unless one of these says otherwise:
//   - answerBefore: the first is held until that long before its context's deadline, and then
//     sent, as an ending the database answers just in time;
//   - failFirst: the first meets its connection broken, as a statement whose connection to the
//     database broke does;
//   - letGo: from the second on, each is held until letGo closes, past its context's end too, as
//     a statement whose client has not yet returned.
//
// It also holds the first batch's read of the events (readBatchSQL) for readHold, or, with
// readUntilLetGo, until letGo closes, as a scan the database keeps busy.
type heldEnding struct {
	hold           time.Duration
	answerBefore   time.Duration
	failFirst      bool
	letGo          <-chan struct{}
	readHold       time.Duration
	readUntilLetGo bool
	// afterFirst, when set, is called as the first ending is let go.
	afterFirst func()

	begun    atomic.Int32
	readOnce sync.Once
	mu       sync.Mutex
	held     []heldStatement
}

// heldStatement is one statement heldEnding held. cut is its context's error when the context had
// ended by the time the statement was let go, and nil when it had not.
type heldStatement struct {
	began, ended, deadline time.Time
	cut                    error
}

func (s heldStatement) String() string {
	deadline := "none"
	if !s.deadline.IsZero() {
		deadline = s.deadline.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("{began %s, ended %s, deadline %s, cut %v}",
		s.began.Format(time.RFC3339Nano), s.ended.Format(time.RFC3339Nano), deadline, s.cut)
}

func (h *heldEnding) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == readBatchSQL && (h.readHold > 0 || h.readUntilLetGo) {
		h.readOnce.Do(func() {
			if h.readUntilLetGo {
				<-h.letGo
				return
			}
			time.Sleep(h.readHold)
		})
		return ctx
	}
	if !strings.Contains(data.SQL, "pg_terminate_backend") {
		return ctx
	}
	deadline, _ := ctx.Deadline()
	began := time.Now()
	n := h.begun.Add(1)
	switch {
	case n == 1 && h.failFirst:
		_ = conn.PgConn().Conn().Close()
	case n == 1 && h.answerBefore > 0 && !deadline.IsZero():
		time.Sleep(time.Until(deadline.Add(-h.answerBefore)))
		if h.afterFirst != nil {
			h.afterFirst()
		}
	case n >= 2 && h.letGo != nil:
		<-h.letGo
	default:
		select {
		case <-ctx.Done():
		case <-time.After(h.hold):
		}
	}
	h.mu.Lock()
	h.held = append(h.held, heldStatement{began: began, ended: time.Now(), deadline: deadline, cut: ctx.Err()})
	h.mu.Unlock()
	return ctx
}

func (h *heldEnding) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (h *heldEnding) statements() []heldStatement {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]heldStatement(nil), h.held...)
}

// slowEndingRig is a stopped process with one event waiting to be published, and a standby on a
// store of its own traced by ending, taking over at a lapse that falls once it listens.
type slowEndingRig struct {
	recorder *logtest.Recorder
	stopped  *store.Store
	standby  *Outbox
	stop     context.CancelFunc
	returned <-chan struct{}
	lapsed   lapsed
}

func startSlowEnding(t *testing.T, ending *heldEnding) *slowEndingRig {
	t.Helper()
	return startSlowEndingWith(t, ending, &recordingPublisher{})
}

// startSlowEndingWith is startSlowEnding with the standby publishing through publisher.
func startSlowEndingWith(t *testing.T, ending *heldEnding, publisher Publisher) *slowEndingRig {
	t.Helper()
	rig := &slowEndingRig{recorder: &logtest.Recorder{}}
	logtest.SetDefault(t, rig.recorder)
	rig.stopped = storetest.Open(t)
	seedIssue(t, rig.stopped, "T-1", nil)
	appendEvent(t, rig.stopped, events.NewBroker(), model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "message-waiting", IssueKey: new("T-1"), Body: "waiting for the new publisher"},
	})
	rig.standby = standbyOf(t, tracedStore(t, storetest.SecondProcess(t, rig.stopped), ending), publisher)
	lapse := lapseOnceListening(t, rig.stopped, rig.standby)
	rig.stop, rig.returned = runStandby(t, rig.standby)
	rig.lapsed = lapse()
	return rig
}

// firstBatchStarted is the started attribute of the first sent batch line, failing t when there
// is none.
func (r *slowEndingRig) firstBatchStarted(t *testing.T) time.Time {
	t.Helper()
	batch, _, found := recordOf(r.recorder, sentBatchLine)
	if !found {
		t.Fatalf("the standby logged no sent batch: %q", loggedMessages(r.recorder))
	}
	return momentOf(t, batch, "started")
}

// requireFirstBatchWithinTheTakeOver fails t when the standby's first batch started after the
// take-over was due (lapsed.takeOverDue): past takeOverWithin of a stop at the renewal that left
// the lapsed row, on the database's clock.
func (r *slowEndingRig) requireFirstBatchWithinTheTakeOver(t *testing.T, what string) {
	t.Helper()
	started := r.firstBatchStarted(t)
	due := r.lapsed.takeOverDue()
	t.Logf("%s: the first batch started %v to %v after the row lapsed on the database's clock (the lapse statement answered within %v), %v before the take-over was due",
		what, started.Sub(r.lapsed.after).Round(time.Millisecond), started.Sub(r.lapsed.before).Round(time.Millisecond),
		r.lapsed.after.Sub(r.lapsed.before).Round(time.Millisecond), due.Sub(started).Round(time.Millisecond))
	if started.After(due) {
		t.Errorf("%s: the standby's first batch started at least %v after the row lapsed on the database's clock, %v past lapseToServe: a holder stopped at the renewal that left the row is replaced past takeOverWithin (%v)",
			what, started.Sub(r.lapsed.after).Round(time.Millisecond), started.Sub(due).Round(time.Millisecond), takeOverWithin)
	}
}

// A standby whose ending of the replaced holder's sessions does not answer gives it what the
// take-over's budget leaves once the first batch's share is kept - the budget, lapseToServe after
// the row's lapse, less firstBatchShare - and takes over at that deadline anyway: it logs the late
// take-over by name, with its cause, then stores the hold, logs that it took it, and starts its
// first batch within the share it kept. The sessions it could not end are ended beside that
// batch, which their ending holds none of, under recordTimeout. The ending is held 3 s, longer
// than lapseToServe and shorter than recordTimeout, and one event waits to be published.
//
// Each bound is read in the statement, so none needs a margin: the first try's context carries
// the ending's deadline itself, and the second's carries recordTimeout from no earlier than its
// start. The took line's start and the first batch's start must follow the first try's end, and
// the first batch must start by the time the take-over was due on the database's clock, so a
// standby that waits after the cut, as one that waits before it, fails. The batch must start
// before the second try waits its 3 s out and ends the sessions, which it logs by name after the
// took line.
func TestAStandbyWhoseEndingOfTheOldSessionsIsSlowPublishesWithinWhatTheTakeOverLeft(t *testing.T) {
	slow := &heldEnding{hold: 3 * time.Second}
	rig := startSlowEnding(t, slow)
	waitFor(t, outboxHangGuard, "the standby ending the lapsed holder's sessions after its late take-over and sending its first batch", func() bool {
		_, _, ended := recordOf(rig.recorder, lateEndingDoneLine)
		_, _, sent := recordOf(rig.recorder, sentBatchLine)
		return ended && sent
	})
	hold := rig.standby.hold.Load()
	if hold == nil {
		t.Fatal("the standby holds no publisher hold")
	}
	budget := hold.ReplacedLapse().Add(lapseToServe)
	deadline := budget.Add(-firstBatchShare)
	held := slow.statements()
	if len(held) != 2 {
		t.Fatalf("the standby ended the sessions in %d statements (%+v), want a first try cut at its deadline and a second that waited the hold out: %q", len(held), held, loggedMessages(rig.recorder))
	}
	first, second := held[0], held[1]
	t.Logf("the ending statements: %+v; the ending's deadline fell %v after the first began", held, deadline.Sub(first.began).Round(time.Millisecond))
	if !first.deadline.Equal(deadline) {
		t.Errorf("the first try was given until %s, %v from the ending's deadline %s (ReplacedLapse + lapseToServe - firstBatchShare), want that deadline itself",
			first.deadline.Format(time.RFC3339Nano), first.deadline.Sub(deadline), deadline.Format(time.RFC3339Nano))
	}
	if !errors.Is(first.cut, context.DeadlineExceeded) {
		t.Errorf("the first try ended with %v, want cut by its deadline", first.cut)
	}
	if second.cut != nil {
		t.Errorf("the second try was cut (%v), want it to wait the 3 s hold out", second.cut)
	}
	if second.deadline.IsZero() || second.deadline.After(second.began.Add(recordTimeout)) {
		t.Errorf("the second try was given until %s, %v after it began, want no later than recordTimeout (%v) after",
			second.deadline.Format(time.RFC3339Nano), second.deadline.Sub(second.began), recordTimeout)
	}

	late, lateAt, _ := recordOf(rig.recorder, lateTakeOverLine)
	took, tookAt, _ := recordOf(rig.recorder, tookHoldLine)
	if lateAt < 0 || tookAt < 0 || lateAt > tookAt {
		t.Errorf("the late take-over's line came at %d and the took line at %d, want the late line logged, before the took line: %q", lateAt, tookAt, loggedMessages(rig.recorder))
	}
	if cause := stringAttribute(late, "cause"); cause != wantCauseEndingTimedOut {
		t.Errorf("the late take-over's cause is %q, want %q", cause, wantCauseEndingTimedOut)
	}
	started := momentOf(t, took, "started")
	if !started.After(first.ended) {
		t.Errorf("the standby began to publish at %s, %v before its bounded ending ended at %s",
			started.Format(time.RFC3339Nano), first.ended.Sub(started), first.ended.Format(time.RFC3339Nano))
	}
	batchStarted := rig.firstBatchStarted(t)
	t.Logf("the first try ended %v past its deadline; the standby began to publish %v after it, and its first batch started %v after that",
		first.ended.Sub(deadline).Round(time.Millisecond), started.Sub(first.ended).Round(time.Millisecond), batchStarted.Sub(started).Round(time.Millisecond))
	rig.requireFirstBatchWithinTheTakeOver(t, "the ending cut at its deadline")
	if !batchStarted.Before(second.ended) {
		t.Errorf("the first batch started at %s, %v after the took line and not before the ending beside it answered at %s: the ending held the batch back",
			batchStarted.Format(time.RFC3339Nano), batchStarted.Sub(started).Round(time.Millisecond), second.ended.Format(time.RFC3339Nano))
	}
	if _, doneAt, _ := recordOf(rig.recorder, lateEndingDoneLine); doneAt < tookAt {
		t.Errorf("the late ending's done line came at %d, before the took line at %d", doneAt, tookAt)
	}
	if failed, _, found := recordOf(rig.recorder, lateEndingFailedLine); found {
		t.Errorf("the late ending logged a failure beside its done line: cause %q", stringAttribute(failed, "cause"))
	}
}

// slowStatusPublisher is a recordingPublisher whose status read, once armed, answers late once,
// after hold, as a client whose lock nats.go holds a while does.
type slowStatusPublisher struct {
	recordingPublisher
	hold  time.Duration
	armed atomic.Bool
}

func (p *slowStatusPublisher) Connected() bool {
	if p.armed.CompareAndSwap(true, false) {
		time.Sleep(p.hold)
	}
	return true
}

// A standby whose ending of the replaced holder's sessions answers just before its deadline
// takes over on time, and what runs between that ending and its first batch's first send is
// bounded, so the first batch starts by the time the take-over was due: the ending's deadline
// leaves firstBatchShare, the sum of serve's status read's bound (statusReadBound) and the first
// scan's reads' (firstBatchReadBound). In the first row the status read and the first batch's
// read of the events each take their bound less 100 ms, and the batch starts within the six
// seconds on the database's clock. In the second the read takes 100 ms more than the whole share,
// so it would end past the take-over's budget, which bounds the first batch's reads: the read
// is cut, the take-over is logged late with that cause, and the next scan, with no bound,
// publishes the event at once.
func TestAStandbyWhoseEndingAnswersJustBeforeItsDeadlineStartsItsFirstBatchWithinTheSixSeconds(t *testing.T) {
	for _, row := range []struct {
		name     string
		readHold time.Duration
		late     bool
	}{
		{"the status read and the first reads take nearly their bounds", firstBatchReadBound - 100*time.Millisecond, false},
		{"the first reads run past the take-over's budget", firstBatchShare + 100*time.Millisecond, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			publisher := &slowStatusPublisher{hold: statusReadBound - 100*time.Millisecond}
			ending := &heldEnding{answerBefore: 100 * time.Millisecond, readHold: row.readHold, afterFirst: func() { publisher.armed.Store(true) }}
			rig := startSlowEndingWith(t, ending, publisher)
			waitFor(t, outboxHangGuard, "the standby's first sent batch", func() bool {
				_, _, sent := recordOf(rig.recorder, sentBatchLine)
				return sent
			})
			_, replacedAt, _ := recordOf(rig.recorder, replacedSessionsLine)
			_, tookAt, _ := recordOf(rig.recorder, tookHoldLine)
			if replacedAt < 0 || replacedAt > tookAt {
				t.Errorf("the replaced line came at %d and the took line at %d, want the sessions ended before the standby became the publisher: %q", replacedAt, tookAt, loggedMessages(rig.recorder))
			}
			if held := ending.statements(); len(held) != 1 || held[0].cut != nil {
				t.Errorf("the ending statements were %+v, want one, answered before its deadline", held)
			}
			if _, _, failed := recordOf(rig.recorder, "dispatch outbox: scan"); failed {
				t.Errorf("the standby logged a failed scan: %q", loggedMessages(rig.recorder))
			}
			late, _, found := recordOf(rig.recorder, lateTakeOverLine)
			if !row.late {
				if found {
					t.Errorf("the standby logged a late take-over with the cause %q, want none", stringAttribute(late, "cause"))
				}
				rig.requireFirstBatchWithinTheTakeOver(t, "the status read and the first reads 100 ms short of their bounds")
				return
			}
			if cause := stringAttribute(late, "cause"); cause != wantCauseFirstReads {
				t.Errorf("the late take-over's cause is %q, want %q: %q", cause, wantCauseFirstReads, loggedMessages(rig.recorder))
			}
			t.Logf("the first batch, after the cut read, started %v after the row lapsed", rig.firstBatchStarted(t).Sub(rig.lapsed.after).Round(time.Millisecond))
		})
	}
}

// A standby whose ending of the replaced holder's sessions fails - the statement's connection to
// the database breaks - takes over late with that cause, and ends the sessions beside its first
// batch, which it logs by name.
func TestAStandbyWhoseEndingOfTheOldSessionsFailsTakesOverLateAndSaysWhy(t *testing.T) {
	ending := &heldEnding{failFirst: true}
	rig := startSlowEnding(t, ending)
	waitFor(t, outboxHangGuard, "the standby's late ending after its first ending failed", func() bool {
		_, _, done := recordOf(rig.recorder, lateEndingDoneLine)
		_, _, failed := recordOf(rig.recorder, lateEndingFailedLine)
		return done || failed
	})
	late, lateAt, _ := recordOf(rig.recorder, lateTakeOverLine)
	_, tookAt, _ := recordOf(rig.recorder, tookHoldLine)
	_, doneAt, _ := recordOf(rig.recorder, lateEndingDoneLine)
	if lateAt < 0 || tookAt < lateAt || doneAt < tookAt {
		t.Errorf("the late line came at %d, the took line at %d and the late ending's done line at %d, want them in that order: %q", lateAt, tookAt, doneAt, loggedMessages(rig.recorder))
	}
	if cause := stringAttribute(late, "cause"); cause != wantCauseEndingFailed {
		t.Errorf("the late take-over's cause is %q, want %q", cause, wantCauseEndingFailed)
	}
	if held := ending.statements(); len(held) != 2 {
		t.Errorf("the ending statements were %+v, want the failed try and the late ending", held)
	}
}

// The ending a late take-over leaves beside the first batch ends when the standby stops, and says
// so by name, and the standby's Run returns only once that ending has: publish waits for it
// before it releases the hold. The late ending is held past its context's end, as a statement
// whose client has not returned yet: for recordTimeout, the most the release that follows takes,
// Run must not return and the hold must stay unreleased. Once let go, the ending logs that it did
// not end the previous holder's sessions, with the cause "the outbox stopped", and Run returns
// after it.
func TestALateTakeOversEndingStopsWithTheStandbyAndSaysSo(t *testing.T) {
	letGo := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(letGo) }) }
	held := &heldEnding{hold: time.Hour, letGo: letGo}
	rig := startSlowEnding(t, held)
	// After the standby's own cleanup is registered, so it runs first: a row that fails while the
	// ending is held lets it go before the cleanup waits for Run.
	t.Cleanup(release)
	waitFor(t, outboxHangGuard, "the standby's late take-over and the ending beside its first batch", func() bool {
		_, _, took := recordOf(rig.recorder, tookHoldLine)
		return took && held.begun.Load() == 2
	})
	third := thirdSession(t, rig.stopped)
	stopping := time.Now()
	rig.stop()
	select {
	case <-rig.returned:
		t.Fatalf("the standby's Run returned %v after its stop while the late ending it started was still held: publish must wait for that ending before it releases the hold",
			time.Since(stopping).Round(time.Millisecond))
	case <-time.After(recordTimeout):
	}
	var holder string
	if err := third.QueryRow(context.Background(), `select holder::text from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil || holder != rig.standby.instance.String() {
		t.Errorf("with the late ending still held, the hold's row names %q (%v), want the standby %s: the hold was released while its ending was in flight", holder, err, rig.standby.instance)
	}
	release()
	var returned time.Time
	select {
	case <-rig.returned:
		returned = time.Now()
	case <-time.After(outboxHangGuard):
		t.Fatalf("the standby's Run did not return within %v of its late ending being let go", outboxHangGuard)
	}
	statements := held.statements()
	t.Logf("Run returned %v after the stop; the ending statements: %+v", returned.Sub(stopping).Round(time.Millisecond), statements)
	if len(statements) != 2 {
		t.Fatalf("the standby ended the sessions in %d statements, want the bounded try and the late ending: %q", len(statements), loggedMessages(rig.recorder))
	}
	if !errors.Is(statements[1].cut, context.Canceled) {
		t.Errorf("the late ending ended with %v, want cut by the standby's stop", statements[1].cut)
	}
	if !statements[1].ended.Before(returned) {
		t.Errorf("the late ending ended at %s, after Run returned at %s", statements[1].ended.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano))
	}
	failed, _, _ := recordOf(rig.recorder, lateEndingFailedLine)
	if cause := stringAttribute(failed, "cause"); cause != wantCauseOutboxStopped {
		t.Errorf("the late ending's failure line names the cause %q, want %q: %q", cause, wantCauseOutboxStopped, loggedMessages(rig.recorder))
	}
	if _, _, found := recordOf(rig.recorder, lateEndingDoneLine); found {
		t.Error("the late ending logged that it ended the sessions, though the stop cut it")
	}
}

// cuttablePublisher is a recordingPublisher whose NATS connection a row can cut: once cut, it
// reads as not connected, as a client whose connection dropped does.
type cuttablePublisher struct {
	recordingPublisher
	cut atomic.Bool
}

func (p *cuttablePublisher) Connected() bool { return !p.cut.Load() }

// A late take-over's ending beside the first batch makes no ending once this process is no
// longer the holder: the previous holder, which was only slow, may take the hold back, and its
// sessions are then no stopped process's to end. In each row the late ending is held, as a
// statement whose client has not returned, until this process has stopped holding - its hold
// found lost, with its first scan still kept busy by a read the database holds, or the hold given
// up for NATS lost - and then let go. The previous holder's transaction still answers and its
// lock stands, and the ending logs that it did not end the sessions, with why.
func TestALateTakeOversEndingEndsNoSessionOnceThisProcessStopsHolding(t *testing.T) {
	for _, row := range []struct {
		name string
		// readHeld keeps the first scan's read of the events held until the ending is let go, so
		// serve has not returned when the hold is found lost.
		readHeld bool
		// stopHolding makes this process stop holding, and waits until it has.
		stopHolding func(t *testing.T, rig *slowEndingRig, third *pgx.Conn, publisher *cuttablePublisher)
		cause       string
	}{
		{"the hold moves back to the previous holder while the first scan is busy", true,
			func(t *testing.T, rig *slowEndingRig, third *pgx.Conn, _ *cuttablePublisher) {
				if _, err := third.Exec(context.Background(), `update duty_holds set holder = $2, token = 'took it back', expires_at = now() + interval '1 hour' where name = $1`,
					publisherHold, rig.stopped.Pool.Identity()); err != nil {
					t.Fatalf("move the hold back to the previous holder: %v", err)
				}
				waitFor(t, outboxHangGuard, "the standby finding its hold lost", func() bool { return !rig.standby.Publishing() })
			}, wantCauseNotHolding},
		{"this process gives the hold up for NATS lost", false,
			func(t *testing.T, rig *slowEndingRig, _ *pgx.Conn, publisher *cuttablePublisher) {
				publisher.cut.Store(true)
				rig.standby.signal()
				waitFor(t, outboxHangGuard, "the standby giving the hold up for NATS lost", func() bool {
					released, _, found := recordOf(rig.recorder, "dispatch outbox: released the publisher hold")
					return found && stringAttribute(released, "reason") == string(natsLost)
				})
			}, "this process gave the publisher hold up"},
	} {
		t.Run(row.name, func(t *testing.T) {
			letGo := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(letGo) }) }
			held := &heldEnding{hold: time.Hour, letGo: letGo, readUntilLetGo: row.readHeld}
			publisher := &cuttablePublisher{}
			rig := startSlowEndingWith(t, held, publisher)
			t.Cleanup(release)
			ctx := context.Background()
			tx, err := rig.stopped.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			const key = int64(73548293)
			if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, key); err != nil {
				t.Fatalf("take a lock in the previous holder's transaction: %v", err)
			}
			waitFor(t, outboxHangGuard, "the standby's late take-over and the ending beside its first batch", func() bool {
				_, _, took := recordOf(rig.recorder, tookHoldLine)
				return took && held.begun.Load() == 2
			})
			third := thirdSession(t, rig.stopped)
			row.stopHolding(t, rig, third, publisher)
			release()
			waitFor(t, outboxHangGuard, "the late ending's end", func() bool {
				_, _, done := recordOf(rig.recorder, lateEndingDoneLine)
				_, _, failed := recordOf(rig.recorder, lateEndingFailedLine)
				return done || failed
			})
			var one int
			if err := tx.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
				t.Errorf("the previous holder's transaction answered %v once the late ending was let go, want it left alone", err)
			}
			var free bool
			if err := third.QueryRow(ctx, `select pg_try_advisory_lock($1)`, key).Scan(&free); err != nil || free {
				t.Errorf("the previous holder's lock was free (%t, %v), want it still held by its transaction", free, err)
			}
			if _, _, found := recordOf(rig.recorder, lateEndingDoneLine); found {
				t.Error("the late ending logged that it ended the previous holder's sessions after this process stopped holding")
			}
			failed, _, _ := recordOf(rig.recorder, lateEndingFailedLine)
			if cause := stringAttribute(failed, "cause"); cause != row.cause {
				t.Errorf("the late ending's failure line names the cause %q, want %q: %q", cause, row.cause, loggedMessages(rig.recorder))
			}
			if statements := held.statements(); len(statements) != 2 || statements[1].cut == nil {
				t.Errorf("the ending statements were %+v, want the late ending cut by its context before it was let go", statements)
			}
		})
	}
}

// An ending of the previous holder's sessions under a hold that is no longer this process's makes
// no statement: endReplacedSessions checks the hold before each ending, and answers why it made
// none. The hold here was released before the ending, which closes its Lost.
func TestAnEndingUnderAHoldNoLongerThisProcesssEndsNothing(t *testing.T) {
	database := storetest.Open(t)
	previous := uuid.New()
	config, err := pgx.ParseConfig(database.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["application_name"] = "dispatch " + previous.String()
	session, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a session of the previous holder: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	tracer := &statementTracer{}
	outbox := New(Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}})
	hold, _ := outbox.tryHold(context.Background())
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	if err := hold.Release(context.Background()); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	err = outbox.endReplacedSessions(context.Background(), hold, previous, time.Now().Add(recordTimeout))
	if !errors.Is(err, errNotHolding) {
		t.Errorf("the ending under a released hold answered %v, want %v", err, errNotHolding)
	}
	if sent := tracer.matching("pg_terminate_backend"); len(sent) != 0 {
		t.Errorf("the ending under a released hold sent %d statements that end sessions, want none", len(sent))
	}
	var one int
	if err := session.QueryRow(context.Background(), `select 1`).Scan(&one); err != nil {
		t.Errorf("the previous holder's session answered %v, want it left alone", err)
	}
}

// A standby stopped while it ends the replaced holder's sessions, before it publishes, logs a stop
// and not a late take-over: it names the stop (untakenLine), logs no late take-over and no took
// line, releases the hold and returns.
func TestAStopDuringTheEndingOfTheOldSessionsIsLoggedAsAStop(t *testing.T) {
	held := &heldEnding{hold: time.Hour}
	rig := startSlowEnding(t, held)
	waitFor(t, outboxHangGuard, "the standby's ending of the replaced holder's sessions", func() bool { return held.begun.Load() == 1 })
	rig.stop()
	select {
	case <-rig.returned:
	case <-time.After(outboxHangGuard):
		t.Fatalf("the standby's Run did not return within %v of its stop", outboxHangGuard)
	}
	if _, _, found := recordOf(rig.recorder, untakenLine); !found {
		t.Errorf("the standby did not log its stopped take-over (%q): %q", untakenLine, loggedMessages(rig.recorder))
	}
	if late, _, found := recordOf(rig.recorder, lateTakeOverLine); found {
		t.Errorf("the standby logged its stop as a late take-over with the cause %q", stringAttribute(late, "cause"))
	}
	if _, _, found := recordOf(rig.recorder, tookHoldLine); found {
		t.Error("the standby logged that it took the publisher hold, though it stopped before publishing")
	}
	if statements := held.statements(); len(statements) != 1 || !errors.Is(statements[0].cut, context.Canceled) {
		t.Errorf("the ending statements were %+v, want the one try, cut by the stop", statements)
	}
	var rows int
	if err := thirdSession(t, rig.stopped).QueryRow(context.Background(), `select count(*) from duty_holds where name = $1 and holder = $2`,
		publisherHold, rig.standby.instance).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("the standby's row of the hold is there %d times (%v) once it stopped, want it released", rows, err)
	}
}

// heldRecord holds the statements that end sessions until their context ends, when ending is
// set, and the first batch's record (recordBatch's update of events) for record or until its
// context ends, and sends the moment it began holding that record on recordAt.
type heldRecord struct {
	ending   bool
	record   time.Duration
	once     sync.Once
	recordAt chan time.Time
}

func (h *heldRecord) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case h.ending && strings.Contains(data.SQL, "pg_terminate_backend"):
		<-ctx.Done()
	case strings.Contains(data.SQL, "published_destinations = e.published_destinations"):
		first := false
		h.once.Do(func() { first = true })
		if first {
			h.recordAt <- time.Now()
			select {
			case <-ctx.Done():
			case <-time.After(h.record):
			}
		}
	}
	return ctx
}

func (h *heldRecord) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A holder whose late take-over's ending beside its first batch does not answer, and whose first
// batch's record the database holds 4.5 s - each within recordTimeout, the bound both are given -
// keeps the hold: the ending, the batch's statement and the hold's renewal each take a connection
// of production's outbox pool (store's outboxPoolSize) at once, so the renewal waits behind
// neither, and the batch is recorded published. The second row, whose ending answers, is the same
// with no late take-over: its first batch's record ends past the take-over's budget, and the reads
// the scan makes past that budget - of the batch it starts next, or of one a wake started beside it,
// which waits for the record's statement - are not cut, since a batch had its reads answered by
// the budget (scan).
func TestAHolderKeepsItsHoldWhileALateEndingAndASlowFirstRecordShareTheOutboxPool(t *testing.T) {
	for _, row := range []struct {
		name   string
		ending bool
	}{
		{"the late ending does not answer", true},
		{"the ending answers", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := &logtest.Recorder{}
			logtest.SetDefault(t, recorder)
			stopped := storetest.Open(t)
			seedIssue(t, stopped, "T-1", nil)
			appendEvent(t, stopped, events.NewBroker(), model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.Message{ID: "message-waiting", IssueKey: new("T-1"), Body: "waiting for the new publisher"},
			})
			tracer := &heldRecord{ending: row.ending, record: 4500 * time.Millisecond, recordAt: make(chan time.Time, 1)}
			standby := standbyOf(t, tracedStore(t, storetest.SecondProcess(t, stopped), tracer), &recordingPublisher{})
			t.Logf("the standby's outbox pool holds at most %d connections", standby.deps.Store.Pool.Config().MaxConns)
			lapse := lapseOnceListening(t, stopped, standby)
			runStandby(t, standby)
			lapse()
			var recordAt time.Time
			select {
			case recordAt = <-tracer.recordAt:
			case <-time.After(outboxHangGuard):
				t.Fatalf("the first batch never reached its record: %q", loggedMessages(recorder))
			}
			// The batch's line comes once its record answers; a hold lost meanwhile is logged once
			// the scan returns, a moment later, so the row reads both half a second after it.
			time.Sleep(time.Until(recordAt.Add(tracer.record + 500*time.Millisecond)))
			waitFor(t, outboxHangGuard, "the first batch's line", func() bool {
				_, _, sent := recordOf(recorder, sentBatchLine)
				return sent
			})
			lateRecord, _, late := recordOf(recorder, lateTakeOverLine)
			_, _, lost := recordOf(recorder, lostHoldLine)
			batch, _, _ := recordOf(recorder, sentBatchLine)
			t.Logf("late take-over logged %t; publishing %t; lost line %t; the batch sent %s and published %s, %v after its record began",
				late, standby.Publishing(), lost, stringAttribute(batch, "sent"), stringAttribute(batch, "published"), time.Since(recordAt).Round(time.Millisecond))
			if late != row.ending {
				t.Fatalf("a late take-over was logged: %t, want %t (cause %q, budget %s, past_budget %s, past_deadline %s, error %q): %q", late, row.ending,
					stringAttribute(lateRecord, "cause"), stringAttribute(lateRecord, "budget"), stringAttribute(lateRecord, "past_budget"),
					stringAttribute(lateRecord, "past_deadline"), stringAttribute(lateRecord, "error"), loggedMessages(recorder))
			}
			if lost || !standby.Publishing() {
				t.Errorf("the holder, still running, lost the publisher hold while its first batch's record was held 4.5 s (publishing %t, lost line %t): %q", standby.Publishing(), lost, loggedMessages(recorder))
			}
			if published := stringAttribute(batch, "published"); published != "1" {
				t.Errorf("the first batch published %s events, want the one waiting", published)
			}
		})
	}
}
