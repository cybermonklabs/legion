package store

import (
	"context"
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
// which the header and the guide both call the sixteen.
const (
	taskNumbersFile              = "internal/store/migrations/20261113175649_task_numbers.up.sql"
	taskNumbersGuide             = "docs/kubernetes.md"
	taskNumbersHeredoc           = "psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'"
	taskNumbersTerminator        = "SQL"
	taskNumbersWayBackStatements = 16
)

// taskNumbersReadMarks are the push path's three marks on a pending task, which the migration
// drops and its way back puts back.
var taskNumbersReadMarks = []string{"delivered_at", "marked_by", "prompted"}

// taskNumbersColumns are the twelve columns the migration adds, which its way back drops.
var taskNumbersColumns = []struct{ table, column string }{
	{"claims", "task_written"}, {"claims", "task_taken"}, {"claims", "task_finished"},
	{"claims", "task_finished_session"}, {"claims", "task_seen_epoch"}, {"claims", "task_seen_revision"},
	{"claims", "task_take_state"}, {"claims", "task_written_epoch"}, {"claims", "task_written_revision"},
	{"pending_task_deliveries", "number"}, {"pending_task_deliveries", "origin"},
	{"pending_task_deliveries", "taken_text"},
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
	return fmt.Sprintf("written %d taken %d finished %d in %q, seen %q/%d take %q, wrote %q/%d",
		written, taken, finished, session, seenEpoch, seenRevision, takeState, writtenEpoch, writtenRevision)
}

// taskNumbersLine is taskNumbersOf's line for a claim whose epochs and revisions are empty, which
// is every claim the fill writes.
func taskNumbersLine(written, taken, finished int, session string) string {
	return fmt.Sprintf("written %d taken %d finished %d in %q, seen %q/%d take %q, wrote %q/%d",
		written, taken, finished, session, "", 0, "", "", 0)
}

// 20261113175649 numbers the task each claim an earlier release left holds as its first. The fill
// is read on the four shapes it decides between: a claim holding a pending task nobody has taken
// yet (task 1 written), one holding a task whose turn started (task 1 written and taken), one
// holding none that served a task — a run, or only a phase entry — (task 1 written, taken and
// finished in the session it holds), and one that was given nothing (no number at all). The
// pending claim that has also served a run is the case that tells the two rules apart: the row it
// holds is its task, so its finished number stays 0.
//
// The negative control is the write of a task number before the migration, which the database
// refuses for a column it does not have; and after it, the three read marks of the push path are
// gone, so a reader of this record finds one model and not two.
func TestMigrationTaskNumbersFillsEachClaimsFirstTask(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskNumbersVersion-1)

	claimRow := `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, served_entry)
		values ($1, 'legion', 'LEGION-208', $2, $3, 1, $4, '', $5, 0, 0, 0, 0, $6, $7)`
	for _, seed := range []struct {
		token, issue, role, session, state string
		serving, entry                     int64
	}{
		{"legion-LEGION-301-implementer", "LEGION-301", "implementer", "ses_queued", "ready", 1, 1},
		{"legion-LEGION-302-implementer", "LEGION-302", "implementer", "ses_taken", "working", 0, 0},
		{"legion-LEGION-303-implementer", "LEGION-303", "implementer", "ses_run", "idle", 2, 0},
		{"legion-LEGION-304-tester", "LEGION-304", "tester", "ses_entry", "idle", 0, 3},
		{"legion-LEGION-305-reviewer", "LEGION-305", "reviewer", "ses_nothing", "ready", 0, 0},
	} {
		if _, err := store.pool.Exec(ctx, claimRow, seed.token, seed.issue, seed.role, seed.session, seed.state,
			seed.serving, seed.entry); err != nil {
			t.Fatalf("record %s before the migration: %v", seed.token, err)
		}
	}
	// The two pending rows as the push path leaves them: the first acknowledged and waiting for its
	// turn, the second confirmed by the turn its send started.
	for _, seed := range []struct {
		token, id string
		confirmed bool
	}{
		{"legion-LEGION-301-implementer", "outbox:31", false},
		{"legion-LEGION-302-implementer", "outbox:32", true},
	} {
		if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase,
			generation, queued_at, delivered_at, marked_by, prompted, confirmed_at)
			values ($1, $2, 'Implement the change.', 'implementing', 1, now(), now(), $2, true,
			case when $3 then now() end)`, seed.token, seed.id, seed.confirmed); err != nil {
			t.Fatalf("record the pending task of %s before the migration: %v", seed.token, err)
		}
	}

	_, err := store.pool.Exec(ctx, "update claims set task_written = 1")
	if err == nil || !strings.Contains(err.Error(), "task_written") {
		t.Fatalf("writing a task number before the migration = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the task number write is refused: %v", err)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate through the task numbers: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the migration applied %d migrations, want this one alone: every other is the fixture's own", applied)
	}

	for _, want := range []struct {
		token, line, why string
	}{
		{"legion-LEGION-301-implementer", taskNumbersLine(1, 0, 0, ""),
			"a pending task nobody has taken is task 1 written, whatever run the claim served before it"},
		{"legion-LEGION-302-implementer", taskNumbersLine(1, 1, 0, ""),
			"a pending task whose turn started is task 1 written and taken, and not finished while that turn runs"},
		{"legion-LEGION-303-implementer", taskNumbersLine(1, 1, 1, "ses_run"),
			"a claim that served a run and holds no task finished task 1 in the session it holds"},
		{"legion-LEGION-304-tester", taskNumbersLine(1, 1, 1, "ses_entry"),
			"a claim that served a phase entry and holds no task finished task 1 in the session it holds"},
		{"legion-LEGION-305-reviewer", taskNumbersLine(0, 0, 0, ""),
			"a claim that was given nothing has no number"},
	} {
		if got := taskNumbersOf(t, store, want.token); got != want.line {
			t.Errorf("%s after the migration reads %s, want %s: %s", want.token, got, want.line, want.why)
		}
	}
	for _, token := range []string{"legion-LEGION-301-implementer", "legion-LEGION-302-implementer"} {
		var number int64
		var origin string
		var taken *string
		if err := store.pool.QueryRow(ctx, `select number, origin, taken_text from pending_task_deliveries
			where claim_token = $1`, token).Scan(&number, &origin, &taken); err != nil {
			t.Fatalf("read the pending task of %s after the migration: %v", token, err)
		}
		if number != 1 || origin != "" || taken != nil {
			t.Errorf("the pending task of %s = number %d, origin %q, taken text %v; want task 1 of no recorded origin, which no take was ever answered for",
				token, number, origin, taken)
		}
	}

	for _, mark := range taskNumbersReadMarks {
		if columnStands(t, store, "pending_task_deliveries", mark) {
			t.Errorf("pending_task_deliveries.%s stands after the migration, want it dropped: a task the worker takes is acknowledged by nothing", mark)
		}
	}
	// The take state holds the four the record names and no other.
	if _, err := store.pool.Exec(ctx, "update claims set task_take_state = 'made' where token = 'legion-LEGION-305-reviewer'"); err != nil {
		t.Fatalf("recording a take made at the seen revision: %v", err)
	}
	_, err = store.pool.Exec(ctx, "update claims set task_take_state = 'taken' where token = 'legion-LEGION-305-reviewer'")
	if err == nil || !strings.Contains(err.Error(), "task_take_state") {
		t.Fatalf("recording the take state 'taken' = %v, want the check to refuse a state the record does not name", err)
	}
}

// Every task column round-trips through the store across a restart: a new pool and a new store
// over the same database, with nothing of the first one carried in memory (reopenStore). The claim
// carries every one of its nine task fields and its pending task its number, its origin and the
// text its accepted take was answered with, which is what lets a take sent again after a lost
// answer, or after a daemon restart, be answered the same text without being taken a second time.
// The text is stored as null while no take has been accepted, and two numbers no bigint holds are
// refused before anything is written.
func TestTaskNumberSurvivesAStoreRestartWithEveryColumn(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.State = supervise.StateWorking
	want.TaskWritten, want.TaskTaken, want.TaskFinished, want.TaskFinishedSession = 7, 7, 6, "ses_implementer"
	want.TaskSeenEpoch, want.TaskSeenRevision, want.TaskTakeState = "2026-11-13T17:56:49.123456789Z", 41, supervise.TakeAccepted
	want.TaskWrittenEpoch, want.TaskWrittenRevision = "2026-11-13T17:56:49.123456789Z", 42
	want.Pending = &supervise.Delivery{
		ID: "outbox:71", Task: "Implement the change. Issue: LEGION-209. Phase: implementing.",
		Phase: phase.Implementing, Generation: 4, Entry: 3, QueuedAt: at(1), ConfirmedAt: at(2),
		Number: 7, Origin: "phase-start",
		TakenText: "Your last turn was interrupted.\n\nImplement the change. Issue: LEGION-209. Phase: implementing.",
	}
	if err := store.PutClaimAndDelivery(ctx, want, *want.Pending); err != nil {
		t.Fatalf("put the claim and its taken task: %v", err)
	}
	sameClaim(t, onlyClaim(t, reopenStore(t, store)), want)

	t.Run("a task no take has been accepted for holds no text", func(t *testing.T) {
		// The claim's next task, queued under the next number and not yet taken: nothing has
		// confirmed it, and no take of it has been answered.
		queued := *want.Pending
		queued.ID, queued.Number, queued.Origin, queued.TakenText = "outbox:72", 8, "wake", ""
		queued.ConfirmedAt = time.Time{}
		if err := store.PutDelivery(ctx, want.Token, queued); err != nil {
			t.Fatalf("put the queued task: %v", err)
		}
		var null bool
		if err := store.pool.QueryRow(ctx, "select taken_text is null from pending_task_deliveries where claim_token = $1",
			string(want.Token)).Scan(&null); err != nil {
			t.Fatalf("read the queued task's taken text: %v", err)
		}
		if !null {
			t.Error("a task no take has been accepted for stores a taken text, want null")
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
// still call them the sixteen stops the test; the count splits on the semicolon, which no
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
		t.Fatalf("the way back's block of %s holds %d statements between its begin and its commit, want the %d that file and %s call the sixteen:\n%s",
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
// has queued and not yet sent — and none of the twelve columns stands, nor this migration's
// version row, while every other row stays recorded. The roll-forward then runs the migration
// again, and its second fill numbers what it finds as the first fill numbered what an earlier
// release left: the task the claim held under number 7 is task 1 again, and the claim that had
// finished task 4 has finished task 1, in the session it holds.
func TestMigrationTaskNumbersWayBackLetsTheReleaseBeforeReadAndReArmsTheFill(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	taken := tmuxClaim("legion-LEGION-209-implementer")
	taken.State = supervise.StateWorking
	taken.TaskWritten, taken.TaskTaken, taken.TaskTakeState = 7, 7, supervise.TakeAccepted
	taken.TaskSeenEpoch, taken.TaskSeenRevision = "2026-11-13T17:56:49Z", 12
	task := supervise.Delivery{ID: "outbox:71", Task: "Implement the change.", Phase: phase.Implementing,
		Generation: 4, QueuedAt: at(1), ConfirmedAt: at(2), Number: 7, Origin: "phase-start", TakenText: "Implement the change."}
	if err := store.PutClaimAndDelivery(ctx, taken, task); err != nil {
		t.Fatalf("put the claim holding its taken task: %v", err)
	}
	finished := tmuxClaim("legion-LEGION-210-tester")
	finished.Role, finished.Issue, finished.Session = claim.RoleTester, "LEGION-210", "ses_tester"
	finished.TaskWritten, finished.TaskTaken, finished.TaskFinished, finished.TaskFinishedSession = 4, 4, 4, "ses_tester"
	if err := store.PutClaim(ctx, finished); err != nil {
		t.Fatalf("put the claim that finished its task: %v", err)
	}

	recovery := taskNumbersWayBack(t, taskNumbersWayBackOfTheHeader(t))
	for run := range 2 {
		if _, err := store.pool.Exec(ctx, recovery, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %d of the way back's block of %s as the one transaction an operator runs: %v", run+1, taskNumbersFile, err)
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
	if armed != 0 || recorded != len(all)-1 {
		t.Fatalf("schema_version holds %d rows of %d and %d rows in all after the way back, want none of it and the %d every other migration recorded: store.apply decides each migration on its own row",
			armed, taskNumbersVersion, recorded, len(all)-1)
	}
	highest := 0
	for _, migration := range all {
		if migration.Version != taskNumbersVersion {
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
	if applied != 1 {
		t.Fatalf("the roll-forward applied %d migrations, want this one run a second time", applied)
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
	var takenText *string
	if err := store.pool.QueryRow(ctx, "select number, origin, taken_text from pending_task_deliveries where claim_token = $1",
		string(taken.Token)).Scan(&number, &origin, &takenText); err != nil {
		t.Fatalf("read the pending task after the roll-forward: %v", err)
	}
	if number != 1 || origin != "" || takenText != nil {
		t.Errorf("the pending task after the roll-forward = number %d, origin %q, taken text %v; want task 1 as the second fill writes it", number, origin, takenText)
	}
}
