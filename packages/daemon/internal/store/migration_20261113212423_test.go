package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// designNewsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const designNewsVersion = 20261113212423

// 20261113212423 records what each holder of a tree's work took of its approved design
// (design_news_told), the change sets between two approved versions (design_changes), the news a
// delivery carries (pending_task_deliveries.design_news) and the outbox kind that reconciles them
// (design_changes), and seeds the told rows from the gates the release before it left, so the
// upgrade owes nobody news of what the record already shows as taken.
//
// The seed is read on five trees that release can leave: a tree whose spec gate stands open at
// version 4 with two members in the workflow; a tree whose gate a changes request closed, which
// cleared its approved version (classify.ApplyDesignGateEvent), so what its holders took is
// unknown; a tree whose gate a new version closed, which keeps the approval of version 4 it closed;
// a tree whose gate never opened, waiting for its first approval; and a member already done. Each
// member in the workflow under an approved version gets a holder row at that version and each such
// root an architect row. The tree a changes request closed gets the same rows, each knowing no
// version, so the member's next phase entry takes no baseline over the news it is owed. The tree
// that never opened gets none: nothing of it has started, so its first opening and each member's
// first phase take their own. The done member gets none.
//
// The negative control is the schema before the migration, which holds no told rows table and
// refuses a design_changes outbox row; after it, every kind the constraint already named is still
// taken.
func TestTheDesignNewsSeedOwesNobodyWhatTheRecordShowsTakenWith20261113212423(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, designNewsVersion-1)

	issue := func(key, tree, phase, status string) string {
		return `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('` + key + `', '` + tree + `', 'LEGION', '` + key + `', '` + phase + `', 1, '` + status + `', '` + key + `', 1)`
	}
	for _, seed := range []string{
		// The tree whose spec gate stands open at version 4, with two members in the workflow and
		// one done.
		issue("LEGION-100", "LEGION-100", "admitted", "in_progress"),
		issue("LEGION-101", "LEGION-100", "implementing", "in_progress"),
		issue("LEGION-102", "LEGION-100", "done", "done"),
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-100', 'spec', 'doc-100', 4, 4, true)`,
		// The tree a changes request closed: the review cleared the approval and was told.
		issue("LEGION-200", "LEGION-200", "admitted", "in_progress"),
		issue("LEGION-201", "LEGION-200", "implementing", "in_progress"),
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened,
			told_changes_version, told_changes_reason)
			values ('LEGION-200', 'spec', 'doc-200', 4, null, true, 4, 'Name the rollback.')`,
		// The tree a new version closed: version 5 stands unapproved above the approval of 4.
		issue("LEGION-300", "LEGION-300", "admitted", "in_progress"),
		issue("LEGION-301", "LEGION-300", "reviewing", "needs_review"),
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-300', 'spec', 'doc-300', 5, 4, true)`,
		// The tree whose gate never opened: its architect registered the spec, and no person has
		// approved it yet, so its member waits admitted.
		issue("LEGION-400", "LEGION-400", "admitted", "in_progress"),
		issue("LEGION-401", "LEGION-400", "admitted", "todo"),
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-400', 'spec', 'doc-400', 1, null, false)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-100', '{}', 0, now(), '')`, kind)
		return err
	}
	if _, err := store.Pool().Exec(ctx, "select 1 from design_news_told"); err == nil || !strings.Contains(err.Error(), "design_news_told") {
		t.Fatalf("reading the told rows before the migration = %v, want the table to be unknown", err)
	}
	if err := queue("design_changes"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a design_changes row before the migration = %v, want the kind constraint to refuse it", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the design news: %v", err)
	}

	rows, err := store.Pool().Query(ctx, `select issue, audience, versions, opening_owed, told_at is not null
		from design_news_told order by issue, audience`)
	if err != nil {
		t.Fatalf("read the seeded told rows: %v", err)
	}
	defer rows.Close()
	got := map[string]map[string]int{}
	for rows.Next() {
		var issue, audience string
		var versions []byte
		var owed, told bool
		if err := rows.Scan(&issue, &audience, &versions, &owed, &told); err != nil {
			t.Fatalf("scan a seeded told row: %v", err)
		}
		if owed || !told {
			t.Fatalf("%s's %s row owes the opening %t and was told %t, want no opening owed and a time it was taken", issue, audience, owed, told)
		}
		var steps map[string]int
		if err := json.Unmarshal(versions, &steps); err != nil {
			t.Fatalf("decode %s's %s versions %s: %v", issue, audience, versions, err)
		}
		got[issue+"/"+audience] = steps
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the seeded told rows: %v", err)
	}
	at4, unknown := map[string]int{"spec": 4}, map[string]int{}
	want := map[string]map[string]int{
		"LEGION-100/architect": at4, "LEGION-100/holder": at4, "LEGION-101/holder": at4,
		"LEGION-200/architect": unknown, "LEGION-200/holder": unknown, "LEGION-201/holder": unknown,
		"LEGION-300/architect": at4, "LEGION-300/holder": at4, "LEGION-301/holder": at4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the seeded told rows = %v, want %v: a holder row at version 4 for each member in the workflow under an approval and an architect row for its root, rows that know no version for the tree a changes request closed, none for the tree that never opened, and none for the done member", got, want)
	}

	if _, err := store.Pool().Exec(ctx, `insert into design_changes (artifact_id, from_version, to_version, state, reason, read_at)
		values ('doc-100', 4, 5, 'unavailable', 'dispatch has no changes route', now())`); err != nil {
		t.Fatalf("record an unreadable change set after the migration: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into design_changes (artifact_id, from_version, to_version, state, reason, read_at)
		values ('doc-100', 5, 6, 'unavailable', '', now())`); err == nil {
		t.Fatal("recording an unreadable change set with no reason was taken, want it refused")
	}
	if _, err := store.Pool().Exec(ctx, "select design_news from pending_task_deliveries"); err != nil {
		t.Fatalf("read the news a delivery carries after the migration: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "design_changes",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
