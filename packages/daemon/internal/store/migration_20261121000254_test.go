package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// workHoldsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const workHoldsVersion = 20261121000254

// workHoldsRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const workHoldsRelease = "the release that records each tree the work hold holds"

// 20261121000254 adds work_holds, the record of each tree the work hold holds (record.WorkHold): a
// database the release before left has no such table, and one this migration took stores a hold
// and reads it back. The way back, the last paragraph of the migration's header
// (migrations.WayBack), leaves the outbox as the release before decodes it: it deletes each
// queued stop of the hold, takes the hold's keys off every other row — a start's opening
// sentences, a role message's wake and stop — and leaves each row that carries none of them as it
// was. Then it drops the table and deletes the migration's own version row, so the roll-forward
// runs it again and finds the table empty.
func TestTheWorkHoldsRecordAndItsWayBackLeaveWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, workHoldsVersion-1)
	if _, err := store.Pool().Exec(ctx, `select 1 from work_holds`); !undefinedTable(err) {
		t.Fatalf("the database the release before left read work_holds = %v, want no such table (42P01): the migration would prove nothing", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the work hold's record: %v", err)
	}
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: "admitted", Generation: 4,
		Status: "in_progress", Rank: "U", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	hold := record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 4, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 4, ClosedAt: time.Date(2026, 11, 21, 0, 2, 54, 0, time.UTC)}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		if err := record.NewStore().PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return record.NewStore().PutWorkHold(ctx, tx, hold)
	}); err != nil {
		t.Fatalf("record the hold: %v", err)
	}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		read, err := record.NewStore().WorkHold(ctx, tx, "LEGION-208")
		if err == nil && (read == nil || !reflect.DeepEqual(*read, hold)) {
			t.Fatalf("the hold reads back %+v, want %+v", read, hold)
		}
		return err
	}); err != nil {
		t.Fatalf("read the hold: %v", err)
	}

	stop := `{"op":"suspend","tree":"LEGION-208","role":"implementer","generation":4,"reason":"the work hold","suspendedBy":"hold"}`
	timed := `{"op":"suspend","tree":"LEGION-208","role":"tester","generation":4,"reason":"the work hold","suspendedBy":"hold",` +
		`"deadline":"2026-11-21T00:04:54Z","turnAfter":"2026-11-21T00:02:54Z"}`
	daemonStop := `{"op":"suspend","tree":"LEGION-208","role":"planner","generation":4,"reason":"the tree closed"}`
	opening := `{"op":"start","tree":"LEGION-208","role":"reviewer","generation":4,"task":"The work hold stopped your turn.\n\nThe issue's other roles may ask you questions.","opening":"The work hold stopped your turn."}`
	held := `{"role":"legion-LEGION-208-merger","body":"End your turn now.","wake":true,"stop":` + stop + `}`
	plain := `{"role":"legion-LEGION-208-architect","body":"The design gate of LEGION-208 opened."}`
	for _, row := range [][2]string{{"supervise", stop}, {"supervise", timed}, {"supervise", daemonStop}, {"supervise", opening},
		{"role_message", held}, {"role_message", plain}} {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', $2, 0, now(), '')`, row[0], row[1]); err != nil {
			t.Fatalf("queue %s: %v", row[1], err)
		}
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, workHoldsVersion, workHoldsRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}

	decoded := func(payload string) map[string]any {
		var value map[string]any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		return value
	}
	want := []map[string]any{decoded(daemonStop), decoded(opening), decoded(held), decoded(plain)}
	delete(want[1], "opening")
	delete(want[2], "wake")
	delete(want[2], "stop")
	var got []map[string]any
	for _, payload := range rowsAsText(t, store, `select payload::text from outbox order by id`) {
		got = append(got, decoded(payload))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the way back holds %v, want %v: no stop of the hold, and no key of it on any other row", got, want)
	}
	var table, recorded bool
	if err := store.Pool().QueryRow(ctx, `select to_regclass('work_holds') is not null,
		exists (select 1 from schema_version where version = $1)`, workHoldsVersion).Scan(&table, &recorded); err != nil || table || recorded {
		t.Fatalf("after the way back the table stands = %v and the version row = %v (%v), want neither", table, recorded, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	if holds := rowsAsText(t, store, `select tree from work_holds`); len(holds) != 0 {
		t.Fatalf("after the roll-forward work_holds holds %v, want it empty", holds)
	}
}

// undefinedTable is whether err is Postgres refusing a statement over a table it does not hold
// (42P01, undefined_table).
func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
