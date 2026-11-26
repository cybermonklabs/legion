package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// conflictReadsVersion is the version of the migration this file tests, so the tests can migrate
// through everything before it whatever lands beside it.
const conflictReadsVersion = 20261119003157

// A conflict_confirm row is the second read of a head whose first read found it conflicting, and a
// merge_forward row is one message about a confirmed conflict the daemon acted on. The outbox
// refuses a kind its constraint does not name, so neither could be queued before this migration:
// the negative control is that refusal, read before the migration, on a database that carries the
// plan-record list (20261117023932), which this version stands above. After it
// both are queued, and so is every kind the constraint already named, phase_ended among them,
// since the migration rewrites the list whole and a rollback to the release before still queues
// that kind. A kind that is none is still refused.
//
// The phase_ended rows queued before the migration are what it deletes: each tells a role its
// phase ended on a conflict, a move this release no longer makes. A row of another kind queued
// beside them stays.
func TestTheOutboxTakesTheConflictConfirmAndMergeForwardKindsWith20261119003157(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, conflictReadsVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	count := func(kind string) int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = $1`, kind).Scan(&n); err != nil {
			t.Fatalf("count the %s rows: %v", kind, err)
		}
		return n
	}
	added := []string{"conflict_confirm", "merge_forward"}
	for _, kind := range added {
		err := queue(kind)
		if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row before the migration = %v, want the kind constraint to refuse it: it is a row the migration exists for", kind, err)
		}
		t.Logf("before the migration a %s row is refused: %v", kind, err)
	}
	for _, kind := range []string{"phase_ended", "phase_ended", "notice"} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row before the migration: %v", kind, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the conflict reads and the merge forward: %v", err)
	}
	if n := count("phase_ended"); n != 0 {
		t.Fatalf("phase_ended rows after the migration = %d, want the two queued before it deleted", n)
	}
	if n := count("notice"); n != 1 {
		t.Fatalf("notice rows after the migration = %d, want the one queued before it kept", n)
	}
	t.Logf("the migration deleted the two queued phase_ended rows and kept the notice beside them")

	kinds := append([]string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "step_record", "plan_record",
	}, added...)
	for _, kind := range kinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	// The role-holders publication keeps its one row per issue (20261116165926's partial index),
	// so it is queued for an issue of its own.
	if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('role_holders_publish', 'LEGION-209', '{}', 0, now(), '')`); err != nil {
		t.Fatalf("queue a role_holders_publish row after the migration: %v", err)
	}
	t.Logf("after the migration the outbox takes all %d kinds, conflict_confirm, merge_forward and phase_ended among them", len(kinds)+1)
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}

// 20261119003157 adds pull_requests.conflict_read and pull_requests.merge_forward
// (record.PullRequest.ConflictRead and MergeForward). A pull request written before it was
// recorded by a release that read no conflict twice and asked for no merge forward, so it reads
// back with neither, and what that release wrote reads back as it was written. A pull request
// written after it with both set reads both back as written, and a write of neither, which is
// how the daemon clears them, stores SQL null, which is what the way back in the migration's
// header sets and what a row written before it holds.
func TestAPullRequestKeepsItsConflictReadAndMergeForwardFrom20261119003157(t *testing.T) {
	t.Parallel()
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, conflictReadsVersion-1)
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		if _, err := store.Pool().Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'root', 'accepting', 1, 'in_progress', 'A', 1)`, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved,
		conflicting_head)
		values ('LEGION-208', 'acme/widgets', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0,
		 '[]', 3, 'h4-3', '', false, 0, 'open', 'h4', '[]', '', 'CONFLICTING', 'dirty', 'main', '', 'h4')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	read := func(issue string) *record.PullRequest {
		t.Helper()
		var pr *record.PullRequest
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pr, err = record.NewStore().PullRequest(ctx, tx, issue)
			return err
		}); err != nil || pr == nil {
			t.Fatalf("read the pull request of %s after 20261119003157: %+v, %v", issue, pr, err)
		}
		return pr
	}
	nulls := func(issue string) (bool, bool) {
		t.Helper()
		var conflictRead, mergeForward bool
		if err := store.Pool().QueryRow(ctx, `select conflict_read is null, merge_forward is null from pull_requests where issue = $1`,
			issue).Scan(&conflictRead, &mergeForward); err != nil {
			t.Fatalf("read whether the two columns of %s are null: %v", issue, err)
		}
		return conflictRead, mergeForward
	}

	if conflictRead, mergeForward := nulls("LEGION-208"); !conflictRead || !mergeForward {
		t.Fatalf("conflict_read null = %v and merge_forward null = %v after 20261119003157, want both null for a pull request recorded before it",
			conflictRead, mergeForward)
	}
	before := read("LEGION-208")
	if before.ConflictRead != nil || before.MergeForward != nil {
		t.Fatalf("the pull request recorded before 20261119003157 reads a first conflicting read %+v and an ask %+v, want neither",
			before.ConflictRead, before.MergeForward)
	}
	if before.HeadSHA != "h4" || before.ConflictingHead != "h4" || before.Mergeability != record.MergeabilityConflicting ||
		before.Base != "main" || before.Generation != 3 || before.Snapshot != "h4-3" {
		t.Fatalf("the pull request recorded before 20261119003157 = head %q, conflicting %q, mergeability %q, base %q, generation %d, snapshot %q; "+
			"want what the release before wrote", before.HeadSHA, before.ConflictingHead, before.Mergeability, before.Base, before.Generation, before.Snapshot)
	}

	firstRead := time.Date(2026, 11, 19, 0, 31, 57, 0, time.UTC)
	written := record.PullRequest{Issue: "LEGION-209", Repo: "acme/widgets", Number: 1209, Branch: "legion/LEGION-209",
		Base: "main", HeadSHA: "h7", Failing: []string{}, State: record.PullRequestOpen, ConflictingHead: "h6",
		ConflictRead: &record.ConflictRead{Head: "h7", Base: "main", At: firstRead},
		MergeForward: &record.MergeForward{Head: "h6", Base: "main", Phase: phase.Accepting, Why: "merge-next",
			AskedAt: firstRead.Add(-time.Hour), Landed: "h7", ReviewAsked: true}}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, written)
	}); err != nil {
		t.Fatalf("write a pull request with a first conflicting read and an ask to merge forward: %v", err)
	}
	got := read("LEGION-209")
	if !reflect.DeepEqual(got.ConflictRead, written.ConflictRead) || !reflect.DeepEqual(got.MergeForward, written.MergeForward) {
		t.Fatalf("the pull request read back a first conflicting read %+v and an ask %+v, want %+v and %+v as written",
			got.ConflictRead, got.MergeForward, written.ConflictRead, written.MergeForward)
	}
	t.Logf("written and read back: first read %+v, ask %+v", *got.ConflictRead, *got.MergeForward)

	written.ConflictRead, written.MergeForward = nil, nil
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, written)
	}); err != nil {
		t.Fatalf("write the pull request with neither: %v", err)
	}
	if conflictRead, mergeForward := nulls("LEGION-209"); !conflictRead || !mergeForward {
		t.Fatalf("conflict_read null = %v and merge_forward null = %v after a write of neither, want both null", conflictRead, mergeForward)
	}
	if cleared := read("LEGION-209"); cleared.ConflictRead != nil || cleared.MergeForward != nil {
		t.Fatalf("the pull request written with neither reads a first conflicting read %+v and an ask %+v, want neither",
			cleared.ConflictRead, cleared.MergeForward)
	}
}
