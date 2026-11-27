package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestsByRepositoryVersion is the version of the migration that keys each pull request by
// its repository and its number, so a test can migrate through everything before it whatever lands
// beside it, and run its way back first in a rollback past an older migration.
const pullRequestsByRepositoryVersion = 20261127031536

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

// The way back that the header of 20261127031536 carries gives the release before the keys it
// writes and reads by. The database it runs over holds two pull requests of LEGION-208 on its one
// branch, legion/LEGION-208, in its repository (#41, the older, and #44), a pull request of
// another repository that the issue links (acme/gadgets#7), what each remembers of a settled head,
// a review decision, LEGION-209 held for pull-requests, and the outbox's rows this release writes
// and the release before cannot read: a read of a linked pull request, a notice of each of the
// three new kinds, and a merge-queue read that names its repository. After it the keys and the
// indexes of pull_requests and pull_request_settled_heads are the ones a database migrated to the
// version before reads; of the two on one branch the older is gone, and so is the linked one, each
// with its settled head, and the newer keeps its own, by the issue and the branch again; the links,
// the review decisions and the hold's reason are gone, LEGION-209 stays held from merging; the
// rows of the new kinds and the row with a repository are gone, while a merge-queue read of the
// release before and an older notice stay; the outbox refuses the new kind again; and the version
// row is gone, so the roll-forward's Migrate runs the migration again and the record reads #44 by
// its repository and number.
func TestTheWayBackOfThePullRequestsMigrationRestoresTheKeyOfTheBase(t *testing.T) {
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
	if got := rowsAsText(t, store, pullRequestKeys); strings.Join(got, "\n") == strings.Join(baseKeys, "\n") {
		t.Fatalf("the keys after the migration = %q, the keys of the version before: the migration keyed nothing", got)
	}
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, github_repo)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'the issue', 'reviewing', 1, 'needs_review', 'A', 1, 'acme/widgets', 'Acme/Widgets')`)
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, github_repo,
		held_from, hold_reason)
		values ('LEGION-209', 'LEGION-209', 'LEGION', 'the held issue', 'held', 1, 'in_progress', 'B', 1, 'acme/widgets', 'acme/widgets',
		'merging', 'pull-requests')`)
	pullRequest := `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted, review_decision)
		values ('LEGION-208', $1, $2::integer, $3, 'h' || $2::text, $4::timestamptz, '[]', 0, 0, '[]', 0, '', '', $5)`
	seed(pullRequest, "acme/widgets", 41, "legion/LEGION-208", "2026-11-26T10:00:00Z", nil)
	seed(pullRequest, "acme/widgets", 44, "legion/LEGION-208", "2026-11-26T11:00:00Z", `{"state": "approved", "head": "h44"}`)
	seed(pullRequest, "acme/gadgets", 7, "fix-gadgets", "2026-11-26T12:00:00Z", nil)
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
	if got, want := rowsAsText(t, store, `select issue || ' ' || repo || '#' || number || ' ' || branch from pull_requests order by 1`),
		[]string{"LEGION-208 acme/widgets#44 legion/LEGION-208"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the pull requests after the way back = %q, want %q: the newer of the branch, and not the older nor the linked one", got, want)
	}
	if got, want := rowsAsText(t, store, `select issue || ' ' || branch || ' ' || kind || ' ' || head from pull_request_settled_heads order by 1`),
		[]string{"LEGION-208 legion/LEGION-208 pull_request h44"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the settled heads after the way back = %q, want %q: the newer's own, by its issue and its branch", got, want)
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
	if err := queue("pull_request_link_read", `{"repo": "acme/gadgets", "number": 7}`); !errors.As(err, &pgErr) || pgErr.Code != "23514" ||
		pgErr.ConstraintName != "outbox_kind_check" {
		t.Fatalf("queueing a pull_request_link_read row after the way back = %v, want the kind constraint to refuse it (23514)", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261127031536`); got[0] != "0" {
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
