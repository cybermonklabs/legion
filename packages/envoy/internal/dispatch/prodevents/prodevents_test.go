package prodevents

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

var (
	writer = model.Actor{Kind: "session", ID: "01a1d375-3e02-77e8-91b9-88e4d96b9c68"}
	person = model.Actor{Kind: "user", ID: "alice"}
	day    = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
)

// project makes a project row the record's foreign key needs.
func project(t *testing.T, ctx context.Context, pool *store.Pool, key string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`, key); err != nil {
		t.Fatalf("make project %s: %v", key, err)
	}
}

func at(hours int) *time.Time {
	return new(day.Add(time.Duration(hours) * time.Hour))
}

func deploy(projectKey, ref string, finished *time.Time, commits ...string) Event {
	return Event{ProjectKey: projectKey, Kind: KindDeploy, Source: SourceGitHub, SourceRef: ref, StartedAt: finished,
		FinishedAt: finished, Repo: "acme/widgets", Commits: commits, PullRequests: []string{"acme/widgets#1"}, Actor: writer}
}

func failure(projectKey, ref, deployID string, signal string) Event {
	return Event{ProjectKey: projectKey, Kind: KindFailure, Source: SourceAPI, SourceRef: ref, DeployID: new(deployID),
		Signal: new(signal), DetectedAt: at(30), Actor: writer}
}

// pendingOf is every pending change row of the record's kind naming id, oldest first, each as its
// two times.
func pendingOf(t *testing.T, ctx context.Context, pool *store.Pool, id string) [][2]*time.Time {
	t.Helper()
	rows, err := pool.Query(ctx, `select t_old, t_new from (
			select id, t_old, t_new from delivery_changes_pending where kind = 'production_events' and ref = $1
			union all
			select seq, t_old, t_new from delivery_changes where kind = 'production_events' and ref = $1
		) c order by id`, id)
	if err != nil {
		t.Fatalf("read the change rows of %s: %v", id, err)
	}
	defer rows.Close()
	var changes [][2]*time.Time
	for rows.Next() {
		var change [2]*time.Time
		if err := rows.Scan(&change[0], &change[1]); err != nil {
			t.Fatalf("scan a change row of %s: %v", id, err)
		}
		changes = append(changes, change)
	}
	return changes
}

func TestARepeatedSourceRefWritesNoSecondRow(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")

	first, created, err := Write(ctx, database.Pool, deploy("ALPHA", "github:7", at(10), "aaaa"))
	if err != nil || !created {
		t.Fatalf("first write: created %v, err %v", created, err)
	}
	again, created, err := Write(ctx, database.Pool, deploy("ALPHA", "github:7", at(11), "bbbb"))
	if err != nil {
		t.Fatalf("repeat write: %v", err)
	}
	if created || again.ID != first.ID || !again.FinishedAt.Equal(*first.FinishedAt) {
		t.Fatalf("repeat answered created %v, row %s finished %v; want the first row %s finished %v", created, again.ID, again.FinishedAt, first.ID, first.FinishedAt)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from production_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the record holds %d rows after a repeat, want 1", count)
	}

	// The store itself refuses a second row of one project and source_ref, whatever writes it.
	_, err = database.Pool.Exec(ctx, `insert into production_events (project_key, kind, source, source_ref, finished_at, actor)
		values ('ALPHA', 'deploy', 'api', 'github:7', now(), '{}')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("a second row of one (project_key, source_ref) answered %v, want unique_violation 23505", err)
	}

	// Another project may hold the same source_ref: the key is per project.
	project(t, ctx, database.Pool, "BETA")
	if _, created, err := Write(ctx, database.Pool, deploy("BETA", "github:7", at(10))); err != nil || !created {
		t.Fatalf("another project's row under the same source_ref: created %v, err %v", created, err)
	}
}

func TestAFailureNeedsTheDeployItBlames(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")

	var refusal *Error
	if _, _, err := Write(ctx, database.Pool, Event{ProjectKey: "ALPHA", Kind: KindFailure, Source: SourceAPI, SourceRef: "x",
		Signal: new(SignalAPI), DetectedAt: at(1), Actor: writer}); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_INPUT" {
		t.Fatalf("a failure with no deploy answered %v, want PRODUCTION_EVENT_INPUT", err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "y", "0193a000-0000-7000-8000-000000000000", SignalAPI)); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_NO_DEPLOY" {
		t.Fatalf("a failure blaming no row answered %v, want PRODUCTION_EVENT_NO_DEPLOY", err)
	}
	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := Write(ctx, database.Pool, failure("ALPHA", "z", stored.ID, SignalAPI))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "w", first.ID, SignalAPI)); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_NO_DEPLOY" {
		t.Fatalf("a failure blaming a failure answered %v, want PRODUCTION_EVENT_NO_DEPLOY", err)
	}

	// The store refuses the same shapes whatever writes them.
	_, err = database.Pool.Exec(ctx, `insert into production_events (project_key, kind, source, source_ref, signal, detected_at, actor)
		values ('ALPHA', 'failure', 'api', 'v', 'api', now(), '{}')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("a failure row with no deploy_id answered %v, want check_violation 23514", err)
	}
}

func TestEachWriteLogsAChangeOfTheDayItCountsOn(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")

	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)))
	if err != nil {
		t.Fatal(err)
	}
	if changes := pendingOf(t, ctx, database.Pool, stored.ID); len(changes) != 1 || changes[0][0] != nil || !changes[0][1].Equal(*at(10)) {
		t.Fatalf("a deploy's insert logged %v, want one change carrying its finished_at", changes)
	}
	failed, _, err := Write(ctx, database.Pool, failure("ALPHA", "flag", stored.ID, SignalPerson))
	if err != nil {
		t.Fatal(err)
	}
	if changes := pendingOf(t, ctx, database.Pool, failed.ID); len(changes) != 1 || !changes[0][1].Equal(*at(10)) {
		t.Fatalf("a failure's insert logged %v, want one change carrying its deploy's finished_at", changes)
	}
	if _, err := SetState(ctx, database.Pool, "ALPHA", failed.ID, StateChange{State: StateRejected, Reason: "not a production failure", Actor: person, At: *at(40)}); err != nil {
		t.Fatal(err)
	}
	if changes := pendingOf(t, ctx, database.Pool, failed.ID); len(changes) != 2 || !changes[1][0].Equal(*at(10)) || !changes[1][1].Equal(*at(10)) {
		t.Fatalf("a reject logged %v, want a second change on the deploy's day", changes)
	}
	if _, err := SetState(ctx, database.Pool, "ALPHA", failed.ID, StateChange{State: StateRestored, Actor: person, At: *at(50)}); err != nil {
		t.Fatal(err)
	}
	changes := pendingOf(t, ctx, database.Pool, failed.ID)
	if len(changes) != 4 || changes[3][0] != nil || !changes[3][1].Equal(*at(50)) {
		t.Fatalf("a restore logged %v, want the row's change and one more carrying the restore time", changes)
	}
}

func TestTheStateRoutesWriteKeepsWhoAndWhy(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")
	project(t, ctx, database.Pool, "BETA")
	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)))
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := Write(ctx, database.Pool, failure("ALPHA", "flag", stored.ID, SignalAPI))
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != StateCounted {
		t.Fatalf("a signal arrived %s, want counted", failed.State)
	}

	var refusal *Error
	if _, err := SetState(ctx, database.Pool, "ALPHA", failed.ID, StateChange{State: StateRejected, Actor: person, At: *at(20)}); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_STATE_INPUT" {
		t.Fatalf("a reject with no reason answered %v, want PRODUCTION_EVENT_STATE_INPUT", err)
	}
	if _, err := SetState(ctx, database.Pool, "ALPHA", stored.ID, StateChange{State: StateRejected, Reason: "r", Actor: person, At: *at(20)}); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_STATE_INPUT" {
		t.Fatalf("a deploy's reject answered %v, want PRODUCTION_EVENT_STATE_INPUT", err)
	}
	if _, err := SetState(ctx, database.Pool, "BETA", failed.ID, StateChange{State: StateRejected, Reason: "r", Actor: person, At: *at(20)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project's reject answered %v, want ErrNotFound", err)
	}

	rejected, err := SetState(ctx, database.Pool, "ALPHA", failed.ID, StateChange{State: StateRejected, Reason: "a staging alert", Actor: person, At: *at(20)})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != StateRejected || rejected.StateReason == nil || *rejected.StateReason != "a staging alert" ||
		rejected.StateBy == nil || rejected.StateBy.ID != "alice" || rejected.StateAt == nil || !rejected.StateAt.Equal(*at(20)) {
		t.Fatalf("the reject kept %+v, want the state, the reason, the writer and the time", rejected)
	}
	restored, err := SetState(ctx, database.Pool, "ALPHA", failed.ID, StateChange{State: StateRestored, RestoredByDeploy: &stored.ID, Actor: person, At: *at(25)})
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredAt == nil || !restored.RestoredAt.Equal(*at(25)) || restored.RestoredByDeploy == nil || *restored.RestoredByDeploy != stored.ID || restored.State != StateRejected {
		t.Fatalf("the restore kept %+v, want its time and its deploy beside the reject", restored)
	}
}

func TestTheListPagesAWindowOfAProjectsRows(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")
	project(t, ctx, database.Pool, "BETA")
	var deploys []Event
	for hour, ref := range []string{"github:1", "github:2", "github:3"} {
		stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", ref, at(hour*24)))
		if err != nil {
			t.Fatal(err)
		}
		deploys = append(deploys, stored)
	}
	if _, _, err := Write(ctx, database.Pool, deploy("BETA", "github:9", at(0))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "f", deploys[0].ID, SignalAPI)); err != nil {
		t.Fatal(err)
	}

	page, total, err := List(ctx, database.Pool, ListQuery{Project: "ALPHA", From: day, To: day.Add(72 * time.Hour), Kind: KindDeploy, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 2 || page[0].ID != deploys[2].ID || page[1].ID != deploys[1].ID {
		t.Fatalf("first page: total %d, %d rows; want 3 deploys, the two newest", total, len(page))
	}
	page, total, err = List(ctx, database.Pool, ListQuery{Project: "ALPHA", From: day, To: day.Add(72 * time.Hour), Kind: KindDeploy, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 1 || page[0].ID != deploys[0].ID {
		t.Fatalf("second page: total %d, %d rows; want the oldest of 3", total, len(page))
	}
	page, total, err = List(ctx, database.Pool, ListQuery{Project: "ALPHA", From: day.Add(24 * time.Hour), To: day.Add(48 * time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(page) != 2 {
		t.Fatalf("the second day holds %d rows (total %d), want its deploy and the failure detected on it", len(page), total)
	}
	if _, total, err = List(ctx, database.Pool, ListQuery{Project: "ALPHA", From: day, To: day.Add(72 * time.Hour), State: StateRejected, Limit: 10}); err != nil || total != 0 {
		t.Fatalf("no rejected row: total %d, err %v", total, err)
	}
	if _, total, err = List(ctx, database.Pool, ListQuery{Project: "ALPHA", From: day, To: day.Add(72 * time.Hour), Limit: 10, Offset: 10}); err != nil || total != 4 {
		t.Fatalf("a page past the end answered total %d, err %v; want 4", total, err)
	}
}

// A drill-down reads the failures that blame one deploy (ListQuery.DeployID): two deploys with
// failures on each, and the filter answers the one deploy's failures alone, no deploy row and no
// failure of another project, even one that blames the same deploy.
func TestTheListKeepsTheFailuresThatBlameOneDeploy(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")
	project(t, ctx, database.Pool, "BETA")
	first, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(1)))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:2", at(2)))
	if err != nil {
		t.Fatal(err)
	}
	var blamingFirst []string
	for _, ref := range []string{"api:1", "api:2"} {
		stored, _, err := Write(ctx, database.Pool, failure("ALPHA", ref, first.ID, SignalAPI))
		if err != nil {
			t.Fatal(err)
		}
		blamingFirst = append(blamingFirst, stored.ID)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "api:3", second.ID, SignalAPI)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("BETA", "api:4", first.ID, SignalAPI)); err != nil {
		t.Fatal(err)
	}

	window := ListQuery{Project: "ALPHA", From: day, To: day.Add(72 * time.Hour), Limit: 10}
	if _, total, err := List(ctx, database.Pool, window); err != nil || total != 5 {
		t.Fatalf("the window holds %d rows (err %v), want ALPHA's two deploys and three failures", total, err)
	}
	filtered := window
	filtered.DeployID = first.ID
	page, total, err := List(ctx, database.Pool, filtered)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range page {
		got = append(got, event.ID)
	}
	slices.Sort(got)
	slices.Sort(blamingFirst)
	if total != 2 || !slices.Equal(got, blamingFirst) {
		t.Fatalf("the failures of deploy %s: total %d, rows %v; want ALPHA's two that blame it, %v", first.ID, total, got, blamingFirst)
	}
	filtered.Offset = 5
	if page, total, err := List(ctx, database.Pool, filtered); err != nil || len(page) != 0 || total != 2 {
		t.Fatalf("a page past the end of deploy %s's failures answered %d rows, total %d, err %v; want none and 2", first.ID, len(page), total, err)
	}
	filtered = window
	filtered.DeployID = "0195f2a8-0000-7000-8000-000000000000"
	if page, total, err := List(ctx, database.Pool, filtered); err != nil || len(page) != 0 || total != 0 {
		t.Fatalf("an id of no row answered %d rows, total %d, err %v; want an empty page", len(page), total, err)
	}
}

func TestACommitBlamesTheFirstDeployThatShippedIt(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")
	later, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:2", at(20), "cccc", "aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10), "aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := DeployForCommit(ctx, database.Pool, "AAAA")
	if err != nil || !ok || found.ID != first.ID {
		t.Fatalf("commit aaaa blamed %v (ok %v, err %v), want the first deploy %s", found.ID, ok, err, first.ID)
	}
	if found, ok, _ := DeployForCommit(ctx, database.Pool, "cccc"); !ok || found.ID != later.ID {
		t.Fatalf("commit cccc blamed %v, want %s", found.ID, later.ID)
	}
	if _, ok, err := DeployForCommit(ctx, database.Pool, "dddd"); err != nil || ok {
		t.Fatalf("a commit no deploy holds answered ok %v, err %v", ok, err)
	}
	if found, ok, err := DeployForRun(ctx, database.Pool, "ACME/widgets", 1); err != nil || !ok || found.ID != first.ID {
		t.Fatalf("run 1 answered %v (ok %v, err %v), want %s", found.ID, ok, err, first.ID)
	}
}

// underTheChangeLogLock runs write, one of the record's writers, while another transaction holds
// the delivery change log's lock (pg_advisory_xact_lock(6900, 0), which the drain takes and no
// writer of the record does), and holds that the write committed all the same: landed answers the
// id of the row the write made or moved once it stands as the write leaves it, polled for 30 s
// while the lock is held; that row's change rows are then waiting in delivery_changes_pending and
// none of them went into the log; and once the lock is released the write returns and every one of
// those change rows is in the log, none left pending (dispatch://INFRA-978, D8 and R13).
func underTheChangeLogLock(t *testing.T, ctx context.Context, pool *store.Pool, what string, write func() error, landed func() (string, bool)) {
	t.Helper()
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin the gate: %v", what, err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock(6900, 0)`); err != nil {
		t.Fatalf("%s: take the change log's lock: %v", what, err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()

	var id string
	for deadline := time.Now().Add(30 * time.Second); ; {
		if got, ok := landed(); ok {
			id = got
			break
		}
		select {
		case err := <-done:
			t.Fatalf("%s: the write ended (%v) with its row not stored while the change log's lock was held", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the write did not commit in 30 s while another transaction held the change log's lock", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
	changeRows := func(table string) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `select count(*) from `+table+` where kind = 'production_events' and ref = $1`, id).Scan(&count); err != nil {
			t.Fatalf("%s: count the change rows of %s in %s: %v", what, id, table, err)
		}
		return count
	}
	pending, logged := changeRows("delivery_changes_pending"), changeRows("delivery_changes")
	if pending == 0 {
		t.Fatalf("%s: with the lock held, row %s has no change row waiting in the outbox, want its changes there", what, id)
	}
	t.Logf("%s: row %s committed with the change log's lock held, its %d change rows waiting in the outbox, %d already in the log", what, id, pending, logged)
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("%s: release the change log's lock: %v", what, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: the write: %v", what, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: the write did not return within 30 s of the lock's release", what)
	}
	if left, now := changeRows("delivery_changes_pending"), changeRows("delivery_changes"); left != 0 || now-logged != pending {
		t.Errorf("%s: once the lock is free, row %s has %d change rows pending and %d more in the log, want none pending and its %d moved", what, id, left, now-logged, pending)
	}
}

// TestTheRoutesAndTheSignalsWritesTakeNoChangeLogLock: every write of the record the routes and the
// automatic signals make commits while another transaction holds the delivery change log's lock,
// each with its change rows waiting in delivery_changes_pending until the drain after the lock is
// released, as TestTheDeployRowWriteTakesNoChangeLogLock holds for the adapter's deploy row. The
// routes' writes: a POST of a deploy and of a failure, a person's flag, and each state of PUT
// .../state (a reject, a re-count and a restore). The signals' row writes: a check_fail, a
// revert (whose deploy restored it) and an incident_issue. This package cannot call the signals'
// passes, which import it; TestTheSignalsWritesTakeNoChangeLogLock, in prodevents/signals, holds
// those and the handler's own write of a check_fail.
func TestTheRoutesAndTheSignalsWritesTakeNoChangeLogLock(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	blamed, _, err := Write(ctx, pool, deploy("ALPHA", "github:1", at(10)))
	if err != nil {
		t.Fatal(err)
	}
	restorer, _, err := Write(ctx, pool, deploy("ALPHA", "github:2", at(40)))
	if err != nil {
		t.Fatal(err)
	}
	rowOf := func(ref string) func() (string, bool) {
		return func() (string, bool) {
			var id string
			err := pool.QueryRow(ctx, `select id::text from production_events where project_key = 'ALPHA' and source_ref = $1`, ref).Scan(&id)
			return id, err == nil
		}
	}
	writes := func(event Event) func() error {
		return func() error {
			_, _, err := Write(ctx, pool, event)
			return err
		}
	}

	posted := deploy("ALPHA", "ci:deploy-1", at(20))
	posted.Source = SourceAPI
	underTheChangeLogLock(t, ctx, pool, "a posted deploy", writes(posted), rowOf("ci:deploy-1"))
	underTheChangeLogLock(t, ctx, pool, "a posted failure", writes(failure("ALPHA", "alerts:1", blamed.ID, SignalAPI)), rowOf("alerts:1"))
	flag := failure("ALPHA", PersonSourceRef(blamed.ID, "alice"), blamed.ID, SignalPerson)
	flag.Source, flag.Actor, flag.Note = SourcePerson, person, "The deploy broke the sign-in page."
	underTheChangeLogLock(t, ctx, pool, "a person's flag", writes(flag), rowOf(flag.SourceRef))

	failed, ok := rowOf("alerts:1")()
	if !ok {
		t.Fatal("the posted failure is not stored")
	}
	stateIs := func(predicate string) func() (string, bool) {
		return func() (string, bool) {
			var held bool
			err := pool.QueryRow(ctx, `select exists (select 1 from production_events where id = $1 and `+predicate+`)`, failed).Scan(&held)
			return failed, err == nil && held
		}
	}
	for _, write := range []struct {
		what   string
		change StateChange
		moved  string
	}{
		{"a reject", StateChange{State: StateRejected, Reason: "a staging alert", Actor: person, At: *at(40)}, `state = 'rejected'`},
		{"a re-count", StateChange{State: StateCounted, Reason: "it was real", Actor: person, At: *at(41)}, `state = 'counted' and state_at is not null`},
		{"a restore", StateChange{State: StateRestored, Actor: person, At: *at(50)}, `restored_at is not null`},
	} {
		underTheChangeLogLock(t, ctx, pool, write.what, func() error {
			_, err := SetState(ctx, pool, "ALPHA", failed, write.change)
			return err
		}, stateIs(write.moved))
	}

	check := failure("ALPHA", "check:doc/c1@"+blamed.ID, blamed.ID, SignalCheckFail)
	check.Source, check.PullRequests = SourceCheckResults, []string{"acme/widgets#8"}
	underTheChangeLogLock(t, ctx, pool, "a check_fail", writes(check), rowOf(check.SourceRef))
	revert := failure("ALPHA", "revert:acme/widgets#9", blamed.ID, SignalRevert)
	revert.Source, revert.PullRequests = SourceReverts, []string{"acme/widgets#5"}
	revert.RestoredAt, revert.RestoredBy, revert.RestoredByDeploy = at(40), &writer, &restorer.ID
	underTheChangeLogLock(t, ctx, pool, "a revert restored by its deploy", writes(revert), rowOf(revert.SourceRef))
	incident := failure("ALPHA", "incident:ALPHA-10", blamed.ID, SignalIncidentIssue)
	incident.Source = SourceIncidentLabel
	underTheChangeLogLock(t, ctx, pool, "an incident_issue", writes(incident), rowOf(incident.SourceRef))
}
