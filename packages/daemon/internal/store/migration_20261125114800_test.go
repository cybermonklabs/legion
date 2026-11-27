package store

import (
	"context"
	"reflect"
	"testing"
)

// mergeForwardOwnVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const mergeForwardOwnVersion = 20261125114800

// mergeForwardOwnRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const mergeForwardOwnRelease = "the release that marks an ask made at the task's implementation step"

// 20261125114800 marks each ask to merge forward that the release before recorded in implementing
// as made at the task's implementation step (record.MergeForward.Own): that release asked the
// implementer alone, and in implementing it asked it at its own step, so the ask covers that step
// alone, and the move on from it asks again. An ask it recorded in another phase stays unmarked, as
// does a pull request that holds no ask. The way back deletes the migration's version row alone:
// the release before reads the record leniently and drops the mark at its next write of the ask,
// so the roll forward marks again each ask in implementing that release wrote in between.
func TestAnAskTheReleaseBeforeMadeInImplementingIsMarkedAsMadeAtTheImplementationStep(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, mergeForwardOwnVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'asked in implementing', 'implementing', 1, 'in_progress', 'A', 1),
		('LEGION-209', 'LEGION-209', 'LEGION', 'asked in testing', 'testing', 1, 'testing', 'B', 1),
		('LEGION-210', 'LEGION-210', 'LEGION', 'asked nothing', 'testing', 1, 'testing', 'C', 1)`)
	// The pull requests as the release before wrote them: its ask names no mark at all.
	exec(`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red, review_seen, state, checked_head,
		required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved, conflicting_head, merge_forward)
		values ('LEGION-208', 'acme/widgets', 208, 'legion/LEGION-208', 'h1', now(), '[]', '[]', 0, 0, '[]', 1, 'h1-1', '', false, 0,
		'open', 'h1', '[]', '', 'CONFLICTING', 'dirty', 'main', '', 'h1',
		'{"head": "h1", "base": "main", "phase": "implementing", "why": "required-check-not-green", "askedAt": "2026-11-22T00:00:00Z"}'),
		('LEGION-209', 'acme/widgets', 209, 'legion/LEGION-209', 'h1', now(), '[]', '[]', 0, 0, '[]', 1, 'h1-1', '', false, 0,
		'open', 'h1', '[]', '', 'CONFLICTING', 'dirty', 'main', '', 'h1',
		'{"head": "h1", "base": "main", "phase": "testing", "why": "required-check-not-green", "askedAt": "2026-11-22T00:00:00Z"}'),
		('LEGION-210', 'acme/widgets', 210, 'legion/LEGION-210', 'h1', now(), '[]', '[]', 0, 0, '[]', 1, 'h1-1', '', false, 0,
		'open', 'h1', '[]', '', 'MERGEABLE', 'clean', 'main', '', '', null)`)

	const marks = `select issue || ' ' || coalesce(merge_forward->>'own', 'unmarked') from pull_requests order by issue`
	want := []string{"LEGION-208 true", "LEGION-209 unmarked", "LEGION-210 unmarked"}
	if got := rowsAsText(t, store, marks); !reflect.DeepEqual(got, []string{"LEGION-208 unmarked", "LEGION-209 unmarked", "LEGION-210 unmarked"}) {
		t.Fatalf("the marks before the migration = %v, want none: the fixture is not what the release before wrote", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the mark of an ask made at the implementation step: %v", err)
	}
	if got := rowsAsText(t, store, marks); !reflect.DeepEqual(got, want) {
		t.Fatalf("the marks after the migration = %v, want %v: the ask in implementing alone is made at its agent's own step", got, want)
	}

	exec(wayBackOf(t, mergeForwardOwnVersion, mergeForwardOwnRelease))
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261125114800`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the version rows of the migration after the way back = %v, want none", got)
	}
	// The release before's next write of the ask, which names no mark.
	exec(`update pull_requests set merge_forward = merge_forward - 'own' where merge_forward is not null`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if got := rowsAsText(t, store, marks); !reflect.DeepEqual(got, want) {
		t.Fatalf("the marks after the roll forward = %v, want %v", got, want)
	}
}
