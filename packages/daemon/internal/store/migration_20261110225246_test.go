package store

import (
	"context"
	"strings"
	"testing"
)

// backwardMovesVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const backwardMovesVersion = 20261110225246

// A hook-prompt row carries a hook's prompt to one role of the issue, and the counts of the moves
// back it is fired by ride on the implementer's phase row. A write naming a column that does not
// exist is refused, so before the migration neither count can be written: that refusal is the
// negative control, read before it. The kind is no control here any more, because this release's
// own 20261108152000 stands below this migration and already names `hook_prompt` — it has to, so
// that a database main migrated, which applies that list last, takes the hook-prompt rows main's
// daemon queued (migration_upgrade_from_main_test.go). The row is written before the migration
// for that reason, and the merge-lane kind below is what the constraint still refuses. After it the row is queued, and so is every kind the
// constraint already named, since the migration rewrites the list whole and a kind left out would
// stop its own effect. The list is the merged daemon's: this release's eighteen, `role_message`
// among them and the three merge-lane kinds out, since the daemon submits no merge and arms no
// auto-merge, with the hook prompt this migration adds. A kind that is none is still refused, and
// so is a merge-lane kind, which the row below reads. The counts arrive empty on a row
// recorded before the migration, which is the deploy's own reading: no count is filled in from
// the past, and no row has told a cap.
func TestTheOutboxTakesTheHookPromptKindAndThePhaseTakesItsCountsWith20261110225246(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, backwardMovesVersion-1)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 2, '')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	if err := queue("hook_prompt"); err != nil {
		t.Fatalf("queueing a hook_prompt row before the migration = %v, want it taken: this release's 20261108152000 names the kind below this version", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row before the migration = %v, want the kind constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
	_, err := store.Pool().Exec(ctx,
		`update phases set backward_moves = '{"testing-implementing": 1}'::jsonb, cap_told = 3 where issue = 'LEGION-208' and role = 'implementer'`)
	if err == nil || !strings.Contains(err.Error(), "backward_moves") {
		t.Fatalf("counting a move back before the migration = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the count is refused: %v", err)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the hook-prompt kind: %v", err)
	}

	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, hook_prompt among them")
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}

	var moves string
	var capTold, rounds int
	if err := store.Pool().QueryRow(ctx,
		"select backward_moves::text, cap_told, rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&moves, &capTold, &rounds); err != nil {
		t.Fatalf("read the phases row after the migration: %v", err)
	}
	if moves != "{}" || capTold != 0 || rounds != 2 {
		t.Fatalf("the row recorded before the migration reads backward_moves=%s cap_told=%d rounds=%d, want {} and 0 beside the two rounds it already had: no count is filled in from the past",
			moves, capTold, rounds)
	}
	if _, err := store.Pool().Exec(ctx,
		`update phases set backward_moves = '{"testing-implementing": 1}'::jsonb, cap_told = 3 where issue = 'LEGION-208' and role = 'implementer'`); err != nil {
		t.Fatalf("count a move back after the migration: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		"select backward_moves::text, cap_told from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&moves, &capTold); err != nil {
		t.Fatalf("read the counted move back: %v", err)
	}
	if !strings.Contains(moves, "testing-implementing") || capTold != 3 {
		t.Fatalf("after counting one move back the row reads backward_moves=%s cap_told=%d, want the pair counted and the cap told", moves, capTold)
	}
}
