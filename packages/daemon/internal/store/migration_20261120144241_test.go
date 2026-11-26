package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// intakeFailuresVersion is the version of the migration this file tests, so the tests can migrate
// through everything before it whatever lands beside it. Its header also carries the way back of
// the release that gives up on intake messages, which the two way-back tests below run.
const intakeFailuresVersion = 20261120144241

// intakeGiveUpRelease names the release whose way back the header of intakeFailuresVersion
// carries, as a failure to read that block names it. docs/kubernetes.md carries the same block
// under "Rolling back the release that gives up on intake messages", and internal/textcheck holds
// the two to one text; this file reads the header alone, as plan_ratings_way_back_test.go does.
const intakeGiveUpRelease = "the release that gives up on intake messages"

// The two predicates that scope each delete of the way back to the project that rolls back: the
// outbox's rows by their issue's project, as the daemon's lanes read them, and the records by
// their own project column.
const (
	intakeNoticeScope  = "and split_part(issue, '-', 1) = '" + rollingBackProject + "'"
	intakeFailureScope = "project = '" + rollingBackProject + "' and "
)

// intakeGiveUpWayBackFor is the block the header of intakeFailuresVersion carries, as an operator
// who rolls project back runs it: with project's key in place of rollingBackProject.
func intakeGiveUpWayBackFor(t *testing.T, project string) string {
	t.Helper()
	return strings.ReplaceAll(wayBackOf(t, intakeFailuresVersion, intakeGiveUpRelease), rollingBackProject, project)
}

// tableExists says whether store's database holds the table name.
func tableExists(t *testing.T, store *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := store.Pool().QueryRow(t.Context(), `select to_regclass($1) is not null`, name).Scan(&exists); err != nil {
		t.Fatalf("read whether %s exists: %v", name, err)
	}
	return exists
}

// columnShape is the type, nullability and default of table's column as the database holds it,
// "" for a column it does not hold.
func columnShape(t *testing.T, store *Store, table, column string) string {
	t.Helper()
	var shape string
	err := store.Pool().QueryRow(t.Context(), `select data_type || ' nullable=' || is_nullable || ' default=' || coalesce(column_default, '')
		from information_schema.columns where table_schema = current_schema() and table_name = $1 and column_name = $2`, table, column).Scan(&shape)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the shape of %s.%s: %v", table, column, err)
	}
	return shape
}

// intakeFailuresTables is the tables 20261120144241 creates.
var intakeFailuresTables = []string{"intake_failures", "cleared_pull_requests", "pull_request_settled_heads"}

// 20261120144241 creates the tables intakeFailuresTables names, none of which the database before
// it holds (the negative control), and adds no column to pull_requests: what a pull request
// remembers of its settled heads is a table of its own. A record takes the shapes the intake
// writes — an open failure, a give-up, a give-up a replay closed, a discarded one, and a discarded
// one a replay closed later, which keeps its discard — and the checks refuse each shape no
// statement may write, each by its own constraint: a closed record that is not given up, a discard
// with no reason, a close or a give-up that is no known word, either set without its time, and a
// discard's three columns set apart. A second row of one message, and a second clear of one pull
// request, are refused by their keys, and a clear with no time or no issue is refused. A settled
// head is a row of a pull request the record holds, of one of two kinds, once for each kind and
// head, and it goes with its pull request's row.
func TestMigration20261120144241KeepsEachIntakeFailureToItsRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, intakeFailuresVersion-1)
	for _, table := range intakeFailuresTables {
		if tableExists(t, store, table) {
			t.Fatalf("the database before %d holds %s; the control wants it absent", intakeFailuresVersion, table)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the intake's failures: %v", err)
	}
	for _, table := range intakeFailuresTables {
		if !tableExists(t, store, table) {
			t.Errorf("the database after %d holds no %s", intakeFailuresVersion, table)
		}
	}
	if shape := columnShape(t, store, "pull_requests", "settled_heads"); shape != "" {
		t.Errorf("pull_requests after %d holds settled_heads (%s), want what a pull request remembers of its settled heads in a table of its own",
			intakeFailuresVersion, shape)
	}

	seq := 0
	// write inserts one record of the LEGION dispatch consumer with the columns set, a stream
	// sequence of its own, and every other column as a first failed delivery writes it.
	write := func(set string) error {
		seq++
		columns, values := "", ""
		if set != "" {
			name, value, _ := strings.Cut(set, "=")
			columns, values = ", "+name, ", "+value
		}
		_, err := store.Pool().Exec(ctx, fmt.Sprintf(`insert into intake_failures (project, consumer, subject, stream_seq, stored_at, payload,
			first_failed_at, updated_at, deliveries%s)
			values ('LEGION', 'legion-go-LEGION-dispatch', 'notifications.dispatch.issue.LEGION-208.issue.updated', %d, now(), '\x00ff'::bytea,
			now(), now(), 1%s)`, columns, seq, values))
		return err
	}
	// Each set is the columns a shape writes beyond a first failed delivery's, then "=", then
	// their values.
	for _, tc := range []struct {
		shape      string
		set        string
		constraint string
	}{
		{shape: "an open failure"},
		{shape: "a give-up", set: "given_up_at, give_up_reason=now(), 'bound'"},
		{shape: "a give-up a replay closed", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by" +
			"=now(), 'did not end', now(), 'applied', 'operator'"},
		{shape: "a discarded give-up", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by, discarded_at, discarded_by, discard_reason" +
			"=now(), 'undecodable', now(), 'discarded', 'operator', now(), 'operator', 'the issue was deleted'"},
		{shape: "a discarded give-up a replay closed", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by, discarded_at, discarded_by, discard_reason" +
			"=now(), 'consumer moved past', now(), 'already applied', 'controller session ses-1', now(), 'operator', 'the issue was deleted'"},
		{shape: "a give-up whose replay holds a claim", set: "given_up_at, give_up_reason, replay_claimed_at=now(), 'retention', now()"},
		{shape: "a closed record that is not given up", set: "closed_at, closed_how, closed_by=now(), 'applied', 'operator'",
			constraint: "intake_failures_closed_given_up"},
		{shape: "a discarded record that is not given up", set: "discarded_at, discarded_by, discard_reason=now(), 'operator', 'not wanted'",
			constraint: "intake_failures_closed_given_up"},
		{shape: "a discard with an empty reason", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by, discarded_at, discarded_by, discard_reason" +
			"=now(), 'bound', now(), 'discarded', 'operator', now(), 'operator', ''", constraint: "intake_failures_discard"},
		{shape: "a discard with no reason", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by, discarded_at, discarded_by" +
			"=now(), 'bound', now(), 'discarded', 'operator', now(), 'operator'", constraint: "intake_failures_discard"},
		{shape: "a discarded close with no discard", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by" +
			"=now(), 'bound', now(), 'discarded', 'operator'", constraint: "intake_failures_discarded_close"},
		{shape: "an unknown close", set: "given_up_at, give_up_reason, closed_at, closed_how, closed_by=now(), 'bound', now(), 'replayed', 'operator'",
			constraint: "intake_failures_closed_how"},
		{shape: "a close with no time", set: "given_up_at, give_up_reason, closed_how=now(), 'bound', 'applied'", constraint: "intake_failures_closed_how"},
		{shape: "a closed time with no close", set: "given_up_at, give_up_reason, closed_at=now(), 'bound', now()", constraint: "intake_failures_closed_how"},
		{shape: "an unknown give-up reason", set: "given_up_at, give_up_reason=now(), 'poison'", constraint: "intake_failures_give_up_reason"},
		{shape: "a give-up with no reason", set: "given_up_at=now()", constraint: "intake_failures_give_up_reason"},
		{shape: "a give-up reason with no give-up", set: "give_up_reason='bound'", constraint: "intake_failures_give_up_reason"},
	} {
		err := write(tc.set)
		var pgErr *pgconn.PgError
		switch {
		case tc.constraint == "" && err != nil:
			t.Errorf("%s is refused: %v; the intake writes it", tc.shape, err)
		case tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != tc.constraint):
			t.Errorf("writing %s = %v, want the check %s to refuse it (23514)", tc.shape, err, tc.constraint)
		}
	}

	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	insertOne := `insert into intake_failures (project, consumer, subject, stream_seq, stored_at, payload, first_failed_at, updated_at, deliveries)
		values ('LEGION', 'legion-go-LEGION-dispatch', 's', 9000, $1, '\x00'::bytea, now(), now(), 1)`
	if _, err := store.Pool().Exec(ctx, insertOne, at); err != nil {
		t.Fatalf("write a record of stream sequence 9000: %v", err)
	}
	var pgErr *pgconn.PgError
	if _, err := store.Pool().Exec(ctx, insertOne, at); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("a second record of one message = %v, want its key to refuse it (23505)", err)
	}

	clear := `insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at) values ('acme/widgets', 42, 'LEGION-208', $1, $2)`
	if _, err := store.Pool().Exec(ctx, clear, at, at.Add(time.Hour)); err != nil {
		t.Fatalf("record the clear of acme/widgets#42: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, clear, at, at.Add(time.Hour)); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("a second clear of acme/widgets#42 = %v, want its key to refuse it (23505)", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into cleared_pull_requests (repo, number, issue) values ('acme/widgets', 43, 'LEGION-208')`); !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Errorf("a clear with no time = %v, want it refused (23502)", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into cleared_pull_requests (repo, number, cleared_at) values ('acme/widgets', 44, now())`); !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Errorf("a clear with no issue = %v, want it refused (23502): the rename pass reads and writes a clear by its issue", err)
	}

	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record LEGION-208: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved,
		conflicting_head)
		values ('LEGION-208', 'acme/widgets', 1208, 'legion/LEGION-208', 'h2', now(), '[]', '[]', 0, 0,
		 '[]', 1, 'h2-1', '', false, 0, 'closed', 'h2', '[]', '', '', '', 'main', '', '')`); err != nil {
		t.Fatalf("record LEGION-208's pull request: %v", err)
	}
	for _, row := range []struct {
		shape            string
		issue, kind      string
		position         int
		head             string
		code, constraint string
	}{
		{shape: "the pull request's own fence of h1", issue: "LEGION-208", kind: "pull_request", position: 0, head: "h1"},
		{shape: "the pull request's own fence of h2", issue: "LEGION-208", kind: "pull_request", position: 1, head: "h2"},
		{shape: "its head run's fence of h1", issue: "LEGION-208", kind: "head_run", position: 0, head: "h1"},
		{shape: "a second fence of h1 of the pull request's own", issue: "LEGION-208", kind: "pull_request", position: 2, head: "h1",
			code: "23505", constraint: "pull_request_settled_heads_pkey"},
		{shape: "a fence of no known kind", issue: "LEGION-208", kind: "pull_requests", position: 2, head: "h3",
			code: "23514", constraint: "pull_request_settled_heads_kind_check"},
		{shape: "a fence of an issue with no pull request recorded", issue: "LEGION-209", kind: "pull_request", position: 0, head: "h1",
			code: "23503", constraint: "pull_request_settled_heads_pull_request_fkey"},
	} {
		// The migration that keys pull_requests by the issue and the branch (20261122122826) keys
		// these rows by the branch too, so each is written on its pull request's branch.
		_, err := store.Pool().Exec(ctx, `insert into pull_request_settled_heads (issue, branch, kind, position, head, fence)
			values ($1, 'legion/' || $1, $2, $3, $4, '{"checkRuns": []}'::jsonb)`, row.issue, row.kind, row.position, row.head)
		switch {
		case row.code == "" && err != nil:
			t.Errorf("%s is refused: %v; the store writes it", row.shape, err)
		case row.code != "" && (!errors.As(err, &pgErr) || pgErr.Code != row.code || pgErr.ConstraintName != row.constraint):
			t.Errorf("writing %s = %v, want %s to refuse it (%s)", row.shape, err, row.constraint, row.code)
		}
	}
	if _, err := store.Pool().Exec(ctx, `delete from pull_requests where issue = 'LEGION-208'`); err != nil {
		t.Fatalf("delete LEGION-208's pull request: %v", err)
	}
	if left := rowsAsText(t, store, `select row(pull_request_settled_heads.*)::text from pull_request_settled_heads`); len(left) != 0 {
		t.Errorf("deleting the pull request left its settled heads %v, want them deleted with it", left)
	}
}

// registerProjectController records project's controller as the daemon does at
// `legion controller start`, under the project token, so the give-ups below are counted in a
// notice (record.Postgres.ControllerRegistered).
func registerProjectController(t *testing.T, store *Store, project string) {
	t.Helper()
	ctx := t.Context()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.MintController(ctx, token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller of %s: %v", project, err)
	}
	if ok, err := store.RegisterController(ctx, token, generation, "ses-"+project, []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller of %s = %t, %v", project, ok, err)
	}
}

// intakeMessage is a decoded message of project's Dispatch consumer at stream sequence seq.
func intakeMessage(project string, seq uint64) record.IntakeMessage {
	return record.IntakeMessage{
		Project:     project,
		Consumer:    "legion-go-" + project + "-dispatch",
		Subject:     fmt.Sprintf("notifications.dispatch.issue.%s-208.issue.updated", project),
		StreamSeq:   seq,
		StoredAt:    time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond),
		Headers:     record.IntakeHeaders(map[string][]string{"Nats-Msg-Id": {fmt.Sprintf("dispatch-%d", seq)}}),
		Payload:     fmt.Appendf(nil, `{"seq":%d}`, seq),
		EventSource: "dispatch",
		EventID:     fmt.Sprintf("dispatch-%d", seq),
	}
}

// intakeRecords is the records one project's daemon leaves at the moment it stops for the
// rollback, written by this release's own statements: an open failure, a give-up the notice
// counts and that waits for its replay, a give-up a replay closed, a discarded one, and a message
// that did not decode. It answers the ids of the open failure and of the given-up records.
func intakeRecords(t *testing.T, store *Store, project string, at time.Time) (open int64, givenUp []int64) {
	t.Helper()
	ctx := t.Context()
	records := record.NewStore()
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		failure, err := records.NoteFailure(ctx, tx, intakeMessage(project, 41), 1, "the handler failed", at)
		if err != nil {
			return err
		}
		open = failure.ID
		for _, seq := range []uint64{42, 43, 44} {
			failure, err := records.NoteFailure(ctx, tx, intakeMessage(project, seq), 21, "the handler failed", at)
			if err != nil {
				return err
			}
			if _, _, err := records.GiveUpIntakeMessage(ctx, tx, project, failure.ID, record.GiveUpBound, failure.LastError, at); err != nil {
				return err
			}
			givenUp = append(givenUp, failure.ID)
		}
		if _, err := records.ClaimIntakeReplay(ctx, tx, project, record.IntakeSelector{ID: givenUp[1]}, 100, time.Minute, at); err != nil {
			return err
		}
		if _, err := records.CloseIntakeGiveUp(ctx, tx, project, givenUp[1], record.IntakeApplied, "operator", at); err != nil {
			return err
		}
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, project, record.IntakeSelector{ID: givenUp[2]}, "operator", "the issue was deleted", time.Minute, at); err != nil {
			return err
		}
		undecodable, _, err := records.RecordUndecodable(ctx, tx, intakeMessage(project, 45), 1, "Envoy envelope payload is not JSON", at)
		givenUp = append(givenUp, undecodable.ID)
		return err
	}); err != nil {
		t.Fatalf("write %s's intake records: %v", project, err)
	}
	return open, givenUp
}

// releaseBeforeDecodes is the strict decode the release before this one gives a controller
// notice's payload (record.DecodeOutboxPayload, DisallowUnknownFields): this release's
// record.Notice less the two fields this release adds, count and giveUps, and every kind but the
// one it adds. It is built from this release's type rather than copied, so the one difference it
// holds is the one this release made.
func releaseBeforeDecodes(payload []byte) error {
	notice := reflect.TypeOf(record.Notice{})
	var fields []reflect.StructField
	for i := range notice.NumField() {
		if field := notice.Field(i); field.Name != "Count" && field.Name != "GiveUps" {
			fields = append(fields, field)
		}
	}
	value := reflect.New(reflect.StructOf(fields))
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value.Interface()); err != nil {
		return err
	}
	if kind := record.NoticeKind(value.Elem().FieldByName("Kind").String()); kind == record.IntakeGaveUpNotice {
		return fmt.Errorf("unknown controller notice kind %q", kind)
	}
	return nil
}

// controllerNotices is every controller_notice row of store's outbox, oldest first.
func controllerNotices(t *testing.T, store *Store) []record.OutboxRow {
	t.Helper()
	rows, err := store.Pool().Query(t.Context(), `select id, kind, issue, payload from outbox where kind = 'controller_notice' order by id`)
	if err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	notices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var notice record.OutboxRow
		var kind string
		err := row.Scan(&notice.ID, &kind, &notice.Issue, &notice.Payload)
		notice.Kind = record.OutboxKind(kind)
		return notice, err
	})
	if err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	return notices
}

// The way back of the release that gives up on intake messages deletes the rows an earlier image
// cannot take or would read wrong, and nothing else. The rows are one project's as this release
// leaves them when its daemon stops: its unsent intake-gave-up notice, which the earlier image's
// strict decode refuses at every attempt, a tick notice beside it, an open failure, and four
// records the intake gave up — one waiting, one a replay closed, one discarded and one that did not
// decode — and a pull request a clear recorded. The control comes first, on the same rows: the
// release before's decode refuses the intake-gave-up notice and takes the tick. The block runs as
// the header carries it, as one exec (the simple protocol a psql run uses). Then the notice and
// the open failure are gone, and the tick, the four records and the clear stand as they were, in
// their tables, and every table the migration created stands: the earlier image reads none of
// them, and every controller notice left takes its decode.
//
// The roll forward after the rollback is this release's Migrate, which finds the migration's
// version recorded and applies nothing, and reads the records where they stood: the two still
// open are in the state, and a new give-up is recorded and counted in a new notice.
func TestTheIntakeGiveUpWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	registerProjectController(t, store, "LEGION")
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	tick, err := record.NewOutboxRow("LEGION", record.ControllerNotice{Kind: record.TickNotice}, at)
	if err != nil {
		t.Fatalf("queue the tick: %v", err)
	}
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().Enqueue(ctx, tx, tick) }); err != nil {
		t.Fatalf("queue the tick: %v", err)
	}
	open, givenUp := intakeRecords(t, store, "LEGION", at)
	if _, err := store.Pool().Exec(ctx, `insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
		values ('acme/widgets', 42, 'LEGION-208', $1, $1)`, at); err != nil {
		t.Fatalf("record a cleared pull request: %v", err)
	}
	const recordsAsText = `select row(intake_failures.*)::text from intake_failures where given_up_at is not null order by id`
	keptRecords := rowsAsText(t, store, recordsAsText)
	keptClears := rowsAsText(t, store, `select row(cleared_pull_requests.*)::text from cleared_pull_requests`)

	before := controllerNotices(t, store)
	if len(before) != 2 {
		t.Fatalf("the outbox holds %d controller notices before the way back, want the tick and one intake-gave-up notice", len(before))
	}
	for _, notice := range before {
		refused := releaseBeforeDecodes(notice.Payload) != nil
		if gaveUp := strings.Contains(string(notice.Payload), string(record.IntakeGaveUpNotice)); refused != gaveUp {
			t.Fatalf("the release before's decode of %s refuses it: %t; the control wants it to refuse the intake-gave-up notice alone", notice.Payload, refused)
		}
	}

	if _, err := store.Pool().Exec(ctx, intakeGiveUpWayBackFor(t, "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back the header of migration %d carries: %v", intakeFailuresVersion, err)
	}

	// The tick was queued first, so it is the first controller notice before the way back.
	after := controllerNotices(t, store)
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("the outbox holds the controller notices %v after the way back, want the tick alone, row %d", after, before[0].ID)
	}
	for _, notice := range after {
		if err := releaseBeforeDecodes(notice.Payload); err != nil {
			t.Errorf("the release before refuses the notice %s after the way back, and retries it at every attempt: %v", notice.Payload, err)
		}
		if decoded, err := record.DecodeOutboxPayload(notice); err != nil || !reflect.DeepEqual(decoded, record.ControllerNotice{Kind: record.TickNotice}) {
			t.Errorf("this release reads the notice left as %#v, %v; want the tick as it was queued", decoded, err)
		}
	}
	for _, table := range intakeFailuresTables {
		if !tableExists(t, store, table) {
			t.Fatalf("the way back dropped %s; the roll forward would find its migration recorded and no table", table)
		}
	}
	if left := rowsAsText(t, store, `select id::text from intake_failures where given_up_at is null`); len(left) != 0 {
		t.Errorf("the way back left the open failures %v, want the open failure %d deleted", left, open)
	}
	if left := rowsAsText(t, store, recordsAsText); !reflect.DeepEqual(left, keptRecords) {
		t.Errorf("the given-up records read %v after the way back, want them as they were, %v", left, keptRecords)
	}
	if left := rowsAsText(t, store, `select row(cleared_pull_requests.*)::text from cleared_pull_requests`); !reflect.DeepEqual(left, keptClears) {
		t.Errorf("the cleared pull requests read %v after the way back, want them as they were, %v", left, keptClears)
	}

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 0 {
		t.Fatalf("the roll forward's migrate applied %d, %v; want nothing, its version recorded", applied, err)
	}
	records := record.NewStore()
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		state, err := records.OpenIntakeGiveUps(ctx, tx, "LEGION", 50)
		if err != nil {
			return err
		}
		var listed []int64
		for _, failure := range state.Newest {
			listed = append(listed, failure.ID)
		}
		slices.Sort(listed)
		if want := []int64{givenUp[0], givenUp[3]}; !reflect.DeepEqual(listed, want) {
			t.Errorf("the roll forward's state lists the open give-ups %v, want %v: the waiting record and the undecodable one", listed, want)
		}
		later := at.Add(time.Hour)
		failure, err := records.NoteFailure(ctx, tx, intakeMessage("LEGION", 50), 21, "the handler failed", later)
		if err != nil {
			return err
		}
		gave, ok, err := records.GiveUpIntakeMessage(ctx, tx, "LEGION", failure.ID, record.GiveUpBound, failure.LastError, later)
		if err != nil || !ok || gave.NoticeID == 0 {
			return fmt.Errorf("the roll forward's give-up = %#v, %t, %v; want it given up and counted in a notice", gave, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read and write the records after the roll forward: %v", err)
	}
}

// The way back deletes the unsent intake-gave-up notices and the open failures of the project
// that rolls back, and no other project's. Several projects' daemons can share one database, and
// each runs only its own project's outbox rows and records, so one project can roll back while
// another stays on this release, whose daemon still owes its notice and still fails its message.
// Each project here has an open failure and four give-ups counted in a notice. The block runs for
// LEGION as an operator runs it: LEGION's notice and open failure go, OTHER's stand, and every
// given-up record of both stands.
//
// The control runs the same block with the project's predicate cut out of both deletes, over the
// same rows written again: it leaves neither of OTHER's two, so the predicates are what keep them.
func TestTheIntakeGiveUpWayBackDeletesOnlyTheRowsOfTheProjectThatRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	// leftBy is, on a fresh store with both projects' rows written, the project of each
	// intake-gave-up notice and the project and state of each record left after block ran.
	leftBy := func(block string) (notices, failures []string) {
		store := emptyStore(t)
		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		for _, project := range []string{"LEGION", "OTHER"} {
			registerProjectController(t, store, project)
			intakeRecords(t, store, project, at)
		}
		if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run the way back:\n%s\n%v", block, err)
		}
		notices = rowsAsText(t, store, `select issue from outbox
			where kind = 'controller_notice' and payload->>'kind' = 'intake-gave-up' order by id`)
		failures = rowsAsText(t, store, `select project || case when given_up_at is null then ' open' else ' given up' end
			from intake_failures order by id`)
		return notices, failures
	}
	givenUp := []string{"given up", "given up", "given up", "given up"}
	of := func(project string, states ...string) []string {
		var rows []string
		for _, state := range states {
			rows = append(rows, project+" "+state)
		}
		return rows
	}

	notices, failures := leftBy(intakeGiveUpWayBackFor(t, "LEGION"))
	if want := []string{"OTHER"}; !reflect.DeepEqual(notices, want) {
		t.Errorf("the way back for LEGION leaves the intake-gave-up notices of %v, want OTHER's %v: another project's daemon, still on this release, never sends a row the block deleted",
			notices, want)
	}
	if want := slices.Concat(of("LEGION", givenUp...), of("OTHER", append([]string{"open"}, givenUp...)...)); !reflect.DeepEqual(failures, want) {
		t.Errorf("the way back for LEGION leaves the records %v, want %v: LEGION's open failure deleted, OTHER's kept, and every give-up kept", failures, want)
	}

	block := wayBackOf(t, intakeFailuresVersion, intakeGiveUpRelease)
	for _, scope := range []string{intakeNoticeScope, intakeFailureScope} {
		if n := strings.Count(block, scope); n != 1 {
			t.Errorf("the way back carries the predicate %q %d times, want once:\n%s", scope, n, block)
		}
	}
	unscoped := strings.ReplaceAll(strings.ReplaceAll(block, intakeNoticeScope, ""), intakeFailureScope, "")
	notices, failures = leftBy(unscoped)
	if len(notices) != 0 {
		t.Errorf("the control, the way back with no project predicate, leaves the notices of %v; want none, or this test cannot see a block that deletes another project's rows", notices)
	}
	if want := slices.Concat(of("LEGION", givenUp...), of("OTHER", givenUp...)); !reflect.DeepEqual(failures, want) {
		t.Errorf("the control leaves the records %v, want %v: with no predicate it deletes OTHER's open failure too", failures, want)
	}
}
