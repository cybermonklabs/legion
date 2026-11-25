package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ciIssue is the issue of the outbox-level CI news rows here, and ciHeadX and ciHeadY two heads of
// its pull request: the one the rows are at, and the one a push moves it to.
const (
	ciIssue = "LEGION-208"
	ciHeadX = "c1c1000000000000000000000000000000000001"
	ciHeadY = "c1c1000000000000000000000000000000000002"
)

// ciRig is an outbox runner over an isolated database holding ciIssue in a phase, its tree admitted
// and holding its slot, its open pull request #42 at ciHeadX, and a supervisor with no claim on it
// yet, as runnerIn builds it. Its listener is holderPublisher's, and its log is kept. The rows it
// queues are due at start, and each attempt runs the outbox once on the runner's clock set past it.
type ciRig struct {
	t         *testing.T
	runner    *outbox
	sup       *supervisor
	pool      *store.Pool
	publisher *holderPublisher
	logs      *syncBuffer
	start     time.Time
}

func newCIRig(t *testing.T, at phase.Phase) *ciRig {
	t.Helper()
	publisher := &holderPublisher{}
	logs := &syncBuffer{}
	runner, sup, pool := runnerIn(t, at, "in_progress", publisher, slog.New(slog.NewJSONHandler(logs, nil)))
	holdSlot(t, pool, ciIssue)
	r := &ciRig{t: t, runner: runner, sup: sup, pool: pool, publisher: publisher, logs: logs, start: time.Now()}
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return runner.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: ciIssue, Repo: "acme/widgets", Number: 42,
			Branch: "legion/" + ciIssue, HeadSHA: ciHeadX, State: "open"})
	})
	return r
}

// tx writes the rig's records in one transaction.
func (r *ciRig) tx(write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.pool, write); err != nil {
		r.t.Fatalf("write the rig's records: %v", err)
	}
}

// entry raises role's phases-row entry to entry.
func (r *ciRig) entry(role claim.Role, entry int64) {
	r.t.Helper()
	token := mustClaimToken(r.t, ciIssue, role)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.runner.records.PutPhase(ctx, tx, record.PhaseRow{Issue: ciIssue, Role: role, Claim: token, Entry: entry,
			EntryTask: "work", EntryRaisedAt: time.Now().UTC()})
	})
}

// attempt runs the outbox once with its clock at the rig's start and after.
func (r *ciRig) attempt(after time.Duration) {
	r.t.Helper()
	at := r.start.Add(after)
	r.runner.now = func() time.Time { return at }
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("run the outbox at +%s: %v", after, err)
	}
}

// left is how many rows of kind the outbox still holds.
func (r *ciRig) left(kind record.OutboxKind) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1`, string(kind)).Scan(&n); err != nil {
		r.t.Fatalf("count the %s rows: %v", kind, err)
	}
	return n
}

// ciTopic is the role topic of role's claim on ciIssue.
func ciTopic(t *testing.T, role claim.Role) string {
	t.Helper()
	return notify.RoleTopicPrefix + string(mustClaimToken(t, ciIssue, role))
}

// ciPrompt is a prompt to read Dispatch for role's reader of the task default on ciIssue at
// generation and head, replacing the head's set of checks with checks.
func ciPrompt(t *testing.T, role claim.Role, generation uint64, head string, reader record.CIReader, checks ...string) record.CINews {
	t.Helper()
	set := checks
	return record.CINews{Purpose: record.CINewsState, Role: string(mustClaimToken(t, ciIssue, role)), Generation: generation, Task: "default",
		Repo: "acme/widgets", PullRequest: 42, Head: head, Reader: &reader, Checks: &set, Body: record.ReadPromptBody(ciIssue, head, checks)}
}

// staleLine is the line a row for work no longer current finishes with.
const staleLine = "outbox CI news finished undelivered: the work it is for is no longer current"

// releasedLine is main's line of a role message to a role whose pod is released.
const releasedLine = "outbox role message finished undelivered: its role's pod is released, and the message is not worth a wake"

// wokenThenDelivered holds that row id, a row of CI news for role's released reader, wakes the
// reader's claim, its wakes then numbering wakes, and that its publish is refused while no session
// holds the role's topic, the row staying; that once a session holds the topic the row is
// published once, under its own key, and finishes; and that it woke the claim no second time.
func (d *ciDaemon) wokenThenDelivered(role claim.Role, id int64, wakes int) {
	d.t.Helper()
	token := d.token(d.key, role)
	key := record.OutboxKey(id)
	refused := d.listener.refusedTo(token)
	d.until(token, fmt.Sprintf("row %d to take wake %d of the %s, and its publish refused while no session holds the role", id, wakes, role), func() bool {
		return d.wakes(role) == wakes && d.listener.refusedTo(token) > refused
	})
	if taken := d.listener.takenTo(token, key); len(taken) != 0 || !d.rowLeft(id) {
		d.t.Fatalf("before a session holds the %s's topic: %d publishes of row %d taken, the row left %t; want none and the row kept",
			role, len(taken), id, d.rowLeft(id))
	}
	d.listener.hold(token)
	d.hurry(record.OutboxKindCINews)
	d.until(token, fmt.Sprintf("row %d to finish once a session holds the %s's topic", id, role), func() bool { return !d.rowLeft(id) })
	if taken := d.listener.takenTo(token, key); len(taken) != 1 {
		d.t.Fatalf("row %d was published %d times under its key %s, want once", id, len(taken), key)
	}
	if n := d.wakes(role); n != wakes {
		d.t.Fatalf("the %s took %d wakes, want %d: the row wakes its claim once", role, n, wakes)
	}
}

// A row of CI news for a current reader whose pod was released wakes that pod and waits for it:
// the reviewer, a running fix and the merger in merging each get one wake reservation, no publish
// while no session holds the role's topic, and then one publish under the row's own key, after
// which the row is finished. Each runs on a whole daemon (ciDaemon).
//
// The control, a kept subtest: delivered as a role message (the seam ciNewsAsRoleMessage), the row
// to the released reviewer finishes undelivered, with no wake.
func TestACINewsRowWakesItsReleasedReader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		role   claim.Role
		entry  int64
		reader record.CIReader
		fix    bool
	}{
		{"a released reviewer", phase.Reviewing, claim.RoleReviewer, ciReviewerEntry, record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}, false},
		{"a released fix", phase.Reviewing, claim.RoleImplementer, ciFixEntry, record.CIReader{Fix: 1, Entry: ciFixEntry}, true},
		{"a released merger in merging", phase.Merging, claim.RoleMerger, ciMergerEntry, record.CIReader{Phase: phase.Merging, Entry: ciMergerEntry}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newCIDaemon(t, nil)
			d.seedIssue(tc.at, 1, ciHeadX, d.releasedReader(tc.role, 1, tc.entry))
			if tc.fix {
				d.runningFix(tc.entry, ciHeadX)
			} else {
				d.entry(tc.role, tc.entry)
			}
			d.boot()
			id := d.queue(d.prompt(tc.role, 1, ciHeadX, tc.reader, "k-ci"))
			d.wokenThenDelivered(tc.role, id, 1)
		})
	}
	t.Run("control: ciNewsAsRoleMessage finishes the row undelivered with no wake", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, func(o *outbox) { o.ciNewsAsRoleMessage = true })
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.boot()
		id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}, "k-ci"))
		d.until(token, "the row delivered as a role message to finish", func() bool { return !d.rowLeft(id) })
		if wakes, refused, taken := d.wakes(claim.RoleReviewer), d.listener.refusedTo(token), len(d.listener.takenTo(token, "")); wakes != 0 || refused != 0 || taken != 0 {
			t.Fatalf("the row as a role message: %d wakes, %d publishes refused, %d taken; want it finished undelivered with no wake, the fault the reader's delivery takes out",
				wakes, refused, taken)
		}
		if lines := d.ciLines(releasedLine, id); len(lines) != 1 {
			t.Fatalf("the role message's lines = %v, want main's one", lines)
		}
	})
}

// A reader released a second time is woken a second time: a report of the listener's runs wakes
// the released reviewer and reaches it once its session holds its topic; the woken reviewer comes
// up, takes the task the daemon gives a ready worker, ends its turn and reports no background
// work, and the idle release takes its pod again; a prompt to read Dispatch then wakes it again
// and reaches it once. Two wakes in all, two launches, and each row published once under its key.
//
// The control, a kept subtest: delivered as role messages (the seam ciNewsAsRoleMessage), neither
// row wakes the released reviewer, so no wake follows the state.
func TestASecondReleaseIsWokenByTheStatePrompt(t *testing.T) {
	t.Parallel()
	reader := record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}
	t.Run("the prompt wakes the reviewer the idle release took again", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.boot()
		first := d.queue(d.report(claim.RoleReviewer, ciHeadX, reader))
		d.wokenThenDelivered(claim.RoleReviewer, first, 1)

		d.upAndIdle(token)
		d.listener.unhold(token)
		d.advance(pauseRigIdleMinutes*time.Minute + 5*time.Minute)
		if released := d.awaitState(token, supervise.StateSuspended, "the idle release takes the reviewer's pod again"); !released.Released() {
			t.Fatalf("the reviewer after the idle pass is suspended by %s, want released", released.SuspendedBy)
		}
		second := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, reader, "k-ci"))
		d.wokenThenDelivered(claim.RoleReviewer, second, 2)
		if launches := len(d.rt.launchesOf(token)); launches != 2 {
			t.Fatalf("the reviewer was launched %d times, want twice: once for each wake", launches)
		}
	})
	t.Run("control: ciNewsAsRoleMessage, and no wake follows the state", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, func(o *outbox) { o.ciNewsAsRoleMessage = true })
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.boot()
		first := d.queue(d.report(claim.RoleReviewer, ciHeadX, reader))
		d.until(token, "the report delivered as a role message to finish", func() bool { return !d.rowLeft(first) })
		second := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, reader, "k-ci"))
		d.until(token, "the prompt delivered as a role message to finish", func() bool { return !d.rowLeft(second) })
		if wakes, launches := d.wakes(claim.RoleReviewer), len(d.rt.launchesOf(token)); wakes != 0 || launches != 0 {
			t.Fatalf("as role messages the rows took %d wakes and %d launches, want none: no wake follows the state, the fault the reader's delivery takes out",
				wakes, launches)
		}
	})
}

// A release between the executor's read of the claim and its publish is a publish the listener
// answers with no holder: the reviewer is up and idle, not released, when the row reads its claim,
// so the row wakes nothing, its publish is refused, and the row stays. The pod is then released,
// as the idle release stops a claim (supervise.RequestSuspend by SuspendedByReleased), and the
// row's retry wakes the released reviewer and, once a session holds the role's topic again,
// publishes once, under the row's key. Each runs on a whole daemon.
//
// The control, a kept subtest: with no holder taken as delivered (the seam noHolderDelivers), the
// row finishes unpublished at the refused publish, which the subtest asserts.
func TestAReleaseDuringDeliveryKeepsTheRow(t *testing.T) {
	t.Parallel()
	play := func(t *testing.T, noHolderDelivers bool) (*ciDaemon, claim.Token, int64) {
		t.Helper()
		d := newCIDaemon(t, func(o *outbox) { o.noHolderDelivers = noHolderDelivers })
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.launchingReader(claim.RoleReviewer, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.boot()
		d.until(token, "the boot's launch of the reviewer", func() bool { return len(d.rt.launchesOf(token)) >= 1 })
		d.upAndIdle(token)
		id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}, "k-ci"))
		d.until(token, "the row's publish refused for want of a holder", func() bool { return d.listener.refusedTo(token) >= 1 })
		return d, token, id
	}
	t.Run("the row stays, and its retry wakes the released claim and publishes once", func(t *testing.T) {
		t.Parallel()
		d, token, id := play(t, false)
		key := record.OutboxKey(id)
		if !d.rowLeft(id) || len(d.listener.takenTo(token, key)) != 0 || d.wakes(claim.RoleReviewer) != 0 {
			t.Fatalf("the publish no holder took: the row left %t, %d taken, %d wakes; want the row kept, nothing taken and no wake",
				d.rowLeft(id), len(d.listener.takenTo(token, key)), d.wakes(claim.RoleReviewer))
		}
		d.handle(token, supervise.RequestSuspend{Claim: token, Reason: "the idle release takes the reviewer's pod", By: supervise.SuspendedByReleased})
		if released := d.awaitState(token, supervise.StateSuspended, "the release stops the reviewer"); !released.Released() {
			t.Fatalf("the reviewer after the release is suspended by %s, want released", released.SuspendedBy)
		}
		d.hurry(record.OutboxKindCINews)
		d.wokenThenDelivered(claim.RoleReviewer, id, 1)
	})
	t.Run("control: noHolderDelivers finishes the row unpublished", func(t *testing.T) {
		t.Parallel()
		d, token, id := play(t, true)
		d.until(token, "the row taken as delivered to finish", func() bool { return !d.rowLeft(id) })
		if taken := len(d.listener.takenTo(token, "")); taken != 0 {
			t.Fatalf("with no holder taken as delivered: %d publishes taken; want the row finished unpublished, the fault the wait takes out", taken)
		}
	})
}

// A daemon that stops in the middle of a row's delivery delivers the row once after its restart.
// Stopped after the row's wake took its reservation, the restarted daemon takes no second wake
// and the row reaches the reader once, with no wake of it left waiting in the pacing. Stopped
// after the listener took the row's publish and before the row's finish, the restarted daemon
// publishes the row again under the same key, which the stream keeps once: one message in all.
//
// The control, a kept subtest: the row finished before its publish is confirmed, as a planted
// fault (the test deletes the row while the listener holds its publish untaken): after the
// restart no publish of it was taken, so the count the stop's case asserts reads 0, not 1.
func TestARestartDuringTheWakeOrThePublishDeliversOnce(t *testing.T) {
	t.Parallel()
	reader := record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}
	// gated boots a daemon whose reviewer is launching and holds its topic, queues the row, and
	// stops the daemon once the listener holds the row's publish at a gate, before or after it
	// takes it; leave runs on the stopped daemon's row, and the daemon then boots again.
	gated := func(t *testing.T, afterTake bool, leave func(*ciDaemon, int64)) (*ciDaemon, claim.Token, int64) {
		t.Helper()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.launchingReader(claim.RoleReviewer, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.listener.hold(token)
		gate := d.listener.gate(token, afterTake)
		d.boot()
		id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, reader, "k-ci"))
		d.until(token, "the row's publish to reach the listener's gate", func() bool {
			select {
			case <-gate.reached:
				return true
			default:
				return false
			}
		})
		leave(d, id)
		d.d.stop()
		gate.let()
		rebindHeldPorts(t, &d.cfg)
		d.boot()
		return d, token, id
	}
	t.Run("stopped after the wake's reservation", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.boot()
		id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, reader, "k-ci"))
		d.until(token, "the row's wake to take its reservation", func() bool { return d.wakes(claim.RoleReviewer) == 1 })
		d.restart()
		d.listener.hold(token)
		d.hurry(record.OutboxKindCINews)
		d.until(token, "the row to finish after the restart", func() bool { return !d.rowLeft(id) })
		key := record.OutboxKey(id)
		if taken, wakes := len(d.listener.takenTo(token, key)), d.wakes(claim.RoleReviewer); taken != 1 || wakes != 1 {
			t.Fatalf("across the restart: %d publishes under %s, %d wakes; want one publish and the one wake", taken, key, wakes)
		}
		if d.waiting(token) {
			t.Fatal("a wake of the reviewer is left waiting in the pacing after the delivery, want none")
		}
	})
	t.Run("stopped after the publish and before the finish", func(t *testing.T) {
		t.Parallel()
		d, token, id := gated(t, true, func(*ciDaemon, int64) {})
		d.until(token, "the row to finish after the restart", func() bool { return !d.rowLeft(id) })
		key := record.OutboxKey(id)
		taken := d.listener.takenTo(token, "")
		if len(taken) != 2 || taken[0].key != key || taken[1].key != key || d.listener.delivered(token, "") != 1 {
			t.Fatalf("the publishes taken across the restart = %+v, want two, both under %s, one message on the stream", taken, key)
		}
	})
	t.Run("control: the row finished before its publish is confirmed", func(t *testing.T) {
		t.Parallel()
		d, token, id := gated(t, false, func(d *ciDaemon, id int64) {
			if _, err := d.pool.Exec(d.ctx, `delete from outbox where id = $1`, id); err != nil {
				d.t.Fatalf("finish row %d before its publish is confirmed: %v", id, err)
			}
		})
		time.Sleep(2 * time.Second)
		if delivered := d.listener.delivered(token, record.OutboxKey(id)); delivered != 0 || d.rowLeft(id) {
			t.Fatalf("with the row finished before its publish was confirmed: %d messages, the row left %t; want none, the loss the stop's case would count as 0 for 1",
				delivered, d.rowLeft(id))
		}
	})
}

// A row for work no longer current wakes nothing: a row whose reader's phases-row entry is now
// above the row's, a row of the generation before the issue's, and a row at a head the pull
// request has left each finish with one INFO line, no wake and no publish. A stale prompt of the
// owner of the generation before, beside the current owner's own prompt at the same head and entry
// 0, wakes nobody, and the current prompt reaches the owner once. Each runs on a whole daemon.
//
// The control, a kept subtest: with the fence taken out (the seam noCurrentWorkFence), the row
// whose reader's entry was raised wakes the claim, which the subtest asserts.
func TestARowForWorkNoLongerCurrentWakesNothing(t *testing.T) {
	t.Parallel()
	reviewer := record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}
	for _, tc := range []struct {
		name       string
		generation uint64
		given, row int64
		head       string
		rowAt      uint64
	}{
		{"the reader's entry raised past the row's", 1, ciReviewerEntry + 1, ciReviewerEntry + 1, ciHeadX, 1},
		{"the generation moved", 2, ciReviewerEntry, ciReviewerEntry, ciHeadX, 1},
		{"the head moved", 1, ciReviewerEntry, ciReviewerEntry, ciHeadY, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newCIDaemon(t, nil)
			token := d.token(d.key, claim.RoleReviewer)
			d.seedIssue(phase.Reviewing, tc.generation, tc.head, d.releasedReader(claim.RoleReviewer, tc.generation, tc.given))
			d.entry(claim.RoleReviewer, tc.row)
			d.boot()
			id := d.queue(d.prompt(claim.RoleReviewer, tc.rowAt, ciHeadX, reviewer, "k-ci"))
			d.until(token, "the stale row to finish", func() bool { return !d.rowLeft(id) })
			if wakes, refused, taken := d.wakes(claim.RoleReviewer), d.listener.refusedTo(token), len(d.listener.takenTo(token, "")); wakes != 0 || refused != 0 || taken != 0 {
				t.Fatalf("%s: %d wakes, %d publishes refused, %d taken; want the row finished with no wake and no publish", tc.name, wakes, refused, taken)
			}
			if lines := d.ciLines(staleLine, id); len(lines) != 1 || lines[0]["level"] != "INFO" {
				t.Fatalf("the stale row's lines = %v, want one at INFO", lines)
			}
		})
	}
	t.Run("a stale owner's prompt beside the current one", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleArchitect)
		d.seedIssue(phase.Accepting, 2, ciHeadX, d.releasedReader(claim.RoleArchitect, 2, 0))
		d.boot()
		owner := record.CIReader{Phase: phase.Accepting}
		if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
			for _, generation := range []uint64{1, 2} {
				prompt := d.prompt(claim.RoleArchitect, generation, ciHeadX, owner)
				prompt.Checks = nil
				prompt.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: int64(generation)}}
				if joined, err := d.records.EnqueueReadPrompt(ctx, tx, d.key, prompt, d.dueNow()); err != nil || joined {
					return fmt.Errorf("the prompt of generation %d = joined %t, %v; want a row of its own", generation, joined, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var ids []int64
		rows, err := d.pool.Query(d.ctx, `select id from outbox where issue = $1 and kind = 'ci_news' order by id`, d.key)
		if err == nil {
			ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		}
		if err != nil || len(ids) != 2 {
			t.Fatalf("the two prompts' rows = %v, %v; want two", ids, err)
		}
		stale, current := ids[0], ids[1]
		d.wake(record.OutboxKindCINews)
		d.until(token, "the stale owner's prompt to finish", func() bool { return !d.rowLeft(stale) })
		if lines := d.ciLines(staleLine, stale); len(lines) != 1 || lines[0]["generation"] != float64(1) {
			t.Fatalf("the stale lines = %v, want one, of the generation before's row", lines)
		}
		d.wokenThenDelivered(claim.RoleArchitect, current, 1)
		if taken := len(d.listener.takenTo(token, record.OutboxKey(stale))); taken != 0 {
			t.Fatalf("the stale prompt was published %d times, want never", taken)
		}
	})
	t.Run("control: noCurrentWorkFence wakes the claim of a raised entry", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, func(o *outbox) { o.noCurrentWorkFence = true })
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry+1))
		d.entry(claim.RoleReviewer, ciReviewerEntry+1)
		d.boot()
		id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, reviewer, "k-ci"))
		d.until(token, "the stale row to wake the claim with the fence taken out", func() bool { return d.wakes(claim.RoleReviewer) == 1 })
		if !d.rowLeft(id) {
			t.Fatal("with the fence taken out the stale row finished, want it kept waiting for the claim it woke, the fault the fence takes out")
		}
	})
}

// A row of CI news that a project's pause holds is owed to its reader all the same: while the
// project is paused the row runs nothing, wakes no claim and publishes nothing; at the resume it
// wakes the released reviewer and reaches it once, under its own key. The pause holds the kind as
// it holds a hook prompt (record's pauseHolds).
func TestAPauseKeepsAnOwedCIRow(t *testing.T) {
	t.Parallel()
	d := newCIDaemon(t, nil)
	token := d.token(d.key, claim.RoleReviewer)
	d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
	d.entry(claim.RoleReviewer, ciReviewerEntry)
	d.boot()
	d.pauseProject()
	id := d.queue(d.prompt(claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}, "k-ci"))
	time.Sleep(2 * time.Second)
	if wakes, refused, taken := d.wakes(claim.RoleReviewer), d.listener.refusedTo(token), len(d.listener.takenTo(token, "")); wakes != 0 || refused != 0 || taken != 0 || !d.rowLeft(id) {
		t.Fatalf("while the project is paused: %d wakes, %d publishes refused, %d taken, the row left %t; want the row held, untouched",
			wakes, refused, taken, d.rowLeft(id))
	}
	d.resumeProject()
	d.wokenThenDelivered(claim.RoleReviewer, id, 1)
}

// A row of CI news with no reader is a role message: to a released claim it finishes undelivered
// with main's line and wakes nothing, and to a claim whose session holds its topic it is published
// once.
func TestAnInformationalRowIsDeliveredAsARoleMessage(t *testing.T) {
	t.Parallel()
	news := func(d *ciDaemon) record.CINews {
		return record.CINews{Purpose: record.CINewsRuns, Role: string(d.token(d.key, claim.RoleReviewer)), Generation: 1, Task: "default",
			Repo: liveRepo, PullRequest: ciPullRequest, Head: ciHeadX, Next: record.CINextEndHoldCI,
			Runs: []record.CIRun{{Name: "unit", ID: 9, Head: ciHeadX, Conclusion: "success"}}, Body: "Legion holds the task before its retro."}
	}
	t.Run("to a released claim", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
		d.boot()
		id := d.queue(news(d))
		d.until(token, "the informational row to finish", func() bool { return !d.rowLeft(id) })
		if wakes, refused, taken := d.wakes(claim.RoleReviewer), d.listener.refusedTo(token), len(d.listener.takenTo(token, "")); wakes != 0 || refused != 0 || taken != 0 {
			t.Fatalf("the informational row to a released claim: %d wakes, %d refused, %d taken; want it finished undelivered with no wake", wakes, refused, taken)
		}
		if lines := d.ciLines(releasedLine, id); len(lines) != 1 {
			t.Fatalf("the role message's lines = %v, want main's one", lines)
		}
	})
	t.Run("to a holder", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.launchingReader(claim.RoleReviewer, ciReviewerEntry))
		d.listener.hold(token)
		d.boot()
		id := d.queue(news(d))
		d.until(token, "the informational row to finish", func() bool { return !d.rowLeft(id) })
		if taken := len(d.listener.takenTo(token, record.OutboxKey(id))); taken != 1 {
			t.Fatalf("the informational row to a holder was published %d times under its key, want once", taken)
		}
	})
}

// Ten checks of one run that change before the prompt is delivered give one row, holding the ten,
// one wake of the released reviewer and one publish.
func TestTenChecksOfOneRunGiveOneWake(t *testing.T) {
	t.Parallel()
	d := newCIDaemon(t, nil)
	d.seedIssue(phase.Reviewing, 1, ciHeadX, d.releasedReader(claim.RoleReviewer, 1, ciReviewerEntry))
	d.entry(claim.RoleReviewer, ciReviewerEntry)
	d.boot()
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		for i := 1; i <= 10; i++ {
			prompt := d.prompt(claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry})
			prompt.Checks = nil
			prompt.States = []record.CIStateSnapshot{{Check: fmt.Sprintf("k-%02d", i), State: "no", Seq: int64(i)}}
			joined, err := d.records.EnqueueReadPrompt(ctx, tx, d.key, prompt, d.dueNow())
			if err != nil || joined != (i > 1) {
				return fmt.Errorf("prompt %d = joined %t, %v; want joined from the second on", i, joined, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var id int64
	var states int
	if err := d.pool.QueryRow(d.ctx, `select id, jsonb_array_length(payload->'states') from outbox where issue = $1 and kind = 'ci_news'`,
		d.key).Scan(&id, &states); err != nil || states != 10 || d.left(record.OutboxKindCINews) != 1 {
		t.Fatalf("the queued prompts = %d rows, the row holding %d checks, %v; want one row holding the ten", d.left(record.OutboxKindCINews), states, err)
	}
	d.wake(record.OutboxKindCINews)
	d.wokenThenDelivered(claim.RoleReviewer, id, 1)
}

// A prompt whose set of checks is replaced by no checks, as the removal of a head's last check
// leaves it, keeps that empty set through its encoding, its strict decoding and the store, keeps
// it through a later change of a check's state alone that joins it, and is delivered to its
// current reader once. A prompt with neither a snapshot nor a set is refused.
//
// The control, a kept subtest: the slice encoding (omitEmptyCheckSet), a set of no checks left out
// as an empty slice is, either fails the strict decoding where the row holds no snapshot or loses
// the replacement where it holds one, which the subtest asserts.
func TestAnEmptyReplacementSetSurvivesTheOutbox(t *testing.T) {
	t.Parallel()
	reader := record.CIReader{Phase: phase.Reviewing, Entry: ciReviewerEntry}
	t.Run("the replacement of no checks is kept, joined and delivered once", func(t *testing.T) {
		t.Parallel()
		d := newCIDaemon(t, nil)
		token := d.token(d.key, claim.RoleReviewer)
		d.seedIssue(phase.Reviewing, 1, ciHeadX, d.launchingReader(claim.RoleReviewer, ciReviewerEntry))
		d.entry(claim.RoleReviewer, ciReviewerEntry)
		d.listener.hold(token)
		d.boot()
		if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
			if joined, err := d.records.EnqueueReadPrompt(ctx, tx, d.key, d.prompt(claim.RoleReviewer, 1, ciHeadX, reader), d.dueNow()); err != nil || joined {
				return fmt.Errorf("the empty set's prompt = joined %t, %v; want a row of its own", joined, err)
			}
			change := d.prompt(claim.RoleReviewer, 1, ciHeadX, reader)
			change.Checks = nil
			change.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 3}}
			if joined, err := d.records.EnqueueReadPrompt(ctx, tx, d.key, change, d.dueNow()); err != nil || !joined {
				return fmt.Errorf("the change of a state alone = joined %t, %v; want it joined", joined, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var stored record.OutboxRow
		if err := d.pool.QueryRow(d.ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'ci_news'`, d.key).
			Scan(&stored.ID, &stored.Kind, &stored.Issue, &stored.Payload); err != nil {
			t.Fatalf("read the stored prompt: %v", err)
		}
		decoded, err := record.DecodeOutboxPayload(stored)
		if err != nil {
			t.Fatalf("decode the stored prompt strictly: %v", err)
		}
		if prompt := decoded.(record.CINews); prompt.Checks == nil || len(*prompt.Checks) != 0 || len(prompt.States) != 1 {
			t.Fatalf("the stored prompt = %+v, want a present empty set and the one snapshot", prompt)
		}
		d.wake(record.OutboxKindCINews)
		d.until(token, "the prompt to finish", func() bool { return !d.rowLeft(stored.ID) })
		if taken := len(d.listener.takenTo(token, record.OutboxKey(stored.ID))); taken != 1 {
			t.Fatalf("the prompt was published %d times under its key, want once", taken)
		}
	})
	t.Run("a prompt with neither a snapshot nor a set is refused", func(t *testing.T) {
		t.Parallel()
		prompt := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		prompt.Checks = nil
		if _, err := record.NewOutboxRow(ciIssue, prompt, time.Now()); err == nil {
			t.Fatal("a prompt with neither a snapshot nor a set was taken, want it refused")
		}
	})
	t.Run("control: omitEmptyCheckSet loses the replacement", func(t *testing.T) {
		t.Parallel()
		sliceEncoded := func(n record.CINews) []byte {
			t.Helper()
			encoded, err := json.Marshal(n)
			if err != nil {
				t.Fatalf("encode the prompt: %v", err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &members); err != nil {
				t.Fatalf("read the prompt's members: %v", err)
			}
			if string(members["checks"]) == "[]" {
				delete(members, "checks")
			}
			out, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("encode the prompt again: %v", err)
			}
			return out
		}
		alone := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		if _, err := record.DecodeOutboxPayload(record.OutboxRow{ID: 1, Kind: record.OutboxKindCINews, Issue: ciIssue, Payload: sliceEncoded(alone)}); err == nil ||
			!strings.Contains(err.Error(), "set of checks") {
			t.Fatalf("the slice encoding of a set of no checks decodes = %v, want it refused for want of a set", err)
		}
		joined := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		joined.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 3}}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: 2, Kind: record.OutboxKindCINews, Issue: ciIssue, Payload: sliceEncoded(joined)})
		if err != nil || decoded.(record.CINews).Checks != nil {
			t.Fatalf("the slice encoding beside a snapshot decodes = %+v, %v; want the replacement lost", decoded, err)
		}
	})
}

// readPrompting queues a prompt to read Dispatch inside the fact's transaction, as the workflow's
// checkState does (through record.Store.EnqueueReadPrompt), and keeps whether it joined a row.
type readPrompting struct {
	records record.Store
	prompt  record.CINews
	at      time.Time
	joined  *bool
}

func (h readPrompting) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	joined, err := h.records.EnqueueReadPrompt(ctx, tx, ciIssue, h.prompt, h.at)
	*h.joined = joined
	return intake.Result{}, err
}

// A prompt to read Dispatch is delivered as soon as the fact that queued it commits, as every
// other row is. The workflow queues it through record.Store.EnqueueReadPrompt, whose new row the
// record writes with its own Enqueue, so the workflow's store (projectRecords) wakes the issue's
// role-message lane there: without that wake the prompt waits for the next sweep, or for another
// row of the lane to wake it. The lanes run on a held clock, so no sweep fires: the commit wakes
// the lane, and the reviewer is published the prompt once on that wake alone.
//
// The control, a kept subtest: the same prompt queued past the workflow's store wakes no lane and
// stays queued, published to nobody, until the test fires the sweep, which then delivers it.
func TestAReadPromptRunsOnItsLaneWithoutASweep(t *testing.T) {
	t.Parallel()
	play := func(t *testing.T, workflowStore bool) (*ciRig, *heldClock, []string) {
		t.Helper()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		clock := microClock()
		var mu sync.Mutex
		var woken []string
		records := r.runner.records
		if workflowStore {
			records = projectRecords{Store: r.runner.records, project: "LEGION", log: quietLogger(), wake: func(issue string, kind record.OutboxKind) {
				mu.Lock()
				woken = append(woken, issue+":"+string(kind))
				mu.Unlock()
				r.runner.wake(issue, kind)
			}}
		}
		runLanes(t, r.runner, clock)
		prompt := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: 4})
		prompt.Checks = nil
		prompt.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 1}}
		var joined bool
		if _, err := intake.ApplyFact(context.Background(), r.pool, "lanes-test", "read-prompt", intake.DaemonBooted{Boot: 1},
			readPrompting{records: records, prompt: prompt, at: clock.Now(), joined: &joined}); err != nil || joined {
			t.Fatalf("queue the prompt = joined %t, %v; want a row of its own", joined, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return r, clock, slices.Clone(woken)
	}
	delivered := func(t *testing.T, r *ciRig) {
		t.Helper()
		testwait.Eventually(t, "the prompt published to the reviewer and its row finished", func() bool {
			_, delivered := r.publisher.snapshot()
			return len(delivered) == 1 && r.left(record.OutboxKindCINews) == 0
		})
		if _, delivered := r.publisher.snapshot(); delivered[0].topic != ciTopic(t, claim.RoleReviewer) {
			t.Fatalf("the prompt was published to %s, want the reviewer's topic %s", delivered[0].topic, ciTopic(t, claim.RoleReviewer))
		}
	}
	t.Run("the commit wakes the lane, which delivers the prompt", func(t *testing.T) {
		t.Parallel()
		r, clock, woken := play(t, true)
		if want := ciIssue + ":" + string(record.OutboxKindCINews); !slices.Equal(woken, []string{want}) {
			t.Fatalf("the commit of the prompt woke the lanes %v, want %s's: the prompt waits for the sweep", woken, want)
		}
		delivered(t, r)
		if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
			t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired", sweeps)
		}
	})
	t.Run("control: queued past the workflow's store, the prompt waits for the sweep", func(t *testing.T) {
		t.Parallel()
		r, clock, woken := play(t, false)
		if len(woken) != 0 {
			t.Fatalf("the bare store woke %v, want nothing", woken)
		}
		if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 1 {
			t.Fatalf("before the sweep: %d publishes, %d rows; want none and the prompt still queued", attempts, r.left(record.OutboxKindCINews))
		}
		fireSweep(t, clock)
		delivered(t, r)
	})
}
