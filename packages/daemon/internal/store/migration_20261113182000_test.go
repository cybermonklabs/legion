package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectUpgradeVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const architectUpgradeVersion = 20261113182000

// 20261113182000 settles every root architect the release before this one left running: the
// architect's phases row at entry 1 with no task and no moment raised, its claim queued that
// entry and confirming it, and the first and third stall arms marked told for that root and run.
// A release that read those claims as owed their entry would send a first task to an architect
// that is writing its tree's spec, and the record cannot tell that architect from a stuck one.
//
// The rows are read on the shape the older release leaves: a root at admitted whose architect
// claim exists and whose architect has no phases row at all. Three negative controls stand with
// it, each the same shape with one thing changed, so the fill is read as the rule it is and not
// as a sweep of the table: a child's own sub-architect claim, which the rule leaves outside; a
// root with no architect claim, which the daemon never started; and a role of the same root that
// is not its architect, whose own entry the earlier migration decides.
//
// The marks are read on two more roots, each a root the older daemon re-admitted before the
// upgrade, so each stands at a generation its architect claim was not started at. One still
// waits in the line: it holds no slot, and its next run has not started, so the fill marks no arm
// of it and that run's arms wake the controller as any run's do. The other was promoted: it holds
// its slot and its start is queued, so its run has started under the older release, and the fill
// marks it as it marks every root standing in a run.
func TestTheArchitectUpgradeSettlesEveryRootArchitectWith20261113182000(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, architectUpgradeVersion-1)

	for _, seed := range []string{
		// The root the fill repairs: its architect claim stands, its architect holds no row.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'admitted', 3, 'in_progress', 'A', 1)`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation)
			values ('legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 3, 'ses_a', '',
				'working', 0, 0, 0, 0, 3)`,
		// A worker of that same root, whose phases row the phase entry migration already settled.
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
			values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 0, '', 4, 'Implement the change.')`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, given_entry, served_entry)
			values ('legion-LEGION-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 3, 'ses_i', '',
				'working', 0, 0, 0, 0, 3, 4, 4)`,
		// A child of that tree whose own sub-architect claim an operator started: outside the
		// rule, because no daemon start creates one.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, parent)
			values ('LEGION-209', 'LEGION-208', 'LEGION', 'child', 'admitted', 3, 'in_progress', 'B', 1, 'LEGION-208')`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation)
			values ('legion-LEGION-209-architect', 'legion', 'LEGION-208', 'LEGION-209', 'architect', 3, 'ses_c', '',
				'working', 0, 0, 0, 0, 3)`,
		// A root the daemon holds no architect claim of: nothing of it was ever started.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-210', 'LEGION-210', 'LEGION', 'unstarted root', 'admitted', 1, 'in_progress', 'C', 1)`,
		// A root the older daemon re-admitted, still waiting in the line: its generation is
		// raised to 4 and its status is todo, while its architect claim is the retired one of
		// run 3, the run that ended.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-211', 'LEGION-211', 'LEGION', 'queued root', 'admitted', 4, 'todo', 'D', 1)`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation)
			values ('legion-LEGION-211-architect', 'legion', 'LEGION-211', 'LEGION-211', 'architect', 3, 'ses_q', '',
				'retired', 0, 0, 0, 0, 3)`,
		// A root the older daemon re-admitted and then promoted: it holds its slot at generation
		// 5, and the start of its architect is queued, so the claim is still the one of run 4.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-212', 'LEGION-212', 'LEGION', 'promoted root', 'admitted', 5, 'in_progress', 'E', 1)`,
		`insert into slots (issue, index, admitted_at) values ('LEGION-212', 0, now())`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation)
			values ('legion-LEGION-212-architect', 'legion', 'LEGION-212', 'LEGION-212', 'architect', 4, 'ses_p', '',
				'retired', 0, 0, 0, 0, 4)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the architect upgrade: %v", err)
	}

	var entry int64
	var entryTask string
	var raisedAt *time.Time
	if err := store.Pool().QueryRow(ctx, `select entry, entry_task, entry_raised_at from phases
		where issue = 'LEGION-208' and role = 'architect'`).Scan(&entry, &entryTask, &raisedAt); err != nil {
		t.Fatalf("read the architect's phases row after the migration: %v", err)
	}
	if entry != 1 || entryTask != "" || raisedAt != nil {
		t.Fatalf("the architect's phases row = entry %d raised at %v carrying %q; want entry 1 with no task and no moment: "+
			"the fill settles the entry it finds running and claims no work of its own", entry, raisedAt, entryTask)
	}
	var given, served int64
	if err := store.Pool().QueryRow(ctx, `select given_entry, served_entry from claims
		where token = 'legion-LEGION-208-architect'`).Scan(&given, &served); err != nil {
		t.Fatalf("read the architect's claim after the migration: %v", err)
	}
	if given != 1 || served != 1 {
		t.Fatalf("the architect's claim was given entry %d and served entry %d, want 1 and 1: a claim at the entry its row names is owed nothing",
			given, served)
	}
	var queued int64
	if err := store.Pool().QueryRow(ctx, `select count(*) from pending_task_deliveries
		where claim_token = 'legion-LEGION-208-architect'`).Scan(&queued); err != nil {
		t.Fatalf("count the tasks queued to the architect's claim: %v", err)
	}
	if queued != 0 {
		t.Fatalf("the architect's claim holds %d queued tasks, want none: the two numbers with no times beside them are how a reader "+
			"tells a root of the older release from one this release served", queued)
	}

	if arms := architectUpgradeMarks(t, store, "LEGION-208", 3); !slices.Equal(arms,
		[]record.ArchitectStallArm{record.ArchitectSlowBeforeGate, record.ArchitectTookNothing}) {
		t.Fatalf("the arms LEGION-208 is marked told for = %v, want the first and the third: the second reports a claim that is failed "+
			"or retired with no start queued to relaunch it, which no send of the sweep cures", arms)
	}

	if arms := architectUpgradeMarks(t, store, "LEGION-212", 5); !slices.Equal(arms,
		[]record.ArchitectStallArm{record.ArchitectSlowBeforeGate, record.ArchitectTookNothing}) {
		t.Fatalf("the arms the promoted LEGION-212 is marked told for = %v, want the first and the third: its run started under "+
			"the older release, as every root the fill marks did", arms)
	}
	for _, generation := range []uint64{3, 4} {
		if arms := architectUpgradeMarks(t, store, "LEGION-211", generation); len(arms) != 0 {
			t.Fatalf("the queued LEGION-211 is marked told for %v at generation %d after the migration, want no arm: its next run "+
				"has not started, so a mark would keep that run's first and third arms from waking anybody", arms, generation)
		}
	}

	for _, quiet := range []struct {
		issue, why string
		generation uint64
	}{
		{issue: "LEGION-209", generation: 3, why: "a child's own sub-architect claim is outside the rule, since no daemon start creates one"},
		{issue: "LEGION-210", generation: 1, why: "the daemon holds no architect claim of that root, so it started nothing there"},
	} {
		var rows int64
		if err := store.Pool().QueryRow(ctx, `select count(*) from phases where issue = $1 and role = 'architect'`,
			quiet.issue).Scan(&rows); err != nil {
			t.Fatalf("count the architect phases rows of %s: %v", quiet.issue, err)
		}
		if rows != 0 {
			t.Fatalf("%s holds %d architect phases rows after the migration, want none: %s", quiet.issue, rows, quiet.why)
		}
		if arms := architectUpgradeMarks(t, store, quiet.issue, quiet.generation); len(arms) != 0 {
			t.Fatalf("%s is marked told for %v after the migration, want no arm: %s", quiet.issue, arms, quiet.why)
		}
	}
	var childGiven int64
	if err := store.Pool().QueryRow(ctx, `select given_entry from claims
		where token = 'legion-LEGION-209-architect'`).Scan(&childGiven); err != nil {
		t.Fatalf("read the child's sub-architect claim after the migration: %v", err)
	}
	if childGiven != 0 {
		t.Fatalf("the child's sub-architect claim was given entry %d, want 0: the fill reaches an architect claim whose issue is its own tree's root",
			childGiven)
	}

	var workerEntry, workerGiven int64
	if err := store.Pool().QueryRow(ctx, `select p.entry, c.given_entry from phases p, claims c
		where p.issue = 'LEGION-208' and p.role = 'implementer' and c.token = 'legion-LEGION-208-implementer'`).
		Scan(&workerEntry, &workerGiven); err != nil {
		t.Fatalf("read the implementer's row and claim after the migration: %v", err)
	}
	if workerEntry != 4 || workerGiven != 4 {
		t.Fatalf("the implementer stands at entry %d with its claim given %d, want both at the 4 they stood at: this fill is the architect's alone",
			workerEntry, workerGiven)
	}
}

// architectUpgradeMarks is every stall arm issue is marked told for at generation, in arm order,
// whatever tree the mark names. A mark of a child names the tree the child stands in, so a read
// of the marks whose tree is the issue's own key would pass over it.
func architectUpgradeMarks(t *testing.T, store *Store, issue string, generation uint64) []record.ArchitectStallArm {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `select arm from architect_stalled_notices
		where issue = $1 and generation = $2 order by arm`, issue, int64(generation))
	if err != nil {
		t.Fatalf("list the arms %s is marked told for: %v", issue, err)
	}
	defer rows.Close()
	var arms []record.ArchitectStallArm
	for rows.Next() {
		var arm string
		if err := rows.Scan(&arm); err != nil {
			t.Fatalf("read an arm %s is marked told for: %v", issue, err)
		}
		arms = append(arms, record.ArchitectStallArm(arm))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the arms %s is marked told for: %v", issue, err)
	}
	return arms
}
