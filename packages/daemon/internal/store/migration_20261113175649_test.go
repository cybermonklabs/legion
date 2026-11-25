package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// taskNumbersVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const taskNumbersVersion = 20261113175649

// The way back of migration 20261113175649 stands in two files, as the phase entry's does: the
// header of the migration, which is the source, and a fenced block of docs/kubernetes.md, where an
// operator reads it at a rollback. The two paths are what a failure names, so a failing run names
// the file to open. taskNumbersHeredoc opens the block in both and taskNumbersTerminator closes
// it; taskNumbersWayBackStatements is how many statements stand between its begin and its commit,
// which the header and the guide both call the twenty-two.
const (
	taskNumbersFile              = "internal/store/migrations/20261113175649_task_numbers.up.sql"
	taskNumbersGuide             = "docs/kubernetes.md"
	taskNumbersHeredoc           = "psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'"
	taskNumbersTerminator        = "SQL"
	taskNumbersWayBackStatements = 22
)

// taskNumbersReadMarks are the push path's three marks on a pending task, which the migration
// drops and its way back puts back.
var taskNumbersReadMarks = []string{"delivered_at", "marked_by", "prompted"}

// taskNumbersColumns are the fourteen columns the migration adds, which its way back drops.
var taskNumbersColumns = []struct{ table, column string }{
	{"claims", "task_written"}, {"claims", "task_taken"}, {"claims", "task_finished"},
	{"claims", "task_finished_session"}, {"claims", "task_seen_epoch"}, {"claims", "task_seen_revision"},
	{"claims", "task_take_state"}, {"claims", "task_written_epoch"}, {"claims", "task_written_revision"},
	{"pending_task_deliveries", "number"}, {"pending_task_deliveries", "origin"},
	{"pending_task_deliveries", "taken_text"}, {"pending_task_deliveries", "turn_started"},
	{"pending_task_deliveries", "take_told"},
}

// columnStands is whether table has column in the store's schema.
func columnStands(t *testing.T, store *Store, table, column string) bool {
	t.Helper()
	var standing bool
	if err := store.pool.QueryRow(context.Background(), `select exists (select 1 from information_schema.columns
		where table_schema = current_schema() and table_name = $1 and column_name = $2)`, table, column).Scan(&standing); err != nil {
		t.Fatalf("read whether %s.%s stands: %v", table, column, err)
	}
	return standing
}

// taskNumbersFormat is the one line taskNumbersOf and taskNumbersLine both print a claim's nine
// task columns in.
const taskNumbersFormat = "written %d taken %d finished %d in %q, seen %q/%d take %q, wrote %q/%d"

// taskNumbersOf is the claim's nine task columns as the store holds them, in one line a failure
// prints whole.
func taskNumbersOf(t *testing.T, store *Store, token string) string {
	t.Helper()
	var written, taken, finished, seenRevision, writtenRevision int64
	var session, seenEpoch, takeState, writtenEpoch string
	if err := store.pool.QueryRow(context.Background(), `select task_written, task_taken, task_finished,
		task_finished_session, task_seen_epoch, task_seen_revision, task_take_state, task_written_epoch,
		task_written_revision from claims where token = $1`, token).Scan(&written, &taken, &finished, &session,
		&seenEpoch, &seenRevision, &takeState, &writtenEpoch, &writtenRevision); err != nil {
		t.Fatalf("read the task numbers of %s: %v", token, err)
	}
	return fmt.Sprintf(taskNumbersFormat,
		written, taken, finished, session, seenEpoch, seenRevision, takeState, writtenEpoch, writtenRevision)
}

// taskNumbersLine is taskNumbersOf's line for a claim whose epochs and revisions are empty, which
// is every claim the fill writes.
func taskNumbersLine(written, taken, finished int, session string) string {
	return fmt.Sprintf(taskNumbersFormat, written, taken, finished, session, "", 0, "", "", 0)
}

// taskNumbersProcess is the locator of a claim whose process stands, as the store encodes it: the
// tmux pane tmuxClaim records, under token.
func taskNumbersProcess(t *testing.T, token string) []byte {
	t.Helper()
	encoded, err := json.Marshal(tmuxClaim(claim.Token(token)).Locator)
	if err != nil {
		t.Fatalf("encode the locator of %s: %v", token, err)
	}
	return encoded
}

// taskNumbersMoment is at(minute) as a nullable column takes it, and null for minute 0.
func taskNumbersMoment(minute int) *time.Time {
	if minute == 0 {
		return nil
	}
	moment := at(minute)
	return &moment
}

// 20261113175649 numbers the task each claim an earlier release left holds as its first. The fill
// is read on the six shapes it decides between: a claim holding a pending task nobody has taken
// yet (task 1 written), one whose process stands — it records a locator, is ready, idle or
// working, and its agent said it was ready no later than the acknowledgement — holding a task that
// process acknowledged and started no turn for (task 1 written and taken, the take awaiting its
// turn with the task's text, accepted at the acknowledgement), one holding an acknowledged task no
// standing process acknowledged (task 1 written and owed, nothing of it taken), one holding a task
// whose turn started (task 1 written and taken, its turn started), one holding none that served a
// task — a run, or only a phase entry — (task 1 written, taken and finished in the session it
// holds), one holding none that served a run and then abandoned that session for one that has
// taken nothing yet (task 1 written, taken and finished in the session it abandoned, since the
// session it holds finished nothing), and one that was given nothing (no number at all). The
// pending claim that has also served a run is the case that tells the two rules apart: the row it
// holds is its task, so its finished number stays 0. The claim whose new session has taken a task
// already — the session it abandoned is told, which only an accepted take of the new session
// marks — finished task 1 in the session it holds, as every other served claim does. The
// acknowledged task is read back through the store's own claim read
// too, where the claim counts as in a turn (supervise.Claim.InTurn) while it is ready: the take
// awaits its turn, so the turn the old process starts is that take's and a relaunch before one
// gives the take back. The acknowledged tasks no standing process acknowledged are six: on a claim
// suspended, one queued, one launching — each recording a locator and a ready before the
// acknowledgement, so its state alone decides — one ready that records no locator, one ready whose
// agent said it was ready at minute 6, after the acknowledgement at minute 5, so the process that
// acknowledged the prompt is gone and the one that replaced it never held it, and one idle that
// records no ready at all, so nothing dates its process. Each is left owed and read back as a task
// no take holds, so the next process takes it afresh, with the working copy's adoption, the notes
// it is owed and the confirmation an accepted take makes; filled as a take, it would be answered
// to that process from the record, with the stored text and nothing applied (supervise's
// answeredFromRecord). The task whose turn started is the one whose take told what its claim's
// records name it as having told (toldBy): that release gave a delivery a new id at every
// take-back, so its take marks the record of the let-go its confirmation told (take_told), and not
// the abandoned session a turn under an earlier id of the task told, and reads back so
// (supervise.Delivery.TakeTold).
//
// The negative controls are the write of a task number before the migration, which the database
// refuses for a column it does not have; for the acknowledged task, the fill without its
// statement for acknowledged rows, under which that task reads as nobody's take, which is how a
// prompt the old worker held came to run twice; for the acknowledged tasks no standing process
// acknowledged, the fill without its condition on the claim, under which each of them reads as a
// take accepted at the acknowledgement; for the claim whose new session has taken nothing, the
// fill of every served claim's finish in the session it holds, under which that claim reads as
// finished in its new session, so the resume rule gives that session no task (workflow's
// finishedInItsSession); and for the records told, the fill without its statement
// for take_told, under which the take that told the record marks nothing, so a give-back of it
// after the cutover untells nothing and the sentence is never told again. After the migration the
// three read marks of the push path are gone, so a reader of this record finds one model and not
// two.
func TestMigrationTaskNumbersFillsEachClaimsFirstTask(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskNumbersVersion-1)

	claimRow := `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		locator, last_ready, launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation,
		served_entry)
		values ($1, 'legion', 'LEGION-208', $2, $3, 1, $4, '', $5, $8, $9, 0, 0, 0, 0, $6, $7)`
	// readyAt is the minute the agent of the claim's process said it was ready, 0 for none.
	for _, seed := range []struct {
		token, issue, role, session, state string
		serving, entry                     int64
		process                            bool
		readyAt                            int
	}{
		{"legion-LEGION-301-implementer", "LEGION-301", "implementer", "ses_queued", "ready", 1, 1, false, 4},
		{"legion-LEGION-302-implementer", "LEGION-302", "implementer", "ses_taken", "working", 0, 0, false, 4},
		{"legion-LEGION-303-implementer", "LEGION-303", "implementer", "ses_run", "idle", 2, 0, false, 4},
		{"legion-LEGION-304-tester", "LEGION-304", "tester", "ses_entry", "idle", 0, 3, false, 4},
		{"legion-LEGION-305-reviewer", "LEGION-305", "reviewer", "ses_nothing", "ready", 0, 0, false, 4},
		{"legion-LEGION-306-implementer", "LEGION-306", "implementer", "ses_acknowledged", "ready", 0, 0, true, 4},
		{"legion-LEGION-307-implementer", "LEGION-307", "implementer", "ses_suspended", "suspended", 0, 0, true, 4},
		{"legion-LEGION-308-implementer", "LEGION-308", "implementer", "ses_requeued", "queued", 0, 0, true, 4},
		{"legion-LEGION-309-implementer", "LEGION-309", "implementer", "ses_launching", "launching", 0, 0, true, 4},
		{"legion-LEGION-310-implementer", "LEGION-310", "implementer", "ses_unlocated", "ready", 0, 0, false, 4},
		{"legion-LEGION-311-implementer", "LEGION-311", "implementer", "ses_relaunched", "ready", 0, 0, true, 6},
		{"legion-LEGION-312-implementer", "LEGION-312", "implementer", "ses_undated", "idle", 0, 0, true, 0},
		{"legion-LEGION-313-implementer", "LEGION-313", "implementer", "ses_fresh", "idle", 2, 0, false, 4},
		{"legion-LEGION-314-implementer", "LEGION-314", "implementer", "ses_fresh_told", "idle", 2, 0, false, 4},
	} {
		var locator []byte
		if seed.process {
			locator = taskNumbersProcess(t, seed.token)
		}
		if _, err := store.pool.Exec(ctx, claimRow, seed.token, seed.issue, seed.role, seed.session, seed.state,
			seed.serving, seed.entry, locator, taskNumbersMoment(seed.readyAt)); err != nil {
			t.Fatalf("record %s before the migration: %v", seed.token, err)
		}
	}
	// The pending rows as the push path leaves them: the first queued and never sent, the second
	// acknowledged at minute 5 and waiting for its turn, the third acknowledged at minute 5 and
	// confirmed at minute 6 by the turn its send started, and the last six acknowledged at minute 5
	// by no process that stands to start the turn.
	const acknowledgedAt, confirmedAt = 5, 6
	for _, seed := range []struct {
		token, id               string
		acknowledged, confirmed bool
	}{
		{"legion-LEGION-301-implementer", "outbox:31", false, false},
		{"legion-LEGION-306-implementer", "outbox:36", true, false},
		{"legion-LEGION-302-implementer", "outbox:32", true, true},
		{"legion-LEGION-307-implementer", "outbox:37", true, false},
		{"legion-LEGION-308-implementer", "outbox:38", true, false},
		{"legion-LEGION-309-implementer", "outbox:39", true, false},
		{"legion-LEGION-310-implementer", "outbox:40", true, false},
		{"legion-LEGION-311-implementer", "outbox:41", true, false},
		{"legion-LEGION-312-implementer", "outbox:42", true, false},
	} {
		if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase,
			generation, queued_at, delivered_at, marked_by, prompted, confirmed_at)
			values ($1, $2, 'Implement the change.', 'implementing', 1, $5, case when $3 then $6::timestamptz end,
			case when $3 then $2 else '' end, $3, case when $4 then $7::timestamptz end)`,
			seed.token, seed.id, seed.acknowledged, seed.confirmed, at(1), at(acknowledgedAt), at(confirmedAt)); err != nil {
			t.Fatalf("record the pending task of %s before the migration: %v", seed.token, err)
		}
	}
	// What that release marked told names the delivery whose confirmation told it (toldBy), under the
	// id the delivery had then: the confirmed task's turn told what the last let-go of its claim's
	// process ended, and a turn of the same task under an earlier id, cut off since, told the
	// session the claim abandoned.
	if _, err := store.pool.Exec(ctx, `update claims set
		ended_work = '{"at": "2026-11-13T10:00:00Z", "cause": "its pod was replaced", "generation": 1, "reported": true, "told": true, "toldBy": "outbox:32"}',
		abandoned_session = '{"session": "ses_old", "why": "forget-session", "at": "2026-11-13T09:00:00Z", "told": true, "toldBy": "5ZK7XQ3V4YVBXNQ6"}'
		where token = 'legion-LEGION-302-implementer'`); err != nil {
		t.Fatalf("record what the confirmed task's claim told before the migration: %v", err)
	}
	// The session each of the last two claims abandoned: the first claim's new session has taken
	// nothing, so that record is untold; the second's has taken a task, whose accepted take told it.
	if _, err := store.pool.Exec(ctx, `update claims set
		abandoned_session = '{"session": "ses_abandoned", "why": "forget-session", "at": "2026-11-13T09:00:00Z"}'
		where token = 'legion-LEGION-313-implementer'`); err != nil {
		t.Fatalf("record the untold abandoned session before the migration: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `update claims set
		abandoned_session = '{"session": "ses_abandoned", "why": "forget-session", "at": "2026-11-13T09:00:00Z", "told": true, "toldBy": "outbox:44"}'
		where token = 'legion-LEGION-314-implementer'`); err != nil {
		t.Fatalf("record the told abandoned session before the migration: %v", err)
	}

	_, err := store.pool.Exec(ctx, "update claims set task_written = 1")
	if err == nil || !strings.Contains(err.Error(), "task_written") {
		t.Fatalf("writing a task number before the migration = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the task number write is refused: %v", err)

	// The fixture stands just below this migration, so the run applies it and every embedded
	// migration above it — at this release the thirty-one recorded after it,
	// 20261113181500_architect_stalled_notices, 20261113182000_architect_entry_upgrade,
	// 20261114020537_claim_entry_handshake, 20261114031356_merge_queue_removal,
	// 20261114061900_phase_entry_verbatim, 20261115194357_project_pause,
	// 20261116144907_claim_sessions,
	// 20261116165926_role_holders_publish,
	// 20261116234034_phase_reviewed_head, 20261117000528_outbox_kinds_with_the_step_record,
	// 20261117004117_step_starts, 20261117023932_outbox_kinds_with_the_plan_record,
	// 20261117034837_issue_plans, 20261117064152_step_starts_roll_forward,
	// 20261117112334_pull_request_statuses,
	// 20261117200337_update_ask_marks_at_delivery,
	// 20261119003157_conflict_reads_and_merge_forward, 20261119203047_design_news (whose four
	// columns of pending_task_deliveries stand beside this one's five, and whose functions read
	// confirmed_at, which this one keeps), 20261120012547_set_back_observed_seq,
	// 20261120042902_claim_agent_resume_generation,
	// 20261120055717_dispatch_issue_value_sequences,
	// 20261120144241_intake_failures (which adds the intake's three tables),
	// 20261121170714_dispatch_issue_applied_seq (which adds dispatch_issues.applied_seq at its
	// default),
	// 20261122160000_design_news_credit_at_turn_start (which writes design_news_told_in again to
	// read turn_started, a column this migration adds),
	// 20261123050533_idle_release_keeps_no_workspace (which drops workspace_bundles and adds
	// idle_release_declines and issue_releases.turn_start),
	// 20261123165139_experiment_assignment_plan (which adds experiment_assignments.plan at its
	// default), 20261123212127_claim_start_failure_after_ready (which empties
	// claims.last_start_failure on every claim stored ready, working or idle),
	// 20261123224717_members_owed (which adds issues.members_owed and marks the roots whose
	// re-admitted generation no promotion has opened), 20261124060500_issue_priority (which adds
	// issues.priority), 20261124073512_claim_cause_times (which adds the claims' two cause times and
	// their digests) and 20261124143026_issue_release_teardown (which adds issue_releases.teardown
	// and pod_releases.issue_release_id), which run after its fill and touch no column
	// the fill writes or this test reads — and nothing else. So the fill runs here before the
	// handshake's columns stand, as it does on a store migrated in version order, and confirms no
	// handshake (TestMigrationTaskNumbersConfirmsTheHandshakeOfATakeItFills reads the upgrade from
	// the release before this one, which holds them).
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	want := 0
	for _, migration := range all {
		if migration.Version >= taskNumbersVersion {
			want++
		}
	}
	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate through the task numbers: %v", err)
	}
	if applied != want {
		t.Fatalf("the migration applied %d migrations, want %d: this one and those above it; every other is the fixture's own", applied, want)
	}

	for _, want := range []struct {
		token, line, why string
	}{
		{"legion-LEGION-301-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task nobody has taken is task 1 written, whatever run the claim served before it"},
		{"legion-LEGION-306-implementer", taskNumbersLine(1, 1, 0, ""),
			"a pending task its worker acknowledged is task 1 written and taken, the take awaiting its turn"},
		{"legion-LEGION-302-implementer", taskNumbersLine(1, 1, 0, ""),
			"a pending task whose turn started is task 1 written and taken, and not finished while that turn runs"},
		{"legion-LEGION-303-implementer", taskNumbersLine(1, 1, 1, "ses_run"),
			"a claim that served a run and holds no task finished task 1 in the session it holds"},
		{"legion-LEGION-304-tester", taskNumbersLine(1, 1, 1, "ses_entry"),
			"a claim that served a phase entry and holds no task finished task 1 in the session it holds"},
		{"legion-LEGION-313-implementer", taskNumbersLine(1, 1, 1, "ses_abandoned"),
			"a claim that served a run and then abandoned that session for one that has taken nothing finished task 1 in the session it abandoned, so the new session is owed its resume task"},
		{"legion-LEGION-314-implementer", taskNumbersLine(1, 1, 1, "ses_fresh_told"),
			"a claim whose new session has taken a task since it abandoned the old one finished task 1 in the session it holds"},
		{"legion-LEGION-305-reviewer", taskNumbersLine(0, 0, 0, ""),
			"a claim that was given nothing has no number"},
		{"legion-LEGION-307-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task acknowledged on a suspended claim is task 1 written and owed, since no process stands to start its turn"},
		{"legion-LEGION-308-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task acknowledged on a queued claim is task 1 written and owed, since no process stands to start its turn"},
		{"legion-LEGION-309-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task acknowledged on a launching claim is task 1 written and owed, since no process stands to start its turn"},
		{"legion-LEGION-310-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task acknowledged on a ready claim that records no process is task 1 written and owed"},
		{"legion-LEGION-311-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task a process acknowledged before the claim's process said it was ready is task 1 written and owed: that process never held it"},
		{"legion-LEGION-312-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task acknowledged on a claim that records no ready is task 1 written and owed: nothing says its process acknowledged it"},
	} {
		if got := taskNumbersOf(t, store, want.token); got != want.line {
			t.Errorf("%s after the migration reads %s, want %s: %s", want.token, got, want.line, want.why)
		}
	}
	for _, want := range []struct {
		token, taken, why string
		confirmed         int
		started           bool
		told              []string
	}{
		{"legion-LEGION-301-implementer", "", "no take of a task nobody took", 0, false, nil},
		{"legion-LEGION-306-implementer", "Implement the change.",
			"a take accepted at the acknowledgement, answered with the row's own text, its turn not started, which told nothing", acknowledgedAt, false, nil},
		{"legion-LEGION-302-implementer", "", "a take whose turn started, its text kept by no record, which told the let-go's record its claim names it by",
			confirmedAt, true, []string{"ended"}},
		{"legion-LEGION-307-implementer", "", "no take of a prompt no process stands to run, on a suspended claim", 0, false, nil},
		{"legion-LEGION-308-implementer", "", "no take of a prompt no process stands to run, on a queued claim", 0, false, nil},
		{"legion-LEGION-309-implementer", "", "no take of a prompt no process stands to run, on a launching claim", 0, false, nil},
		{"legion-LEGION-310-implementer", "", "no take of a prompt no process stands to run, on a ready claim that records none", 0, false, nil},
		{"legion-LEGION-311-implementer", "", "no take of a prompt the process before the claim's own acknowledged", 0, false, nil},
		{"legion-LEGION-312-implementer", "", "no take of a prompt no ready dates the claim's process against", 0, false, nil},
	} {
		var number int64
		var origin string
		var taken string
		var started bool
		var confirmed *time.Time
		var told []string
		if err := store.pool.QueryRow(ctx, `select number, origin, taken_text, turn_started, confirmed_at, take_told
			from pending_task_deliveries where claim_token = $1`, want.token).Scan(&number, &origin, &taken, &started, &confirmed, &told); err != nil {
			t.Fatalf("read the pending task of %s after the migration: %v", want.token, err)
		}
		if number != 1 || origin != "" || taken != want.taken || started != want.started || !atMinute(confirmed, want.confirmed) || !slices.Equal(told, want.told) {
			t.Errorf("the pending task of %s = number %d, origin %q, taken text %q, turn started %v, confirmed at %v, told %q; want task 1 of no recorded origin, taken text %q, turn started %v, confirmed at minute %d (0 for none), told %q: %s",
				want.token, number, origin, taken, started, confirmed, told, want.taken, want.started, want.confirmed, want.told, want.why)
		}
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims after the migration: %v", err)
	}
	if c := claimNamed(t, claims, "legion-LEGION-306-implementer"); c.Pending == nil || c.TaskTaken != 1 || c.Pending.TakenText != c.Pending.Task ||
		c.Pending.TurnStarted || !c.InTurn() {
		t.Errorf("the acknowledged claim reads back as %+v holding %+v, want task 1 taken, the take awaiting its turn with the task's text, so the ready claim counts as in a turn",
			c, c.Pending)
	}
	started := claimNamed(t, claims, "legion-LEGION-302-implementer").Pending
	if started == nil || started.TakeTold != (supervise.Sentences{Ended: true}) {
		t.Errorf("the claim whose task's turn started reads back holding %+v, want its take to have told the let-go's record and not the abandoned session",
			started)
	}
	// The confirmed row's take reads as one whose turn started, with no text kept, as it read when
	// a confirmed take with no text was read so: the fill set its turn_started, which is all the
	// claim reads of where the take stands (supervise's Delivery.startRecorded). On an idle claim
	// such a take no longer works its task; one awaiting its turn would.
	if started == nil || started.TakenText != "" || !started.TurnStarted ||
		started.StillWorked(started.Generation, started.Phase, supervise.StateIdle) {
		t.Errorf("the claim whose task's turn started reads back holding %+v, want its take's turn started with no text kept, so an idle claim no longer works it",
			started)
	}
	// The next process of each of these takes the task afresh: the claim read holds it as a task no
	// take holds, so the take is decided by the claim's machine and not answered from the record.
	for _, token := range []claim.Token{"legion-LEGION-307-implementer", "legion-LEGION-308-implementer",
		"legion-LEGION-309-implementer", "legion-LEGION-310-implementer", "legion-LEGION-311-implementer",
		"legion-LEGION-312-implementer"} {
		if c := claimNamed(t, claims, token); c.Pending == nil || c.Pending.Number != 1 || c.TaskTaken != 0 || !c.Pending.ConfirmedAt.IsZero() ||
			c.Pending.TakenText != "" {
			t.Errorf("%s reads back as %+v holding %+v, want task 1 owed and no take of it: no process stood to run the prompt it was sent",
				token, c, c.Pending)
		}
	}

	for _, mark := range taskNumbersReadMarks {
		if columnStands(t, store, "pending_task_deliveries", mark) {
			t.Errorf("pending_task_deliveries.%s stands after the migration, want it dropped: a task the worker takes is acknowledged by nothing", mark)
		}
	}
	// The take state holds the four the record names and no other.
	if _, err := store.pool.Exec(ctx, "update claims set task_take_state = $1 where token = 'legion-LEGION-305-reviewer'",
		string(supervise.TakeMade)); err != nil {
		t.Fatalf("recording a take made at the seen revision: %v", err)
	}
	_, err = store.pool.Exec(ctx, "update claims set task_take_state = 'taken' where token = 'legion-LEGION-305-reviewer'")
	if err == nil || !strings.Contains(err.Error(), "task_take_state") {
		t.Fatalf("recording the take state 'taken' = %v, want the check to refuse a state the record does not name", err)
	}
	// The take's mark holds the two records the claim keeps and no other.
	if _, err := store.pool.Exec(ctx, "update pending_task_deliveries set take_told = '{abandoned,ended}' where claim_token = 'legion-LEGION-302-implementer'"); err != nil {
		t.Fatalf("recording a take that told both records: %v", err)
	}
	_, err = store.pool.Exec(ctx, "update pending_task_deliveries set take_told = '{relaunch}' where claim_token = 'legion-LEGION-302-implementer'")
	if err == nil || !strings.Contains(err.Error(), "take_told") {
		t.Fatalf("recording the records told {relaunch} = %v, want the check to refuse a record the claim does not keep", err)
	}
}

// An accepted take confirms its claim's handshake where the handshake names the task's entry and
// run and no take of that entry stands confirmed yet (supervise.EntryHandshake), so the take the
// fill makes of an acknowledged prompt does the same, at the take's own moment: the state then
// serves entryConfirmedAt beside entryQueuedAt, and a give-back of the take unconfirms the
// handshake again, since the two times are one (EntryHandshake.unconfirmed compares them). The
// store is the one the release before this one leaves, every migration recorded but this release's
// two (branchOnlyMigrations), so the handshake's columns, which
// 20261114020537_claim_entry_handshake adds under a later version, stand when the fill runs, as at
// every upgrade from that release. Four claims each hold a prompt
// of run 1 their process, ready since minute 2, acknowledged at minute 5 and started no turn for,
// beside a handshake of entry 2 of run 1 queued at minute 1: an idle claim whose prompt is of that
// entry, whose take confirms the handshake at minute 5; a suspended claim whose prompt is of that
// entry, which holds no take and so confirms nothing; a ready claim whose prompt is of no entry, an
// operator's task, which is no part of the handshake; and an idle claim whose handshake an earlier
// turn of that entry confirmed at minute 3, which keeps that first confirmation.
//
// The negative control is the fill without its statement for the handshake, under which the first
// claim's take stands accepted beside a handshake queued and never confirmed.
func TestMigrationTaskNumbersConfirmsTheHandshakeOfATakeItFills(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	before := slices.DeleteFunc(slices.Clone(all), func(m migrations.Migration) bool { return slices.Contains(branchOnlyMigrations, m.Version) })
	if _, err := store.migrate(ctx, before); err != nil {
		t.Fatalf("migrate every embedded migration but %v, as the release before this one leaves the store: %v", branchOnlyMigrations, err)
	}
	const queuedAt, readyAt, earlierTurnAt, acknowledgedAt = 1, 2, 3, 5
	for _, seed := range []struct {
		token, issue, id, state string
		process                 bool
		entry                   int64
		confirmedAt             int
	}{
		{"legion-LEGION-401-implementer", "LEGION-401", "outbox:41", "idle", true, 2, 0},
		{"legion-LEGION-402-implementer", "LEGION-402", "outbox:42", "suspended", false, 2, 0},
		{"legion-LEGION-403-implementer", "LEGION-403", "outbox:43", "ready", true, 0, 0},
		{"legion-LEGION-404-implementer", "LEGION-404", "outbox:44", "idle", true, 2, earlierTurnAt},
	} {
		var locator []byte
		if seed.process {
			locator = taskNumbersProcess(t, seed.token)
		}
		if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, locator, last_ready, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
			handshake_entry, handshake_generation, handshake_queued_at, handshake_confirmed_at)
			values ($1, 'legion', 'LEGION-400', $2, 'implementer', 1, 'ses_implementer', '', $3, $4, $5, 0, 0, 0, 0, 2, 1, $6, $7)`,
			seed.token, seed.issue, seed.state, locator, at(readyAt), at(queuedAt), taskNumbersMoment(seed.confirmedAt)); err != nil {
			t.Fatalf("record %s before the migration: %v", seed.token, err)
		}
		if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase,
			generation, entry, queued_at, delivered_at, marked_by, prompted)
			values ($1, $2, 'Implement the change.', 'implementing', 1, $3, $4, $5, $2, true)`,
			seed.token, seed.id, seed.entry, at(queuedAt), at(acknowledgedAt)); err != nil {
			t.Fatalf("record the acknowledged prompt of %s before the migration: %v", seed.token, err)
		}
	}

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate the store the release before this one leaves: %v", err)
	}
	if applied != len(branchOnlyMigrations) {
		t.Fatalf("the migration applied %d migrations, want this release's %d, %v: the release before this one recorded every other", applied, len(branchOnlyMigrations), branchOnlyMigrations)
	}
	for _, want := range []struct {
		token     string
		confirmed int
		why       string
	}{
		{"legion-LEGION-401-implementer", acknowledgedAt,
			"the take the fill made of a prompt of the handshake's entry confirms it at the take's own moment"},
		{"legion-LEGION-402-implementer", 0, "a prompt no process stands to run is no take, and confirms nothing"},
		{"legion-LEGION-403-implementer", 0, "a take of a task of no entry is no part of the handshake"},
		{"legion-LEGION-404-implementer", earlierTurnAt, "the handshake keeps the first confirmation of its entry"},
	} {
		var confirmed *time.Time
		if err := store.pool.QueryRow(ctx, "select handshake_confirmed_at from claims where token = $1",
			want.token).Scan(&confirmed); err != nil {
			t.Fatalf("read the handshake of %s after the migration: %v", want.token, err)
		}
		if !atMinute(confirmed, want.confirmed) {
			t.Errorf("the handshake of %s is confirmed at %v after the migration, want minute %d (0 for never): %s",
				want.token, confirmed, want.confirmed, want.why)
		}
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims after the migration: %v", err)
	}
	if c := claimNamed(t, claims, "legion-LEGION-401-implementer"); c.Pending == nil || c.Pending.ConfirmedAt.IsZero() || c.Handshake.Entry != 2 ||
		c.Handshake.Generation != 1 || !c.Handshake.QueuedAt.Equal(at(queuedAt)) || !c.Handshake.ConfirmedAt.Equal(c.Pending.ConfirmedAt) {
		t.Errorf("the claim whose take the fill made reads back with the handshake %+v beside %+v, want entry 2 of run 1 queued at minute %d and confirmed at its take's own moment, which a give-back of the take compares",
			c.Handshake, c.Pending, queuedAt)
	}
}

// claimNamed is the claim of claims whose token is token, failing t where they hold none.
func claimNamed(t *testing.T, claims []supervise.Claim, token claim.Token) supervise.Claim {
	t.Helper()
	i := slices.IndexFunc(claims, func(c supervise.Claim) bool { return c.Token == token })
	if i < 0 {
		t.Fatalf("the claims read after the migration hold no %s: %+v", token, claims)
	}
	return claims[i]
}

// atMinute is whether moment, a nullable column read back, is at(minute), or null for minute 0.
func atMinute(moment *time.Time, minute int) bool {
	if minute == 0 {
		return moment == nil
	}
	return moment != nil && moment.Equal(at(minute))
}

// Every task column round-trips through the store across a restart: a new pool and a new store
// over the same database, with nothing of the first one carried in memory (reopenStore). The claim
// carries every one of its nine task fields and its pending task its number, its origin, the
// text its accepted take was answered with, whether that take's turn started and which of the
// claim's two records it told, which is what lets a take sent again after a lost answer, or after
// a daemon restart, be answered the same text without being taken a second time, before the
// take's turn starts and after, and a give-back of the take after a restart untell what that take
// told and nothing else. The text is stored empty while no take has been accepted, and the
// records told as none, and two numbers no bigint holds are refused before anything is written.
func TestTaskNumberSurvivesAStoreRestartWithEveryColumn(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.State = supervise.StateWorking
	want.TaskWritten, want.TaskTaken, want.TaskFinished, want.TaskFinishedSession = 7, 7, 6, "ses_implementer"
	want.TaskSeenRevision, want.TaskTakeState = supervise.TaskRevision{Epoch: "2026-11-13T17:56:49.123456789Z", Revision: 41}, supervise.TakeAccepted
	want.TaskWrittenRevision = supervise.TaskRevision{Epoch: "2026-11-13T17:56:49.123456789Z", Revision: 42}
	want.Pending = &supervise.Delivery{
		ID: "outbox:71", Task: "Implement the change. Issue: LEGION-209. Phase: implementing.",
		Phase: phase.Implementing, Generation: 4, Entry: 3, QueuedAt: at(1), ConfirmedAt: at(2),
		Number: 7, Origin: supervise.OriginPhaseStart,
		TakenText:   "Your last turn was interrupted.\n\nImplement the change. Issue: LEGION-209. Phase: implementing.",
		TurnStarted: true,
		TakeTold:    supervise.Sentences{Abandoned: true, Ended: true},
	}
	if err := store.PutClaimAndDelivery(ctx, want, *want.Pending); err != nil {
		t.Fatalf("put the claim and its taken task: %v", err)
	}
	sameClaim(t, onlyClaim(t, reopenStore(t, store)), want)

	t.Run("a task no take has been accepted for holds no text and told nothing", func(t *testing.T) {
		// The claim's next task, queued under the next number and not yet taken: nothing has
		// confirmed it, and no take of it has been answered.
		queued := *want.Pending
		queued.ID, queued.Number, queued.Origin, queued.TakenText = "outbox:72", 8, supervise.OriginWake, ""
		queued.ConfirmedAt, queued.TurnStarted, queued.TakeTold = time.Time{}, false, supervise.Sentences{}
		if err := store.PutDelivery(ctx, want.Token, queued); err != nil {
			t.Fatalf("put the queued task: %v", err)
		}
		var stored string
		var told []string
		if err := store.pool.QueryRow(ctx, "select taken_text, take_told from pending_task_deliveries where claim_token = $1",
			string(want.Token)).Scan(&stored, &told); err != nil {
			t.Fatalf("read the queued task's taken text and records told: %v", err)
		}
		if stored != "" || len(told) != 0 {
			t.Errorf("a task no take has been accepted for stores the taken text %q and the records told %q, want none of either", stored, told)
		}
		held := want
		held.Pending = &queued
		sameClaim(t, onlyClaim(t, reopenStore(t, store)), held)
	})

	t.Run("a number no bigint holds is refused", func(t *testing.T) {
		before := onlyClaim(t, store)
		over := before
		over.TaskWritten = math.MaxUint64
		if err := store.PutClaim(ctx, over); err == nil || !strings.Contains(err.Error(), "does not fit a bigint") {
			t.Fatalf("put a claim whose task number no bigint holds = %v, want it refused", err)
		}
		pending := *before.Pending
		pending.Number = math.MaxUint64
		if err := store.PutDelivery(ctx, before.Token, pending); err == nil || !strings.Contains(err.Error(), "does not fit a bigint") {
			t.Fatalf("put a task whose number no bigint holds = %v, want it refused", err)
		}
		sameClaim(t, onlyClaim(t, store), before)
	})
}

// taskNumbersWayBackOfTheHeader is the recovery block of the migration's header as an operator
// copies it out of the file: the psql line, the heredoc's lines and its terminator, with the
// comment marker and the header's own indent taken off each. It is read from the embedded
// migration, which is the file's bytes as the binary ships them. A header with no block, a block
// never closed, and a line indented outside the opener's indent each stop the test by name,
// because a check that cannot find what it reads passes while reading nothing.
func taskNumbersWayBackOfTheHeader(t *testing.T) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	i := slices.IndexFunc(all, func(m migrations.Migration) bool { return m.Version == taskNumbersVersion })
	if i < 0 {
		t.Fatalf("the embedded migrations hold no migration %d: %s is the source of its way back", taskNumbersVersion, taskNumbersFile)
	}
	var block []string
	indent := ""
	for _, line := range strings.Split(all[i].SQL, "\n") {
		comment, isComment := strings.CutPrefix(line, "--")
		if !isComment {
			break
		}
		if len(block) == 0 {
			if strings.TrimSpace(comment) == taskNumbersHeredoc {
				indent = comment[:len(comment)-len(strings.TrimLeft(comment, " "))]
				block = append(block, taskNumbersHeredoc)
			}
			continue
		}
		text := ""
		if strings.TrimSpace(comment) != "" {
			held, indented := strings.CutPrefix(comment, indent)
			if !indented {
				t.Fatalf("%s indents %q inside the way back's block below its %q line's indent: the block is read by that indent",
					taskNumbersFile, line, taskNumbersHeredoc)
			}
			text = held
		}
		block = append(block, text)
		if text == taskNumbersTerminator {
			return strings.Join(block, "\n") + "\n"
		}
	}
	if len(block) == 0 {
		t.Fatalf("%s carries no %q line in its header: that line opens the way back's block", taskNumbersFile, taskNumbersHeredoc)
	}
	t.Fatalf("%s opens the way back's block and closes it with no %q line; what it carries is:\n%s",
		taskNumbersFile, taskNumbersTerminator, strings.Join(block, "\n"))
	return ""
}

// taskNumbersWayBack is the transaction of that block, what psql reads on its standard input. Its
// statements are counted, so a block that lost one or grew one while the header and the guide
// still call them the twenty-two stops the test; the count splits on the semicolon, which no
// statement of this block carries inside a literal.
func taskNumbersWayBack(t *testing.T, block string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if len(lines) < 2 || lines[0] != taskNumbersHeredoc || lines[len(lines)-1] != taskNumbersTerminator {
		t.Fatalf("the way back's block of %s is no heredoc between %q and %q:\n%s",
			taskNumbersFile, taskNumbersHeredoc, taskNumbersTerminator, block)
	}
	recovery := strings.Join(lines[1:len(lines)-1], "\n") + "\n"
	var statements []string
	for _, statement := range strings.Split(recovery, ";") {
		if held := strings.TrimSpace(statement); held != "" {
			statements = append(statements, held)
		}
	}
	if len(statements) < 2 || statements[0] != "begin" || statements[len(statements)-1] != "commit" {
		t.Fatalf("the way back's block of %s is not one transaction, begin to commit: either half of it left standing is a broken store:\n%s",
			taskNumbersFile, recovery)
	}
	if held := len(statements) - 2; held != taskNumbersWayBackStatements {
		t.Fatalf("the way back's block of %s holds %d statements between its begin and its commit, want the %d that file and %s call the twenty-two:\n%s",
			taskNumbersFile, held, taskNumbersWayBackStatements, taskNumbersGuide, recovery)
	}
	return recovery
}

// taskNumbersWayBackOfTheGuide is the same block as docs/kubernetes.md carries it: the fenced
// block that deletes this migration's version row, found by that one line rather than by its
// place in the guide. The guide is at the repository root, four directories above this package;
// a read that fails stops the test, since an absence would read exactly like a pass.
func taskNumbersWayBackOfTheGuide(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", taskNumbersGuide))
	if err != nil {
		t.Fatalf("read %s: %v", taskNumbersGuide, err)
	}
	mark := fmt.Sprintf("delete from schema_version where version = %d;", taskNumbersVersion)
	var block []string
	inside := false
	for _, line := range strings.Split(string(doc), "\n") {
		switch {
		case !strings.HasPrefix(line, "```"):
			if inside {
				block = append(block, line)
			}
		case !inside:
			inside, block = true, nil
		default:
			if slices.Contains(block, mark) {
				return strings.Join(block, "\n") + "\n"
			}
			inside = false
		}
	}
	t.Fatalf("%s carries no fenced block holding %q: that block is the way back an operator runs, and it is held to the block %s carries in its header",
		taskNumbersGuide, mark, taskNumbersFile)
	return ""
}

// The way back is one text in two files: the header, which ships with the migration, and the
// guide, which is where an operator reads it with the daemon stopped. A copy that drifted is a
// wrong procedure found at a rollback. The header's block is the one the test below runs.
func TestMigrationTaskNumbersWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	header := taskNumbersWayBackOfTheHeader(t)
	taskNumbersWayBack(t, header)
	guide := taskNumbersWayBackOfTheGuide(t)
	if header == guide {
		return
	}
	headerLines := strings.Split(strings.TrimSuffix(header, "\n"), "\n")
	guideLines := strings.Split(strings.TrimSuffix(guide, "\n"), "\n")
	var difference strings.Builder
	for i := range max(len(headerLines), len(guideLines)) {
		held, copied := "", ""
		if i < len(headerLines) {
			held = headerLines[i]
		}
		if i < len(guideLines) {
			copied = guideLines[i]
		}
		if held != copied {
			fmt.Fprintf(&difference, "line %d:\n\theader: %q\n\tguide:  %q\n", i+1, held, copied)
		}
	}
	t.Fatalf("the way back's block differs between %s and %s:\n%s", taskNumbersFile, taskNumbersGuide, difference.String())
}

// The way back the header names, run as the one transaction an operator runs (the simple
// protocol, as psql sends it) over a store this release wrote, twice: the second run is the rerun
// the header says changes nothing. After it the release before this one can read its claims and
// write its deliveries again — the three read marks stand, at the defaults that read as a task it
// has queued and not yet sent — and decode every supervise row and every merge-forward row of its
// outbox: the start this release wrote has lost its `origin`, and the ask this release wrote its
// `mergeState`, `stateReadAt`, `launch` and `sentLaunch`, keys that release's strict decoder
// refuses, and each kept every other key. The failure cause this release adds, never_reconnected,
// which that release's plugins read through a strict enum that names no such word, stands as
// never_registered in both of a claim's cause columns — the pending cause of a relaunch under way
// and the failure cause of a claim whose relaunches ran out — and a cause that release names stays
// as it was. The block of
// 20261122160000_design_news_credit_at_turn_start runs first, as docs/kubernetes.md says, since
// its function reads turn_started, which this block drops. None of the fourteen columns stands,
// nor either migration's version row, while every other row stays recorded. The roll-forward then
// runs the two migrations again, and the task numbers' second fill numbers what it finds as the
// first fill numbered what an earlier release left: the task the claim held under number 7 is
// task 1 again, its turn started since a take confirmed it, and the claim that had finished task
// 4 has finished task 1, in the session it holds.
func TestMigrationTaskNumbersWayBackLetsTheReleaseBeforeReadAndReArmsTheFill(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	taken := tmuxClaim("legion-LEGION-209-implementer")
	taken.State = supervise.StateWorking
	taken.TaskWritten, taken.TaskTaken, taken.TaskTakeState = 7, 7, supervise.TakeAccepted
	taken.TaskSeenRevision = supervise.TaskRevision{Epoch: "2026-11-13T17:56:49Z", Revision: 12}
	task := supervise.Delivery{ID: "outbox:71", Task: "Implement the change.", Phase: phase.Implementing,
		Generation: 4, QueuedAt: at(1), ConfirmedAt: at(2), Number: 7, Origin: supervise.OriginPhaseStart, TakenText: "Implement the change."}
	if err := store.PutClaimAndDelivery(ctx, taken, task); err != nil {
		t.Fatalf("put the claim holding its taken task: %v", err)
	}
	finished := tmuxClaim("legion-LEGION-210-tester")
	finished.Role, finished.Issue, finished.Session = claim.RoleTester, "LEGION-210", "ses_tester"
	finished.TaskWritten, finished.TaskTaken, finished.TaskFinished, finished.TaskFinishedSession = 4, 4, 4, "ses_tester"
	if err := store.PutClaim(ctx, finished); err != nil {
		t.Fatalf("put the claim that finished its task: %v", err)
	}
	// The two cause columns as this release writes them: a claim whose relaunch is under way after
	// its restored shim never said hello again, one whose relaunches after that ran the launch
	// budget out, and one failed under a cause the release before this one names.
	relaunching := tmuxClaim("legion-LEGION-211-reviewer")
	relaunching.Role, relaunching.Issue, relaunching.Session = claim.RoleReviewer, "LEGION-211", "ses_reviewer"
	relaunching.State, relaunching.PendingCause = supervise.StateLaunching, supervise.CauseNeverReconnected
	unreconnected := tmuxClaim("legion-LEGION-212-merger")
	unreconnected.Role, unreconnected.Issue, unreconnected.Session, unreconnected.Locator = claim.RoleMerger, "LEGION-212", "ses_merger", nil
	unreconnected.State, unreconnected.Failure = supervise.StateFailed, "launch failures ran out"
	unreconnected.FailureCause = supervise.CauseNeverReconnected
	died := tmuxClaim("legion-LEGION-213-implementer")
	died.Issue, died.Session, died.Locator = "LEGION-213", "ses_died", nil
	died.State, died.Failure = supervise.StateFailed, "launch failures ran out"
	died.FailureCause, died.PendingCause = supervise.CauseDied, supervise.CauseDied
	for _, c := range []supervise.Claim{relaunching, unreconnected, died} {
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put %s, which holds the failure cause %q and the pending cause %q: %v", c.Token, c.FailureCause, c.PendingCause, err)
		}
	}
	const start = `{"op": "start", "tree": "LEGION-209", "role": "implementer", "task": "Implement the change.", "phase": "implementing", "origin": "phase-start", "generation": 4}`
	const startWithoutOrigin = `{"op": "start", "tree": "LEGION-209", "role": "implementer", "task": "Implement the change.", "phase": "implementing", "generation": 4}`
	var started int64
	if err := store.pool.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', 'LEGION-209', $1::jsonb, 0, now(), '') returning id`, start).Scan(&started); err != nil {
		t.Fatalf("enqueue the start this release writes, naming its origin: %v", err)
	}
	// The ask as this release writes it: the read of GitHub it rests on, and the launches of the
	// asked role's claim a re-send carries (record.MergeForwardMessage's Launch and SentLaunch).
	const ask = `{"role": "implementer", "step": "ask", "head": "c0de", "base": "main", "phase": "testing", "pullRequest": 42, "why": "required-check-not-green", "mergeState": "dirty", "stateReadAt": "2026-11-19T01:01:00Z", "launch": 2, "sentLaunch": 1}`
	const askWithoutTheRead = `{"role": "implementer", "step": "ask", "head": "c0de", "base": "main", "phase": "testing", "pullRequest": 42, "why": "required-check-not-green"}`
	var asked int64
	if err := store.pool.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('merge_forward', 'LEGION-209', $1::jsonb, 0, now(), '') returning id`, ask).Scan(&asked); err != nil {
		t.Fatalf("enqueue the merge-forward ask this release writes, naming the read it rests on and its launches: %v", err)
	}

	if _, err := store.pool.Exec(ctx, wayBackOf(t, creditAtTurnStartVersion, "20261122160000_design_news_credit_at_turn_start"),
		pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back of 20261122160000_design_news_credit_at_turn_start, which goes before this block: %v", err)
	}
	recovery := taskNumbersWayBack(t, taskNumbersWayBackOfTheHeader(t))
	for run := range 2 {
		if _, err := store.pool.Exec(ctx, recovery, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %d of the way back's block of %s as the one transaction an operator runs: %v", run+1, taskNumbersFile, err)
		}
	}
	var stripped, unstripped bool
	if err := store.pool.QueryRow(ctx, `select payload = $2::jsonb, payload ? 'origin' from outbox where id = $1`,
		started, startWithoutOrigin).Scan(&stripped, &unstripped); err != nil {
		t.Fatalf("read the start after the way back: %v", err)
	}
	if !stripped || unstripped {
		t.Fatalf("the start after the way back equals %s = %v and still names origin = %v; want origin taken out and every other key kept: the release before this one decodes a supervise payload strictly and refuses the key",
			startWithoutOrigin, stripped, unstripped)
	}
	var askStripped, askUnstripped bool
	if err := store.pool.QueryRow(ctx, `select payload = $2::jsonb, payload ? 'mergeState' or payload ? 'stateReadAt' or payload ? 'launch' or payload ? 'sentLaunch'
		from outbox where id = $1`, asked, askWithoutTheRead).Scan(&askStripped, &askUnstripped); err != nil {
		t.Fatalf("read the merge-forward ask after the way back: %v", err)
	}
	if !askStripped || askUnstripped {
		t.Fatalf("the ask after the way back equals %s = %v and still names mergeState, stateReadAt, launch or sentLaunch = %v; want all four taken out and every other key kept: the release before this one decodes a merge-forward payload strictly and refuses each key",
			askWithoutTheRead, askStripped, askUnstripped)
	}
	for _, want := range []struct {
		token            claim.Token
		failure, pending supervise.FailureCause
		why              string
	}{
		{relaunching.Token, "", supervise.CauseNeverRegistered,
			"a relaunch under way for a shim that never reconnected is pending under the nearest cause that release names"},
		{unreconnected.Token, supervise.CauseNeverRegistered, "",
			"a claim whose relaunches for a shim that never reconnected ran out is failed under the nearest cause that release names"},
		{died.Token, supervise.CauseDied, supervise.CauseDied, "a cause that release names stays as it was"},
	} {
		var failure, pending string
		if err := store.pool.QueryRow(ctx, "select failure_cause, pending_cause from claims where token = $1",
			string(want.token)).Scan(&failure, &pending); err != nil {
			t.Fatalf("read the causes of %s after the way back: %v", want.token, err)
		}
		if failure != string(want.failure) || pending != string(want.pending) {
			t.Errorf("%s holds the failure cause %q and the pending cause %q after the way back, want %q and %q: %s",
				want.token, failure, pending, want.failure, want.pending, want.why)
		}
	}

	var deliveredAt *string
	var markedBy string
	var prompted bool
	if err := store.pool.QueryRow(ctx, `select d.delivered_at::text, d.marked_by, d.prompted from pending_task_deliveries d
		where d.claim_token = $1`, string(taken.Token)).Scan(&deliveredAt, &markedBy, &prompted); err != nil {
		t.Fatalf("read the pending task's read marks as the release before this one reads them: %v", err)
	}
	if deliveredAt != nil || markedBy != "" || prompted {
		t.Fatalf("the pending task's read marks after the way back = %v, %q, %v; want none of them set, which that release reads as a task it has queued and not yet sent",
			deliveredAt, markedBy, prompted)
	}
	for _, added := range taskNumbersColumns {
		if columnStands(t, store, added.table, added.column) {
			t.Errorf("the way back left %s.%s standing, want it dropped: the roll-forward runs this migration again, and its first statements add that column",
				added.table, added.column)
		}
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var armed, recorded int
	if err := store.pool.QueryRow(ctx, "select count(*) filter (where version = $1::bigint), count(*) from schema_version",
		taskNumbersVersion).Scan(&armed, &recorded); err != nil {
		t.Fatalf("read the recorded versions after the way back: %v", err)
	}
	if armed != 0 || recorded != len(all)-len(branchOnlyMigrations) {
		t.Fatalf("schema_version holds %d rows of %d and %d rows in all after the way back, want none of it and the %d every migration but %v recorded: store.apply decides each migration on its own row",
			armed, taskNumbersVersion, recorded, len(all)-len(branchOnlyMigrations), branchOnlyMigrations)
	}
	highest := 0
	for _, migration := range all {
		if !slices.Contains(branchOnlyMigrations, migration.Version) {
			highest = max(highest, migration.Version)
		}
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != highest {
		t.Fatalf("the schema version after the way back = %d (%v), want %d, the newest version the release before this one records", version, err, highest)
	}

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if applied != len(branchOnlyMigrations) {
		t.Fatalf("the roll-forward applied %d migrations, want the %d of %v, this one run a second time among them", applied, len(branchOnlyMigrations), branchOnlyMigrations)
	}
	for _, mark := range taskNumbersReadMarks {
		if columnStands(t, store, "pending_task_deliveries", mark) {
			t.Errorf("pending_task_deliveries.%s stands after the roll-forward, want it dropped again by the migration's last statements", mark)
		}
	}
	if got, want := taskNumbersOf(t, store, string(taken.Token)), taskNumbersLine(1, 1, 0, ""); got != want {
		t.Errorf("the claim holding its taken task reads %s after the roll-forward, want %s: the second fill numbers it task 1 again", got, want)
	}
	if got, want := taskNumbersOf(t, store, string(finished.Token)), taskNumbersLine(1, 1, 1, "ses_tester"); got != want {
		t.Errorf("the claim that finished its task reads %s after the roll-forward, want %s", got, want)
	}
	var number int64
	var origin string
	var takenText string
	var turnStarted bool
	if err := store.pool.QueryRow(ctx, "select number, origin, taken_text, turn_started from pending_task_deliveries where claim_token = $1",
		string(taken.Token)).Scan(&number, &origin, &takenText, &turnStarted); err != nil {
		t.Fatalf("read the pending task after the roll-forward: %v", err)
	}
	if number != 1 || origin != "" || takenText != "" || !turnStarted {
		t.Errorf("the pending task after the roll-forward = number %d, origin %q, taken text %q, turn started %v; want task 1 as the second fill writes it, its turn started",
			number, origin, takenText, turnStarted)
	}
}
