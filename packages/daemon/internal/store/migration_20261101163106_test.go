package store

import (
	"context"
	"strings"
	"testing"
)

// 20261101163106 adds the table of kept decisions, kept_decisions since 20261124002047, and
// readmitted_approvals: a decision is kept under the issue it was made on, one row per Dispatch
// reference, and a root keeps one approval below its spec's latest version. Both carry the claim
// of the outbox row that names them and the hold of the delivery that carries them, and both go
// with the issue they name. The test migrates the whole set, so it reads the table by the name the
// head gives it.
func TestKeptDecisionsArriveWith20261101163106AndGoWithTheirIssue(t *testing.T) {
	t.Parallel()
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 67)
	var tables int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind = 'r' and c.relname like '%\_decisions'`).Scan(&tables); err != nil {
		t.Fatalf("look for a table of decisions at 0067: %v", err)
	}
	if tables != 0 {
		t.Fatalf("%d tables of decisions exist at 0067, want the kept decisions' added by 20261101163106", tables)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var key []string
	if err := store.Pool().QueryRow(ctx, `select array_agg(a.attname::text order by k.ord)
		from pg_index i
		cross join lateral unnest(i.indkey) with ordinality as k(attnum, ord)
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
		where i.indrelid = 'kept_decisions'::regclass and i.indisprimary`).Scan(&key); err != nil {
		t.Fatalf("read the primary key of kept_decisions: %v", err)
	}
	if strings.Join(key, ",") != "issue,ref" {
		t.Fatalf("kept_decisions primary key = %v, want (issue, ref): a decision is kept under the issue it was made on", key)
	}
	var columns []string
	if err := store.Pool().QueryRow(ctx, `select array_agg(attname::text order by attname) from pg_attribute
		where attrelid = 'kept_decisions'::regclass and attnum > 0 and not attisdropped`).Scan(&columns); err != nil {
		t.Fatalf("read the columns of kept_decisions: %v", err)
	}
	for _, column := range []string{"claimed_by", "held_by_claim", "held_by_delivery"} {
		if !strings.Contains(strings.Join(columns, ","), column) {
			t.Fatalf("kept_decisions columns = %v, want %s among them", columns, column)
		}
	}
	if strings.Contains(strings.Join(columns, ","), "root") {
		t.Fatalf("kept_decisions columns = %v, want no root: the tree a decision is owed to is read from its issue", columns)
	}

	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-210', 'LEGION-210', 'LEGION', 'root in backlog', 'done', 1, 'backlog', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into kept_decisions (issue, ref, decision, actor, at)
		values ('LEGION-210', 'dispatch://LEGION-210/spec', 'design-approved', 'user:alice', now())`); err != nil {
		t.Fatalf("keep a decision: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into kept_decisions (issue, ref, decision, actor, at)
		values ('LEGION-210', 'dispatch://LEGION-210/spec', 'design-changes-requested', 'user:bob', now())`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second row for one ref = %v, want the primary key to refuse it", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into kept_decisions (ref, decision, actor, at)
		values ('dispatch://LEGION-210/ask/ask-1', 'ask-answered', 'user:alice', now())`); err == nil ||
		!strings.Contains(err.Error(), "not-null") {
		t.Fatalf("a decision naming no issue = %v, want the not-null constraint to refuse it: a kept row records where it was decided", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into kept_decisions (issue, ref, decision, actor, at)
		values ('LEGION-404', 'dispatch://LEGION-404/spec', 'design-approved', 'user:alice', now())`); err == nil ||
		!strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("a decision on an unrecorded issue = %v, want the foreign key to refuse it", err)
	}
	for _, refused := range []struct{ why, statement, error string }{
		{"an approval at the latest version", `insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-210', 3, 3)`, "check constraint"},
		{"an approval of version 0", `insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-210', 0, 3)`, "check constraint"},
		{"an approval on an unrecorded root", `insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-404', 2, 3)`, "foreign key"},
	} {
		if _, err := store.Pool().Exec(ctx, refused.statement); err == nil || !strings.Contains(err.Error(), refused.error) {
			t.Fatalf("%s = %v, want %s to refuse it", refused.why, err, refused.error)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-210', 3, 4)`); err != nil {
		t.Fatalf("keep an older approval: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-210', 2, 4)`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second approval for one root = %v, want the primary key to refuse it", err)
	}
	if _, err := store.Pool().Exec(ctx, `delete from issues where key = 'LEGION-210'`); err != nil {
		t.Fatalf("delete the issue: %v", err)
	}
	var kept, approvals int
	if err := store.Pool().QueryRow(ctx, `select (select count(*) from kept_decisions), (select count(*) from readmitted_approvals)`).Scan(&kept, &approvals); err != nil {
		t.Fatalf("count the kept rows: %v", err)
	}
	if kept != 0 || approvals != 0 {
		t.Fatalf("%d decisions and %d approvals kept after their issue was deleted, want none", kept, approvals)
	}
}
