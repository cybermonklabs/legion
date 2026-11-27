package store

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// keptDecisionsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const keptDecisionsVersion = 20261124002047

// keptDecisionsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const keptDecisionsRelease = "the release that renames the kept decisions"

// 20261124002047 gives the table of kept decisions, its index and its constraints the name
// kept_decisions, and moves the three stored values the release before wrote under its own
// words: the kind of a queued controller notice that relays a kept decision, the processed-event
// source that keeps one decision to one notice, and a queued tick's held reason for a member of a
// tree whose root is in backlog or icebox. It moves nothing else: a tick's other reasons, the
// source of a catch-up's telling and the rows themselves stay as they were. Its way back, the last
// paragraph of the migration's header (migrations.WayBack), gives every name and value back as the
// release before made it and deletes the migration's version row, so the roll forward's Migrate
// applies the file again and the new names come back.
//
// The seeds and the reads of the release before are in its own names, since they are what it
// wrote and what it reads.
func TestTheKeptDecisionsTakeTheirNamesAndTheirWayBackGivesTheOldOnesAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, keptDecisionsVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	queue := func(kind, issue, payload string) int64 {
		t.Helper()
		var id int64
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '') returning id`, kind, issue, payload).Scan(&id); err != nil {
			t.Fatalf("queue the %s row %s: %v", kind, payload, err)
		}
		return id
	}

	// What the release before leaves: a root a person moved to backlog with its child, a decision
	// kept on each, the one on the child claimed by a catch-up, the controller notice that relayed
	// the root's decision with the mark that keeps it to one notice, and two ticks.
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-210', 'LEGION-210', 'LEGION', 'root in backlog', 'done', 1, 'backlog', 'A', 1)`)
	exec(`insert into issues (key, tree, parent, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-211', 'LEGION-210', 'LEGION-210', 'LEGION', 'child', 'planning', 1, 'in_progress', 'B', 1)`)
	catchUp := queue("notice", "LEGION-210", `{"kind": "catch-up"}`)
	const (
		oldNotice  = `{"kind": "parked-decision", "decision": "design-approved", "root": "LEGION-210", "ref": "dispatch://LEGION-210/spec", "actor": "user:alice", "version": 3}`
		newNotice  = `{"kind": "kept-decision", "decision": "design-approved", "root": "LEGION-210", "ref": "dispatch://LEGION-210/spec", "actor": "user:alice", "version": 3}`
		oldTick    = `{"kind": "tick", "held": [{"issue": "LEGION-211", "reason": "tree_parked", "heldBy": ["LEGION-210"]}, {"issue": "LEGION-300", "reason": "tree_closed", "heldBy": ["LEGION-299"]}]}`
		newTick    = `{"kind": "tick", "held": [{"issue": "LEGION-211", "reason": "tree_in_backlog_or_icebox", "heldBy": ["LEGION-210"]}, {"issue": "LEGION-300", "reason": "tree_closed", "heldBy": ["LEGION-299"]}]}`
		gatedTick  = `{"kind": "tick", "held": [{"issue": "LEGION-300", "reason": "design_gate", "heldBy": ["LEGION-299"]}]}`
		bareTick   = `{"kind": "tick"}`
		catchUpRow = `{"kind": "catch-up"}`
	)
	notice := queue("controller_notice", "LEGION-210", oldNotice)
	tick := queue("controller_notice", "LEGION", oldTick)
	gated := queue("controller_notice", "LEGION", gatedTick)
	bare := queue("controller_notice", "LEGION", bareTick)
	exec(`insert into parked_decisions (issue, ref, decision, actor, at) values
		('LEGION-210', 'dispatch://LEGION-210/spec', 'design-approved', 'user:alice', '2026-11-24T09:00:00Z'),
		('LEGION-211', 'dispatch://LEGION-211/ask/ask-1', 'ask-answered', 'user:bob', '2026-11-24T10:00:00Z')`)
	exec(`update parked_decisions set claimed_by = $1 where ref = 'dispatch://LEGION-211/ask/ask-1'`, catchUp)
	exec(`insert into processed_events (source, event_id) values ('parked-decision', 'legion-outbox:41'), ('kept-told', 'legion-outbox:41')`)

	constraints := func(table string) []string {
		t.Helper()
		return rowsAsText(t, store, fmt.Sprintf(`select conname::text from pg_constraint where conrelid = '%s'::regclass order by conname`, table))
	}
	indexes := func(table string) []string {
		t.Helper()
		return rowsAsText(t, store, fmt.Sprintf(`select indexname::text from pg_indexes where schemaname = 'public' and tablename = '%s' order by indexname`, table))
	}
	renamed := func(names []string) []string {
		var moved []string
		for _, name := range names {
			moved = append(moved, "kept_decisions_"+strings.TrimPrefix(name, "parked_decisions_"))
		}
		return moved
	}
	// The release before's names, read from the catalog rather than written here: PostgreSQL 18
	// names each NOT NULL constraint after the table, and an older server names none.
	oldConstraints, oldIndexes := constraints("parked_decisions"), indexes("parked_decisions")
	for _, name := range []string{"parked_decisions_pkey", "parked_decisions_issue_fkey", "parked_decisions_claimed_by_fkey"} {
		if !slices.Contains(oldConstraints, name) {
			t.Fatalf("the release before's constraints of the kept decisions = %v, want %s among them", oldConstraints, name)
		}
	}
	if want := []string{"parked_decisions_issue", "parked_decisions_pkey"}; !reflect.DeepEqual(oldIndexes, want) {
		t.Fatalf("the release before's indexes of the kept decisions = %v, want %v", oldIndexes, want)
	}
	const decisions = `select row(issue, ref, decision, actor, at, coalesce(claimed_by, 0), coalesce(held_by_claim, ''), coalesce(held_by_delivery, ''))::text from %s order by ref`
	keptRows := rowsAsText(t, store, fmt.Sprintf(decisions, "parked_decisions"))
	if len(keptRows) != 2 {
		t.Fatalf("the release before's kept decisions = %v, want the two seeded", keptRows)
	}
	payloadIs := func(id int64, want string) bool {
		t.Helper()
		var same bool
		if err := store.Pool().QueryRow(ctx, `select payload = $2::jsonb from outbox where id = $1`, id, want).Scan(&same); err != nil {
			t.Fatalf("read the payload of row %d: %v", id, err)
		}
		return same
	}
	const sources = `select source from processed_events where event_id = 'legion-outbox:41' order by source`
	const version = `select count(*)::text from schema_version where version = 20261124002047`

	// The new names, and the values in the new words, with everything else as it was.
	newNames := func(when string) {
		t.Helper()
		if got := rowsAsText(t, store, `select coalesce(to_regclass('parked_decisions')::text, '')`); !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("%s the table of the release before's name = %v, want none", when, got)
		}
		if got, want := constraints("kept_decisions"), renamed(oldConstraints); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s the constraints of kept_decisions = %v, want %v", when, got, want)
		}
		if got, want := indexes("kept_decisions"), renamed(oldIndexes); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s the indexes of kept_decisions = %v, want %v", when, got, want)
		}
		if got := rowsAsText(t, store, fmt.Sprintf(decisions, "kept_decisions")); !reflect.DeepEqual(got, keptRows) {
			t.Fatalf("%s the kept decisions = %v, want the rows the release before kept, %v", when, got, keptRows)
		}
		for _, row := range []struct {
			id   int64
			want string
		}{{notice, newNotice}, {tick, newTick}, {gated, gatedTick}, {bare, bareTick}, {catchUp, catchUpRow}} {
			if !payloadIs(row.id, row.want) {
				t.Fatalf("%s the payload of row %d = %v, want %s", when, row.id,
					rowsAsText(t, store, fmt.Sprintf(`select payload::text from outbox where id = %d`, row.id)), row.want)
			}
		}
		if got, want := rowsAsText(t, store, sources), []string{"kept-decision", "kept-told"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s the sources of legion-outbox:41 = %v, want %v", when, got, want)
		}
		if got := rowsAsText(t, store, `select relname::text from pg_class c join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'public' and relname ~* 'park'
			union all select conname::text from pg_constraint c join pg_namespace n on n.oid = c.connamespace
			where n.nspname = 'public' and conname ~* 'park'`); len(got) != 0 {
			t.Fatalf("%s the catalog still names %v in the release before's word", when, got)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the kept decisions' names: %v", err)
	}
	newNames("after the migration")

	exec(wayBackOf(t, keptDecisionsVersion, keptDecisionsRelease))
	if got := rowsAsText(t, store, `select coalesce(to_regclass('kept_decisions')::text, '')`); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("after the way back the table of the new name = %v, want none", got)
	}
	if got := constraints("parked_decisions"); !reflect.DeepEqual(got, oldConstraints) {
		t.Fatalf("after the way back the constraints of parked_decisions = %v, want the release before's %v", got, oldConstraints)
	}
	if got := indexes("parked_decisions"); !reflect.DeepEqual(got, oldIndexes) {
		t.Fatalf("after the way back the indexes of parked_decisions = %v, want the release before's %v", got, oldIndexes)
	}
	if got := rowsAsText(t, store, fmt.Sprintf(decisions, "parked_decisions")); !reflect.DeepEqual(got, keptRows) {
		t.Fatalf("after the way back the kept decisions = %v, want %v", got, keptRows)
	}
	for _, row := range []struct {
		id   int64
		want string
	}{{notice, oldNotice}, {tick, oldTick}, {gated, gatedTick}, {bare, bareTick}, {catchUp, catchUpRow}} {
		if !payloadIs(row.id, row.want) {
			t.Fatalf("after the way back the payload of row %d = %v, want the release before's %s", row.id,
				rowsAsText(t, store, fmt.Sprintf(`select payload::text from outbox where id = %d`, row.id)), row.want)
		}
	}
	if got, want := rowsAsText(t, store, sources), []string{"kept-told", "parked-decision"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back the sources of legion-outbox:41 = %v, want %v", got, want)
	}
	if got := rowsAsText(t, store, version); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("after the way back the version row of the kept decisions' names stands %v times, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	newNames("after the roll forward")
	if got := rowsAsText(t, store, version); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("after the roll forward the version row of the kept decisions' names stands %v times, want once", got)
	}
}
