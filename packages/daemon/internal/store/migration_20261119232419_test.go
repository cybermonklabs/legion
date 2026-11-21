package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// backstopArmedAtVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const backstopArmedAtVersion = 20261119232419

// 20261119232419 adds pod_wake_backstops.armed_at, the moment the release that armed a backstop
// computed its due time from, which the backstop of an issue waiting on another issue is
// measured from (dispatch://INFRA-2746 R9). No row written before it says when it was armed, so
// every row the upgrade finds, open or fired, reads its own due time: such a row holds the pod of
// a waiting issue at most backstop_minutes past a day. A row inserted afterwards by a write that
// does not name the column — a rolled-back release's own arming, or a test's insert by hand —
// reads the moment of its insert, which is when that release arms it.
//
// The negative control is the write of the column before the migration, which the database
// refuses as a column it does not know.
func TestEveryBackstopRowTheUpgradeFindsReadsArmedAtItsDueTimeAndALaterInsertReadsNow(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, backstopArmedAtVersion-1)

	open := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	fired := open.Add(-3 * time.Hour)
	for _, seed := range []struct {
		role string
		due  time.Time
		gone bool
	}{
		{role: "implementer", due: open},
		{role: "tester", due: fired, gone: true},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into pod_wake_backstops (project, issue, role, release_id, due_at, fired)
			values ('legion', 'LEGION-208', $1, 0, $2, $3)`, seed.role, seed.due, seed.gone); err != nil {
			t.Fatalf("record the %s backstop before the migration: %v", seed.role, err)
		}
	}
	_, err := store.Pool().Exec(ctx, `insert into pod_wake_backstops (project, issue, role, release_id, armed_at, due_at)
		values ('legion', 'LEGION-209', 'implementer', 0, now(), now())`)
	if err == nil || !strings.Contains(err.Error(), "armed_at") {
		t.Fatalf("writing armed_at before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the backstop's arm time: %v", err)
	}

	armedAt := func(role string) time.Time {
		t.Helper()
		var at time.Time
		if err := store.Pool().QueryRow(ctx,
			"select armed_at from pod_wake_backstops where issue = 'LEGION-208' and role = $1", role,
		).Scan(&at); err != nil {
			t.Fatalf("read the %s backstop's arm time after the migration: %v", role, err)
		}
		return at.UTC()
	}
	for role, due := range map[string]time.Time{"implementer": open, "tester": fired} {
		if got := armedAt(role); !got.Equal(due) {
			t.Errorf("the %s backstop the upgrade found reads armed_at %s, want its own due time %s: no row before the column said when it was armed, "+
				"and its due time holds a waiting issue's pod at most backstop_minutes past a day", role, got, due)
		}
	}

	// An insert that names no such column, as the release before writes its own rows.
	if _, err := store.Pool().Exec(ctx, `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ('legion', 'LEGION-208', 'reviewer', 0, now() + interval '1 hour')`); err != nil {
		t.Fatalf("insert a backstop row that does not name armed_at: %v", err)
	}
	var armed, now time.Time
	if err := store.Pool().QueryRow(ctx,
		"select armed_at, now() from pod_wake_backstops where issue = 'LEGION-208' and role = 'reviewer'",
	).Scan(&armed, &now); err != nil {
		t.Fatalf("read the arm time of a backstop inserted after the migration: %v", err)
	}
	if gap := now.Sub(armed); gap < 0 || gap > time.Second {
		t.Errorf("a backstop inserted after the migration without naming armed_at reads %s, %s from the database's now() %s, want the moment of its insert, within a second",
			armed.UTC(), gap, now.UTC())
	}
}
