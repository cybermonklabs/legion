package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestsByRepositoryVersion is the version of the migration that keys each pull request by
// its repository and its number, so a test can migrate through everything before it whatever lands
// beside it, and run its way back first in a rollback past an older migration.
const pullRequestsByRepositoryVersion = 20261127234212

// pullRequestsByRepositoryRelease names the release whose way back the header of
// pullRequestsByRepositoryVersion carries, as a failure to read that block names it.
const pullRequestsByRepositoryRelease = "the pull requests by repository and number"

// pullRequestKeys is each key and each index of pull_requests and pull_request_settled_heads, as
// its table, its name and its definition, in that order.
const pullRequestKeys = `select k.conrelid::regclass::text || ' ' || k.conname || ' ' || pg_get_constraintdef(k.oid)
	from pg_constraint k where k.conrelid in ('public.pull_requests'::regclass, 'public.pull_request_settled_heads'::regclass)
	and k.contype in ('p', 'f', 'u')
	union all select i.tablename || ' ' || i.indexname || ' ' || i.indexdef from pg_indexes i
	where i.schemaname = current_schema() and i.tablename in ('pull_requests', 'pull_request_settled_heads')
	order by 1`

// The way back that the header of 20261127234212 carries gives the release before the keys it
// writes and reads by. The database it runs over holds two pull requests of LEGION-208, which has
// no plan rows, on its one branch, legion/LEGION-208, in its repository (#41, merged, and #44,
// open), a pull request of another repository that the issue links (acme/gadgets#7), what each
// remembers of a settled head, a review decision, LEGION-209 held for pull-requests, and the
// outbox's rows this release writes and the release before cannot read: a read of a linked pull
// request, a read of the branches of the issue's tasks, a notice of each of the three new kinds,
// and a merge-queue read that names its repository. It holds two more issues, each with two open
// pull requests on one branch:
//   - LEGION-210, the record of dispatch://INFRA-1276 under neutral names, with no plan rows and
//     the default branch main: #3654 onto main and the draft #3955 onto legion/LEGION-210-ci-base,
//     at one head, #3955 changed later;
//   - LEGION-211, whose task b holds legion/LEGION-211.b with its base recorded as
//     legion/LEGION-211.a (plan_tasks.base): #50 onto main, changed later, and #52 onto
//     legion/LEGION-211.a.
//
// After the migration one index of pull_requests holds the issue alone, the column each read of an
// issue's pull requests filters on. After the way back the keys and the indexes of pull_requests
// and pull_request_settled_heads are the ones a database migrated to the version before reads; of
// two rows on one branch the task's own pull request stays, by the plan's rule (an open row before
// a merged one, then the row on the task's base, then the lowest number), and the other is gone:
// #44 stays over the merged #41, #3654 over the draft #3955, and #52 over #50. The linked one is
// gone too, each with its settled head, and #44 keeps its own, by the issue and the branch again;
// the links, the review decisions and the hold's reason are gone, LEGION-209 stays held from
// merging; the rows of the new kinds and the row with a repository are gone, while a merge-queue
// read of the release before and an older notice stay; the outbox refuses each new kind again; and
// the version row is gone, so the roll-forward's Migrate runs the migration again and the record
// reads #44 by its repository and number.
//
// The faults it catches: a way back that keeps the row of the newest head or the highest number,
// under which LEGION-210 keeps the draft #3955 and LEGION-211 keeps #50; and one that keeps the
// lowest number with no regard for the task's base, under which LEGION-211 keeps #50.
func TestTheWayBackOfThePullRequestsMigrationRestoresTheKeyOfTheBase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	base := emptyStore(t)
	migrateThrough(t, base, pullRequestsByRepositoryVersion-1)
	baseKeys := rowsAsText(t, base, pullRequestKeys)
	if len(baseKeys) == 0 {
		t.Fatal("the database of the version before holds no key of pull_requests, so the way back has nothing to be held to")
	}

	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(sql string, args ...any) error {
		_, err := store.Pool().Exec(ctx, sql, args...)
		return err
	}
	seed := func(sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	keys := rowsAsText(t, store, pullRequestKeys)
	if strings.Join(keys, "\n") == strings.Join(baseKeys, "\n") {
		t.Fatalf("the keys after the migration = %q, the keys of the version before: the migration keyed nothing", keys)
	}
	if !slices.ContainsFunc(keys, func(key string) bool {
		return strings.HasPrefix(key, "pull_requests pull_requests_issue ") && strings.HasSuffix(key, " (issue)")
	}) {
		t.Fatalf("the keys after the migration = %q, want an index of pull_requests on the issue alone: each read of an issue's pull requests filters on it", keys)
	}
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, github_repo)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'the issue', 'reviewing', 1, 'needs_review', 'A', 1, 'acme/widgets', 'Acme/Widgets')`)
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, github_repo,
		held_from, hold_reason)
		values ('LEGION-209', 'LEGION-209', 'LEGION', 'the held issue', 'held', 1, 'in_progress', 'B', 1, 'acme/widgets', 'acme/widgets',
		'merging', 'pull-requests')`)
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, github_repo, default_branch)
		values ('LEGION-210', 'LEGION-210', 'LEGION', 'the replayed issue', 'reviewing', 1, 'needs_review', 'C', 1, 'acme/widgets', 'acme/widgets', 'main'),
		('LEGION-211', 'LEGION-211', 'LEGION', 'the planned issue', 'reviewing', 1, 'needs_review', 'D', 1, 'acme/widgets', 'acme/widgets', 'main')`)
	seed(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-211', 1, 1, 'owner', 'artifact-211', 'legion-legion-legion-211-architect', now())`)
	seed(`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, branch, base)
		values ('LEGION-211', 1, 'a', 1, 'Build the gear.', 'done', 'legion/LEGION-211.a', 'main'),
		('LEGION-211', 1, 'b', 1, 'Build the widget on the gear.', 'running', 'legion/LEGION-211.b', 'legion/LEGION-211.a')`)
	pullRequest := `insert into pull_requests (issue, repo, number, branch, base, state, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted, review_decision)
		values ($1, $2, $3::integer, $4, $5, $6, $7, $8::timestamptz, '[]', 0, 0, '[]', 0, '', '', $9)`
	seed(pullRequest, "LEGION-208", "acme/widgets", 41, "legion/LEGION-208", "", "merged", "h41", "2026-11-26T10:00:00Z", nil)
	seed(pullRequest, "LEGION-208", "acme/widgets", 44, "legion/LEGION-208", "", "open", "h44", "2026-11-26T11:00:00Z", `{"state": "approved", "head": "h44"}`)
	seed(pullRequest, "LEGION-208", "acme/gadgets", 7, "fix-gadgets", "", "open", "h7", "2026-11-26T12:00:00Z", nil)
	seed(pullRequest, "LEGION-210", "acme/widgets", 3654, "legion/LEGION-210", "main", "open", "a95b58b7", "2026-11-16T21:50:00Z", nil)
	seed(pullRequest, "LEGION-210", "acme/widgets", 3955, "legion/LEGION-210", "legion/LEGION-210-ci-base", "open", "a95b58b7",
		"2026-11-16T21:54:48Z", nil)
	seed(pullRequest, "LEGION-211", "acme/widgets", 50, "legion/LEGION-211.b", "main", "open", "h50", "2026-11-26T12:00:00Z", nil)
	seed(pullRequest, "LEGION-211", "acme/widgets", 52, "legion/LEGION-211.b", "legion/LEGION-211.a", "open", "h52", "2026-11-26T11:00:00Z", nil)
	for _, pr := range []struct {
		repo   string
		number int
	}{{"acme/widgets", 41}, {"acme/widgets", 44}, {"acme/gadgets", 7}} {
		seed(`insert into pull_request_settled_heads (repo, number, kind, position, head, fence)
			values ($1, $2::integer, 'pull_request', 0, 'h' || $2::text, '{"checkRuns": []}'::jsonb)`, pr.repo, pr.number)
	}
	seed(`insert into pull_request_links (issue, repo, number) values ('LEGION-208', 'acme/gadgets', 7)`)
	queue := func(kind, payload string) error {
		return exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', $2::jsonb, 0, now(), '')`,
			kind, payload)
	}
	for _, row := range []struct{ kind, payload string }{
		{"pull_request_link_read", `{"repo": "acme/gadgets", "number": 7}`},
		{"pull_request_branch_read", `{"generation": 1, "repo": "acme/gadgets", "number": 7, "branches": ["legion/LEGION-208"]}`},
		{"notice", `{"kind": "pr-closed-others-open", "role": "architect"}`},
		{"notice", `{"kind": "pr-holds-close", "role": "architect"}`},
		{"notice", `{"kind": "pr-link-refused", "role": "architect"}`},
		{"merge_queue_read", `{"repo": "acme/gadgets", "number": 7, "head": "h7"}`},
		{"merge_queue_read", `{"number": 44, "head": "h44"}`},
		{"notice", `{"kind": "pr-merged", "role": "architect"}`},
	} {
		if err := queue(row.kind, row.payload); err != nil {
			t.Fatalf("queue a %s row %s: %v", row.kind, row.payload, err)
		}
	}

	if err := exec(wayBackOf(t, pullRequestsByRepositoryVersion, pullRequestsByRepositoryRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, pullRequestKeys); strings.Join(got, "\n") != strings.Join(baseKeys, "\n") {
		t.Fatalf("the keys after the way back =\n%s\nwant the keys of the version before:\n%s", strings.Join(got, "\n"), strings.Join(baseKeys, "\n"))
	}
	if got, want := rowsAsText(t, store, `select issue || ' ' || repo || '#' || number || ' ' || branch from pull_requests order by 1`), []string{
		"LEGION-208 acme/widgets#44 legion/LEGION-208",
		"LEGION-210 acme/widgets#3654 legion/LEGION-210",
		"LEGION-211 acme/widgets#52 legion/LEGION-211.b",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the pull requests after the way back = %q, want %q: of each branch the task's own, open before merged, then on the task's base, "+
			"then the lowest number, and not the linked one", got, want)
	}
	if got, want := rowsAsText(t, store, `select issue || ' ' || branch || ' ' || kind || ' ' || head from pull_request_settled_heads order by 1`),
		[]string{"LEGION-208 legion/LEGION-208 pull_request h44"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the settled heads after the way back = %q, want %q: #44's own, by its issue and its branch", got, want)
	}
	if tableExists(t, store, "pull_request_links") {
		t.Fatal("pull_request_links stands after the way back, want it dropped")
	}
	if shape := columnShape(t, store, "pull_requests", "review_decision"); shape != "" {
		t.Fatalf("pull_requests.review_decision after the way back = %s, want it dropped", shape)
	}
	if got, want := rowsAsText(t, store, `select coalesce(held_from, '') || ' ' || coalesce(hold_reason, 'none') from issues where key = 'LEGION-209'`),
		[]string{"merging none"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("LEGION-209's hold after the way back = %q, want %q: held from merging, with no reason the release before cannot read", got, want)
	}
	if got, want := rowsAsText(t, store, `select kind || ' ' || payload::text from outbox order by id`), []string{
		`merge_queue_read {"head": "h44", "number": 44}`,
		`notice {"kind": "pr-merged", "role": "architect"}`,
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the outbox after the way back = %q, want %q: the rows the release before reads alone", got, want)
	}
	var pgErr *pgconn.PgError
	for _, row := range []struct{ kind, payload string }{
		{"pull_request_link_read", `{"repo": "acme/gadgets", "number": 7}`},
		{"pull_request_branch_read", `{"generation": 1, "repo": "acme/gadgets", "number": 7, "branches": ["legion/LEGION-208"]}`},
	} {
		if err := queue(row.kind, row.payload); !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "outbox_kind_check" {
			t.Fatalf("queueing a %s row after the way back = %v, want the kind constraint to refuse it (23514)", row.kind, err)
		}
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261127234212`); got[0] != "0" {
		t.Fatalf("schema_version rows of %d after the way back = %s, want none", pullRequestsByRepositoryVersion, got[0])
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	var pr *record.PullRequest
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequestByNumber(ctx, tx, "Acme/Widgets", 44)
		return err
	}); err != nil {
		t.Fatalf("read acme/widgets#44 after the roll-forward: %v", err)
	}
	if pr == nil || pr.Issue != "LEGION-208" || pr.Branch != "legion/LEGION-208" || len(pr.SettledHeads) != 1 {
		t.Fatalf("acme/widgets#44 after the roll-forward = %+v, want LEGION-208's row on legion/LEGION-208 with its settled head", pr)
	}
}
