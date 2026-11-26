package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// These tests make a write that keeps the daemon's record of a paced wake true fail at the real
// database, on the daemon's own asynchronous path, and read what its retry (writeAgainUntilMade)
// leaves behind: the stop and the start of a resume's stuck clock (projectPause.writeResumeClock),
// and the let-out of a held catch-up (catchUpPacing.letOut).

// testWriteRetry is the wait these tests give the retry of a failed wake write in place of the
// daemon's wakeWriteRetry (projectPause.retry, catchUpPacing.retry), so a retry a test waits for
// comes within milliseconds rather than fifteen seconds.
const testWriteRetry = 20 * time.Millisecond

// refuseWrites makes pool's database refuse every update of table for which when, a trigger's
// WHEN condition over old and new, holds, as a database that cannot take a write refuses it, until
// the open it returns is called. A write refused here is the daemon's own write through the store,
// and so is each retry of it, which is refused as well while the gate stays shut. Whether the gate
// is open is a row of a table of its own (<table>_gate), which a write reads at its statement's
// start, so every write that begins after open lands. pool must be a database of the test's own
// (isolatedOutboxPool), since the trigger stays on it.
func refuseWrites(t *testing.T, pool *store.Pool, table, when string) (open func()) {
	t.Helper()
	ctx := context.Background()
	gate, refuse := table+"_gate", "refuse_"+table+"_write"
	for _, statement := range []string{
		`create table ` + gate + ` (opened boolean not null)`,
		`insert into ` + gate + ` (opened) values (false)`,
		`create function ` + refuse + `() returns trigger language plpgsql as $$
			begin
				if not (select opened from ` + gate + `) then
					raise exception 'the test refuses this write of ` + table + `';
				end if;
				return new;
			end $$`,
		`create trigger ` + refuse + ` before update on ` + table + ` for each row when (` + when + `)
			execute function ` + refuse + `()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("make the writes of %s fail: %v", table, err)
		}
	}
	return func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `update `+gate+` set opened = true`); err != nil {
			t.Fatalf("let the writes of %s through: %v", table, err)
		}
	}
}

// resumeClockWrite is one write of a resume's stuck clock that landed: whether it left the clock
// stopped (pod_resumes.queued), and the clock it left (clock_at).
type resumeClockWrite struct {
	queued bool
	clock  time.Time
}

func (w resumeClockWrite) String() string {
	if w.queued {
		return "stop"
	}
	return "start at " + w.clock.UTC().Format(time.RFC3339Nano)
}

// recordResumeClockWrites makes pool's database keep each write of a resume's stuck clock that
// lands, a stop or a start, in a table of its own (resume_clock_writes), which resumeClockWrites
// reads in the order they landed: the writes of one resume update one row, so a later one waits
// for the earlier one's commit and takes the higher id. pool must be a database of the test's own
// (isolatedOutboxPool), since the trigger stays on it.
func recordResumeClockWrites(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`create table resume_clock_writes (id bigserial primary key, queued boolean not null, clock_at timestamptz not null)`,
		`create function record_resume_clock_write() returns trigger language plpgsql as $$
			begin insert into resume_clock_writes (queued, clock_at) values (new.queued, new.clock_at); return null; end $$`,
		`create trigger record_resume_clock_write after update on pod_resumes for each row
			when (old.queued is distinct from new.queued or old.clock_at is distinct from new.clock_at)
			execute function record_resume_clock_write()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("record the writes of the stuck clock: %v", err)
		}
	}
}

// resumeClockWrites is every write of a resume's stuck clock that has landed since
// recordResumeClockWrites, in the order they landed.
func resumeClockWrites(t *testing.T, pool *store.Pool) []resumeClockWrite {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select queued, clock_at from resume_clock_writes order by id`)
	if err != nil {
		t.Fatalf("read the writes of the stuck clock: %v", err)
	}
	writes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (resumeClockWrite, error) {
		var write resumeClockWrite
		err := row.Scan(&write.queued, &write.clock)
		return write, err
	})
	if err != nil {
		t.Fatalf("read the writes of the stuck clock: %v", err)
	}
	return writes
}

// A paced launch's wait in the queue stops the stuck clock of its role's open resume, and its
// let-out starts that clock again from the let-out's moment (projectPause.wakeQueued, wakeLetOut):
// two writes, each on a goroutine of its own, the second waiting for the first, and each made
// again until it lands (writeResumeClock). Here the database refuses the stop, at its first write
// and at every retry, until the test lets it through, and the let-out arrives while the stop is
// still being retried. Nothing lands while the stop is refused; once the database takes it, the
// stop lands and then the start, so the resume ends with its clock running from the let-out. The
// stuck warning names it ten minutes after the let-out and not a minute before, and the stranded
// report's read (strandedReleaseWakes) holds it with its clock at the let-out: the retry has not
// left the clock stopped, which would take a resume whose role never took its first turn out of
// both for good.
//
// The negative control is the start that does not wait for the write before it (writeResumeClock
// with its `<-before` dropped): the start lands while the stop is still refused, the stop lands
// after it and leaves the clock stopped, and the test fails at the start that landed first.
func TestARefusedStopOfAStuckClockIsRetriedAndLandsBeforeTheLetOutsStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w, pool, logged := wakeRuntime(t)
	const issue = "LEGION-208"
	role := claim.RoleImplementer
	token := mustClaimToken(t, issue, role)
	// An idle release suspended the implementer, and a wake an hour ago is bringing it back: an
	// open resume of the pod's newest release, which the stranded report reads.
	woke := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	release, _, err := w.idleStore.RecordPodRelease(ctx, store.PodRelease{Project: "legion", Issue: issue, Generation: 1,
		Roles: []string{string(role)}, At: woke.Add(-time.Minute)})
	if err != nil {
		t.Fatalf("record the pod's release: %v", err)
	}
	resume := recordPodWake(t, w.idleStore, store.PodResume{Release: release, Project: "legion", Issue: issue,
		Role: string(role), Kind: pods.WakeDelivery, At: woke})
	released := supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue, Role: role,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}
	recordResumeClockWrites(t, pool)
	open := refuseWrites(t, pool, "pod_resumes", "new.queued and not old.queued")
	logs := &syncBuffer{}
	p := &projectPause{ctx: t.Context(), store: w.idleStore, token: "legion", log: slog.New(slog.NewJSONHandler(logs, nil)),
		retry: testWriteRetry}

	const stopFailed = "wake: could not stop the stuck clock of its resume; trying again"
	p.wakeQueued(token, issue, role)
	testwait.Eventually(t, "the stop's first write to be refused", func() bool { return count(logs, stopFailed) > 0 })
	letOut := woke.Add(40 * time.Minute)
	p.wakeLetOut(token, issue, role, letOut)
	if testwait.Holds(250*time.Millisecond, testwait.Poll, func() bool { return len(resumeClockWrites(t, pool)) > 0 }) {
		t.Fatalf("writes of the stuck clock landed while its stop was refused: %v; want none: the stop is made again until it lands, and the start waits for it",
			resumeClockWrites(t, pool))
	}

	open()
	writes := testwait.Until(t, "the stop and the start of the stuck clock to land", func() ([]resumeClockWrite, bool) {
		writes := resumeClockWrites(t, pool)
		return writes, len(writes) >= 2
	})
	if len(writes) != 2 || !writes[0].queued || writes[1].queued || !writes[1].clock.Equal(letOut) {
		t.Fatalf("the writes of the stuck clock landed as %v; want the stop, then the start at the let-out %s", writes, letOut)
	}
	var queued bool
	var clock time.Time
	if err := pool.QueryRow(ctx, `select queued, clock_at from pod_resumes where id = $1`, resume).Scan(&queued, &clock); err != nil {
		t.Fatalf("read the resume's stuck clock: %v", err)
	}
	if queued || !clock.Equal(letOut) {
		t.Fatalf("the resume's clock reads stopped %t at %s; want it running from the let-out %s", queued, clock, letOut)
	}

	stranded := func(now time.Time) []workflow.ReleasedWake {
		t.Helper()
		wakes, err := store.ReadOnly(ctx, pool, "read the stranded wakes", func(ctx context.Context, tx pgx.Tx) ([]workflow.ReleasedWake, error) {
			return strandedReleaseWakes(ctx, w.idleStore, tx, "legion", []supervise.Claim{released}, now)
		})
		if err != nil {
			t.Fatalf("the stranded read: %v", err)
		}
		return wakes
	}
	if wakes := stranded(letOut.Add(-time.Minute)); len(wakes) != 0 {
		t.Fatalf("the stranded read a minute before the let-out = %+v; want none: the resume's clock runs from the let-out", wakes)
	}
	due := letOut.Add(workflow.StrandedReleaseWake)
	if wakes := stranded(due); len(wakes) != 1 || wakes[0].Issue != issue || wakes[0].Role != string(role) || !wakes[0].ClockAt.Equal(letOut) {
		t.Fatalf("the stranded read ten minutes after the let-out = %+v; want the %s's resume of %s with its clock at %s", wakes, role, issue, letOut)
	}
	const stuckLine = "has had no first turn"
	w.warnStuckResumes(ctx, due.Add(-time.Minute))
	if strings.Contains(logged.String(), stuckLine) {
		t.Fatalf("the daemon's log = %q; want no stuck warning nine minutes after the let-out", logged)
	}
	w.warnStuckResumes(ctx, due)
	line := logged.String()
	for _, want := range []string{stuckLine, issue, string(role), "clock=" + letOut.Format(time.RFC3339)} {
		if !strings.Contains(line, want) {
			t.Fatalf("the stuck warning = %q; want it to name %q", line, want)
		}
	}
	if !stuckMark(t, pool, resume) {
		t.Fatal("the resume is not marked stuck ten minutes after the let-out")
	}
}

// A held catch-up's place is let out by the pacer, and the write that makes its row due at the
// let-out (catchUpPacing.release) is refused by the database. The place is spent, and nothing else
// gives the row a due moment before the next boot, so the let-out is made again until it lands
// (catchUpPacing.again). While the database refuses it the row stays held and its place is kept;
// once the database takes it, the row is due at the let-out's own moment with the turns its place
// waited recorded for its wake's line (record.ReleaseHeldCatchUp), its lane is woken once, and
// its place is forgotten.
//
// The negative control is the let-out with no retry (letOut's again replaced by its one try, whose
// failure is logged and nothing more): the row stays held at record.CatchUpHeldAt with its place
// kept, and the test fails waiting for the row to come due.
func TestARefusedLetOutOfAHeldCatchUpIsRetriedUntilTheRowIsDue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	const root = "LEGION-1"
	runningArchitect(t, pool, records, sup, root)
	clock := &waveClock{now: wakeOrigin}
	pacing := testCatchUpPacing(t, pool, wakepace.New(testWakeRate, testWakeBurst, nil), clock)
	logs := &syncBuffer{}
	pacing.log, pacing.retry = slog.New(slog.NewJSONHandler(logs, nil)), testWriteRetry
	var wokenMu sync.Mutex
	var woken []string
	pacing.wake = func(issue string, kind record.OutboxKind) {
		wokenMu.Lock()
		defer wokenMu.Unlock()
		woken = append(woken, issue+" "+string(kind))
	}
	// The fresh pacer's first turn is spent, so the held row waits for the next one.
	pacing.pacer.Join(wakepace.Second)
	id := pacedCatchUp(t, pool, records, root, 1, wakeOrigin, record.CatchUpHeldAt)
	open := refuseWrites(t, pool, "outbox", "old.next_at is distinct from new.next_at")

	type catchUpRow struct {
		nextAt, pacedAt time.Time
		turns           int
	}
	read := func() catchUpRow {
		t.Helper()
		var row catchUpRow
		if err := pool.QueryRow(ctx, `select next_at, (payload->>'paced_at')::timestamptz, coalesce((payload->>'paced_turns')::int, 0)
			from outbox where id = $1`, id).Scan(&row.nextAt, &row.pacedAt, &row.turns); err != nil {
			t.Fatalf("read the catch-up row: %v", err)
		}
		return row
	}
	kept := func() bool {
		pacing.mu.Lock()
		defer pacing.mu.Unlock()
		_, ok := pacing.places[id]
		return ok
	}

	pacing.hold(id, root)
	turn := wakeOrigin.Add(testWakeInterval)
	if at := pacing.estimate(id); !at.Equal(turn) {
		t.Fatalf("the held catch-up's place estimates %s; want the pacer's next turn, %s", at, turn)
	}
	// The let-out runs on the pacer's turn, here the test's own goroutine, and its first write is
	// refused there.
	letOutCatchUps(t, clock, 1)
	const releaseFailed = "wake: could not let out a held catch-up; trying again"
	if count(logs, releaseFailed) == 0 {
		t.Fatalf("the daemon's log = %q; want the let-out's refused write logged", logs.String())
	}
	if row, place := read(), kept(); !record.CatchUpIsHeld(row.nextAt) || !place {
		t.Fatalf("after its let-out was refused the row is due at %s and its place kept %t; want it held, with its place kept for the retry",
			row.nextAt, place)
	}

	open()
	testwait.Until(t, "the refused let-out to be made again until the row is due", func() (string, bool) {
		row, place := read(), kept()
		return fmt.Sprintf("row %d due at %s, place kept %t", id, row.nextAt, place), !record.CatchUpIsHeld(row.nextAt) && !place
	})
	if row := read(); !row.nextAt.Equal(turn) || !row.pacedAt.Equal(turn) || row.turns != 1 {
		t.Fatalf("the let-out catch-up is due at %s, paced at %s after %d turns; want due and paced at the let-out %s after its one turn",
			row.nextAt, row.pacedAt, row.turns, turn)
	}
	wokenMu.Lock()
	lanes := append([]string(nil), woken...)
	wokenMu.Unlock()
	if want := root + " " + string(record.OutboxKindNotice); len(lanes) != 1 || lanes[0] != want {
		t.Fatalf("the lanes woken = %q; want %q once, by the retry's commit", lanes, want)
	}
	if at := pacing.estimate(id); !at.IsZero() {
		t.Fatalf("the released catch-up's place estimates %s; want its place forgotten", at)
	}
}
