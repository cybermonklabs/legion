package prodevents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
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

	first, created, err := Write(ctx, database.Pool, deploy("ALPHA", "github:7", at(10), "aaaa"), nil)
	if err != nil || !created {
		t.Fatalf("first write: created %v, err %v", created, err)
	}
	again, created, err := Write(ctx, database.Pool, deploy("ALPHA", "github:7", at(11), "bbbb"), nil)
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
	if _, created, err := Write(ctx, database.Pool, deploy("BETA", "github:7", at(10)), nil); err != nil || !created {
		t.Fatalf("another project's row under the same source_ref: created %v, err %v", created, err)
	}
}

func TestAFailureNeedsTheDeployItBlames(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")

	var refusal *Error
	if _, _, err := Write(ctx, database.Pool, Event{ProjectKey: "ALPHA", Kind: KindFailure, Source: SourceAPI, SourceRef: "x",
		Signal: new(SignalAPI), DetectedAt: at(1), Actor: writer}, nil); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_INPUT" {
		t.Fatalf("a failure with no deploy answered %v, want PRODUCTION_EVENT_INPUT", err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "y", "0193a000-0000-7000-8000-000000000000", SignalAPI), nil); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_NO_DEPLOY" {
		t.Fatalf("a failure blaming no row answered %v, want PRODUCTION_EVENT_NO_DEPLOY", err)
	}
	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := Write(ctx, database.Pool, failure("ALPHA", "z", stored.ID, SignalAPI), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "w", first.ID, SignalAPI), nil); !errors.As(err, &refusal) || refusal.Code != "PRODUCTION_EVENT_NO_DEPLOY" {
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

// TestAFailureAndTheDropOfItsDeployWaitForEachOther: a failure blaming a deploy row that a delete
// takes, written while that delete is still open, waits for it, and once the row is gone is
// refused PRODUCTION_EVENT_NO_DEPLOY, as a failure blaming no row is, rather than failing its
// foreign key. And the GitHub adapter's drop of a row that a failure still being written blames
// (DropGitHubDeploy) waits for that failure's commit and then keeps the row, dropped, the failure
// rejected with the drop's reason. Red where the failure reads its deploy with no lock: its insert
// waits on the delete's lock and then fails the foreign key (23503), which the route answers 500.
func TestAFailureAndTheDropOfItsDeployWaitForEachOther(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	// waitsOnALock waits until a statement on production_events waits on a lock, failing where
	// done answers first, the write having ended without waiting.
	waitsOnALock := func(what string, done <-chan error) {
		t.Helper()
		testwait.Until(t, what, func() (bool, bool) {
			select {
			case err := <-done:
				t.Fatalf("%s: it ended (%v) without waiting on a lock", what, err)
			default:
			}
			var waiting bool
			if err := pool.QueryRow(ctx, `select exists (select from pg_stat_activity
				where datname = current_database() and wait_event_type = 'Lock' and query like '%production_events%')`).Scan(&waiting); err != nil {
				t.Fatalf("%s: read the lock waits: %v", what, err)
			}
			return waiting, waiting
		})
	}

	dropped, _, err := Write(ctx, pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer drop.Rollback(ctx)
	// A delete of a row no failure blames, held open before its commit.
	if _, err := drop.Exec(ctx, `select from production_events where id = $1 for update`, dropped.ID); err != nil {
		t.Fatalf("lock the deploy row as the delete does: %v", err)
	}
	if _, err := drop.Exec(ctx, `delete from production_events where id = $1`, dropped.ID); err != nil {
		t.Fatalf("delete the deploy row: %v", err)
	}
	written := make(chan error, 1)
	go func() {
		_, _, err := Write(ctx, pool, failure("ALPHA", "alerts:1", dropped.ID, SignalAPI), nil)
		written <- err
	}()
	waitsOnALock("the failure blaming the deploy being deleted", written)
	if err := drop.Commit(ctx); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}
	var refusal *Error
	if err := testwait.Receive(t, "the failure's write", written); !errors.As(err, &refusal) || refusal.Code != CodeNoDeploy {
		t.Fatalf("a failure blaming a deploy deleted while it was written answered %v, want %s", err, CodeNoDeploy)
	}

	kept, _, err := Write(ctx, pool, deploy("ALPHA", "github:2", at(20)), nil)
	if err != nil {
		t.Fatal(err)
	}
	failing, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer failing.Rollback(ctx)
	blamed, _, err := WriteIn(ctx, failing, failure("ALPHA", "alerts:2", kept.ID, SignalAPI), nil)
	if err != nil {
		t.Fatalf("write the failure blaming the deploy: %v", err)
	}
	const reason = "the rule no longer calls this run a deploy"
	droppedDone := make(chan error, 1)
	go func() {
		droppedDone <- DropGitHubDeploy(ctx, pool, "ALPHA", kept.ID, reason, writer, *at(30))
	}()
	waitsOnALock("the drop of a deploy a failure being written blames", droppedDone)
	if err := failing.Commit(ctx); err != nil {
		t.Fatalf("commit the failure: %v", err)
	}
	if err := testwait.Receive(t, "the drop", droppedDone); err != nil {
		t.Fatalf("the drop of a deploy a failure blames: %v", err)
	}
	if row, err := Get(ctx, pool, "ALPHA", kept.ID); err != nil || !row.Dropped() || row.Note != reason {
		t.Fatalf("the dropped deploy is %+v (%v), want it kept, dropped with the drop's reason", row, err)
	}
	if stored, err := Get(ctx, pool, "ALPHA", blamed.ID); err != nil || stored.State != StateRejected || stored.StateReason == nil || *stored.StateReason != reason {
		t.Fatalf("the failure the drop found is %+v (%v), want it rejected with the drop's reason", stored, err)
	}
}

func TestEachWriteLogsAChangeOfTheDayItCountsOn(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	project(t, ctx, database.Pool, "ALPHA")

	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if changes := pendingOf(t, ctx, database.Pool, stored.ID); len(changes) != 1 || changes[0][0] != nil || !changes[0][1].Equal(*at(10)) {
		t.Fatalf("a deploy's insert logged %v, want one change carrying its finished_at", changes)
	}
	failed, _, err := Write(ctx, database.Pool, failure("ALPHA", "flag", stored.ID, SignalPerson), nil)
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
	stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := Write(ctx, database.Pool, failure("ALPHA", "flag", stored.ID, SignalAPI), nil)
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
		stored, _, err := Write(ctx, database.Pool, deploy("ALPHA", ref, at(hour*24)), nil)
		if err != nil {
			t.Fatal(err)
		}
		deploys = append(deploys, stored)
	}
	if _, _, err := Write(ctx, database.Pool, deploy("BETA", "github:9", at(0)), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "f", deploys[0].ID, SignalAPI), nil); err != nil {
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
	first, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(1)), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:2", at(2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var blamingFirst []string
	for _, ref := range []string{"api:1", "api:2"} {
		stored, _, err := Write(ctx, database.Pool, failure("ALPHA", ref, first.ID, SignalAPI), nil)
		if err != nil {
			t.Fatal(err)
		}
		blamingFirst = append(blamingFirst, stored.ID)
	}
	if _, _, err := Write(ctx, database.Pool, failure("ALPHA", "api:3", second.ID, SignalAPI), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Write(ctx, database.Pool, failure("BETA", "api:4", first.ID, SignalAPI), nil); err != nil {
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
	later, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:2", at(20), "cccc", "aaaa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := Write(ctx, database.Pool, deploy("ALPHA", "github:1", at(10), "aaaa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := DeployForCommit(ctx, database.Pool, "ALPHA", "AAAA")
	if err != nil || !ok || found.ID != first.ID {
		t.Fatalf("commit aaaa blamed %v (ok %v, err %v), want the first deploy %s", found.ID, ok, err, first.ID)
	}
	if found, ok, _ := DeployForCommit(ctx, database.Pool, "ALPHA", "cccc"); !ok || found.ID != later.ID {
		t.Fatalf("commit cccc blamed %v, want %s", found.ID, later.ID)
	}
	if _, ok, err := DeployForCommit(ctx, database.Pool, "ALPHA", "dddd"); err != nil || ok {
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
	blamed, _, err := Write(ctx, pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	restorer, _, err := Write(ctx, pool, deploy("ALPHA", "github:2", at(40)), nil)
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
			_, _, err := Write(ctx, pool, event, nil)
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

// TestASignalBlamesNoDeployAnotherProjectPosted: a commit and a pull request that a deploy posted
// to BETA names, and that finished first, are blamed for ALPHA's signals on ALPHA's GitHub deploy
// of them, never on BETA's post, which counts for BETA alone; BETA's signals blame BETA's post. A
// deploy ALPHA posted itself, earlier still, is the one ALPHA's signals blame.
func TestASignalBlamesNoDeployAnotherProjectPosted(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	project(t, ctx, pool, "BETA")
	posted := func(projectKey, ref string, finished *time.Time) Event {
		t.Helper()
		event := deploy(projectKey, ref, finished, "aaaa")
		event.Source = SourceAPI
		stored, _, err := Write(ctx, pool, event, nil)
		if err != nil {
			t.Fatalf("post the deploy %s of %s: %v", ref, projectKey, err)
		}
		return stored
	}
	github, _, err := Write(ctx, pool, deploy("ALPHA", "github:1", at(20), "aaaa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	beta := posted("BETA", "ci:beta-1", at(5))
	blames := func(when string, want map[string]string) {
		t.Helper()
		for projectKey, id := range want {
			if found, held, err := DeployForCommit(ctx, pool, projectKey, "AAAA"); err != nil || !held || found.ID != id {
				t.Errorf("%s: %s's signals blame %q (held %v, %v) for commit aaaa, want %s", when, projectKey, found.ID, held, err, id)
			}
			if found, held, err := DeployForPullRequest(ctx, pool, projectKey, "ACME/widgets#1"); err != nil || !held || found.ID != id {
				t.Errorf("%s: %s's signals blame %q (held %v, %v) for acme/widgets#1, want %s", when, projectKey, found.ID, held, err, id)
			}
		}
	}
	blames("beside BETA's earlier post", map[string]string{"ALPHA": github.ID, "BETA": beta.ID})
	alpha := posted("ALPHA", "ci:alpha-1", at(10))
	blames("beside ALPHA's own post", map[string]string{"ALPHA": alpha.ID, "BETA": beta.ID})
}

// TestARepeatAnswersItsRowOnceItsDeployNoLongerCounts: a write under a source_ref its project
// holds answers that row, created false, before it asks anything of the deploy the failure
// blames, so a retry is answered so after the deploy stopped counting for the project: its check
// refuses the deploy now (R10).
func TestARepeatAnswersItsRowOnceItsDeployNoLongerCounts(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	blamed, _, err := Write(ctx, pool, deploy("ALPHA", "github:1", at(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	post := failure("ALPHA", "alerts:1", blamed.ID, SignalAPI)
	first, created, err := Write(ctx, pool, post, nil)
	if err != nil || !created {
		t.Fatalf("the first write: created %v, %v", created, err)
	}
	refusing := func(context.Context, pgx.Tx, Event) error { return NoDeploy(blamed.ID) }
	var refusal *Error
	if _, _, err := Write(ctx, pool, failure("ALPHA", "alerts:2", blamed.ID, SignalAPI), refusing); !errors.As(err, &refusal) || refusal.Code != CodeNoDeploy {
		t.Fatalf("a new failure the check refuses answered %v, want %s", err, CodeNoDeploy)
	}
	again, created, err := Write(ctx, pool, post, refusing)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("the repeat answered row %q, created %v, %v; want the first row %s", again.ID, created, err, first.ID)
	}
}

// TestARestoreKeepsTheFirstRestore: a failure restored already keeps its first restore: a
// person's restore of a revert's failure, which its own deploy restored, with or without a
// restoring deploy of its own, and a person's second restore of a failure a person restored, each
// answer the row as it stands, its restore time, writer and restoring deploy unmoved, and log no
// change (R7).
func TestARestoreKeepsTheFirstRestore(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	write := func(event Event) Event {
		t.Helper()
		stored, _, err := Write(ctx, pool, event, nil)
		if err != nil {
			t.Fatalf("write %s: %v", event.SourceRef, err)
		}
		return stored
	}
	blamed, restoring, later := write(deploy("ALPHA", "github:1", at(10))), write(deploy("ALPHA", "github:2", at(20))), write(deploy("ALPHA", "github:3", at(30)))
	revert := failure("ALPHA", "revert:acme/widgets#9", blamed.ID, SignalRevert)
	revert.Source = SourceReverts
	revert.RestoredAt, revert.RestoredBy, revert.RestoredByDeploy = restoring.FinishedAt, &writer, &restoring.ID
	reverted := write(revert)
	flag := failure("ALPHA", PersonSourceRef(blamed.ID, "alice"), blamed.ID, SignalPerson)
	flag.Source, flag.Actor = SourcePerson, person
	flagged, err := SetState(ctx, pool, "ALPHA", write(flag).ID, StateChange{State: StateRestored, Actor: person, At: *at(40)})
	if err != nil || flagged.RestoredAt == nil || !flagged.RestoredAt.Equal(*at(40)) {
		t.Fatalf("the first restore of the flag answered %+v, %v; want it restored at %v", flagged, err, at(40))
	}
	restoreOf := func(row Event) string {
		restorer := "none"
		if row.RestoredByDeploy != nil {
			restorer = *row.RestoredByDeploy
		}
		by := "nobody"
		if row.RestoredBy != nil {
			by = row.RestoredBy.Kind + ":" + row.RestoredBy.ID
		}
		return fmt.Sprintf("restored at %v by %s, restoring deploy %s", row.RestoredAt, by, restorer)
	}
	for _, again := range []struct {
		what   string
		row    Event
		change StateChange
	}{
		{"a person's restore of a revert's failure", reverted, StateChange{State: StateRestored, Actor: person, At: *at(50)}},
		{"a person's restore of a revert's failure naming another deploy", reverted,
			StateChange{State: StateRestored, RestoredByDeploy: &later.ID, Actor: person, At: *at(50)}},
		{"a person's repeated restore", flagged, StateChange{State: StateRestored, Actor: person, At: *at(60)}},
	} {
		changes := len(pendingOf(t, ctx, pool, again.row.ID))
		answered, err := SetState(ctx, pool, "ALPHA", again.row.ID, again.change)
		if err != nil {
			t.Fatalf("%s: %v", again.what, err)
		}
		stored, err := Get(ctx, pool, "ALPHA", again.row.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := restoreOf(again.row)
		if got := restoreOf(answered); got != want {
			t.Errorf("%s answered a row %s, want the first restore, %s", again.what, got, want)
		}
		if got := restoreOf(stored); got != want {
			t.Errorf("%s left the row %s, want the first restore, %s", again.what, got, want)
		}
		if now := len(pendingOf(t, ctx, pool, again.row.ID)); now != changes {
			t.Errorf("%s logged %d changes, want none", again.what, now-changes)
		}
	}
}

// TestAnotherProjectsAutomaticFailureIsListedAndJudgedWhereItsDeployCountsForIt: an automatic
// signal's failure of ALPHA on a GitHub deploy counts for every project that deploy counts for,
// so it is among the failures of other projects BETA's list asks its route of (OthersFailures),
// and BETA's list and state write take it where the route's check of its deploy does (Reached,
// StateChange.Check). A posted failure of ALPHA on that deploy, an automatic failure on a deploy
// ALPHA posted, and the deploy's own row are none of them: BETA's write of the first and the last
// is ErrNotFound whatever its check says, and so is its write of the check_fail where its check
// refuses the deploy. A reject through BETA's route is the row's own state, which ALPHA reads too.
func TestAnotherProjectsAutomaticFailureIsListedAndJudgedWhereItsDeployCountsForIt(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	pool := database.Pool
	project(t, ctx, pool, "ALPHA")
	project(t, ctx, pool, "BETA")
	write := func(event Event) Event {
		t.Helper()
		stored, _, err := Write(ctx, pool, event, nil)
		if err != nil {
			t.Fatalf("write %s: %v", event.SourceRef, err)
		}
		return stored
	}
	shared := write(deploy("ALPHA", "github:1", at(10)))
	postedDeploy := deploy("ALPHA", "ci:deploy-1", at(11))
	postedDeploy.Source = SourceAPI
	posted := write(postedDeploy)
	checkFail := func(ref, deployID string) Event {
		event := failure("ALPHA", ref, deployID, SignalCheckFail)
		event.Source = SourceCheckResults
		return write(event)
	}
	check := checkFail("check:doc/c1@"+shared.ID, shared.ID)
	checkFail("check:doc/c2@"+posted.ID, posted.ID)
	alert := write(failure("ALPHA", "alerts:1", shared.ID, SignalAPI))

	query := ListQuery{Project: "BETA", From: day, To: day.Add(72 * time.Hour), Limit: 10}
	others, err := OthersFailures(ctx, pool, query)
	if err != nil || len(others) != 1 || others[0].ID != check.ID || others[0].Deploy.ID != shared.ID || others[0].Deploy.Source != SourceGitHub {
		t.Fatalf("the failures of other projects that may count for BETA = %+v (%v), want ALPHA's check_fail %s on its GitHub deploy %s", others, err, check.ID, shared.ID)
	}
	deploys := query
	deploys.Kind = KindDeploy
	if others, err := OthersFailures(ctx, pool, deploys); err != nil || len(others) != 0 {
		t.Fatalf("a read of deploys alone asked of %+v (%v), want no failure", others, err)
	}
	query.Reached = []string{check.ID}
	if page, total, err := List(ctx, pool, query); err != nil || total != 1 || len(page) != 1 || page[0].ID != check.ID {
		t.Fatalf("BETA's page with ALPHA's check_fail reached = %+v, total %d (%v); want that row alone", page, total, err)
	}

	reaches := func(_ context.Context, _ pgx.Tx, deploy Event) error {
		if deploy.ID == shared.ID {
			return nil
		}
		return NoDeploy(deploy.ID)
	}
	refuses := func(_ context.Context, _ pgx.Tx, deploy Event) error { return NoDeploy(deploy.ID) }
	reject := func(id string, asks DeployCheck) (Event, error) {
		return SetState(ctx, pool, "BETA", id, StateChange{State: StateRejected, Reason: "A flaky probe.", Check: asks, Actor: person, At: *at(40)})
	}
	for _, refused := range []struct {
		what  string
		id    string
		check DeployCheck
	}{
		{"the check_fail where the check refuses its deploy", check.ID, refuses},
		{"the check_fail with no check", check.ID, nil},
		{"the posted failure", alert.ID, reaches},
		{"the deploy's own row", shared.ID, reaches},
	} {
		if _, err := reject(refused.id, refused.check); !errors.Is(err, ErrNotFound) {
			t.Errorf("BETA's reject of %s answered %v, want ErrNotFound", refused.what, err)
		}
	}
	if stored, err := Get(ctx, pool, "ALPHA", check.ID); err != nil || stored.State != StateCounted {
		t.Fatalf("after the refused writes ALPHA's check_fail is %s (%v), want counted", stored.State, err)
	}
	rejected, err := reject(check.ID, reaches)
	if err != nil || rejected.State != StateRejected || rejected.StateBy == nil || rejected.StateBy.ID != person.ID {
		t.Fatalf("BETA's reject of ALPHA's check_fail answered %+v, %v; want it rejected by %s", rejected, err, person.ID)
	}
	if stored, err := Get(ctx, pool, "ALPHA", check.ID); err != nil || stored.State != StateRejected {
		t.Fatalf("after BETA's reject ALPHA reads its check_fail %s (%v), want rejected", stored.State, err)
	}
}
