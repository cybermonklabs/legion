package refs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// issueRepoCap is the depth the walk has to stop at, held here as the number rather than read
// from IssueDepthCap: a chain measured by the constant would move with it, and the walk could
// reach any depth with these tests still passing. It is the same 32-link rule the API's parent
// cap refuses past and migration 0113's census counts against.
const issueRepoCap = 32

// issueRepoChainDefault is the default repository of the chain's project. No issue in the chain
// names a repository of its own, so it is the answer for every issue whose walk finishes, and the
// wrong answer for every issue whose walk does not.
const issueRepoChainDefault = "acme/widgets"

// newIssueRepoChain writes a project whose default repository is issueRepoChainDefault and a
// chain of length issues under it, CORE-1 the root and CORE-n n-1 links below it, and returns
// the database holding them. The rows go in as SQL because no API write builds a chain this deep
// (api's refuseParentTooDeep) and migration 0113 refuses to apply while one exists: such a chain
// is left by a direct database write, or by an earlier release serving against the rollout rule,
// and that is what the walk has to survive.
func newIssueRepoChain(t *testing.T, length int) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the chain fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	if _, err := tx.Exec(ctx, `update projects set default_repo = $1 where key = 'CORE'`, issueRepoChainDefault); err != nil {
		t.Fatalf("set %s as the project's default: %v", issueRepoChainDefault, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select 'CORE-' || n, 'CORE', n, 'Chain ' || n,
		       case when n = 1 then null else 'CORE-' || (n - 1) end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, $1::int) n
		order by n
	`, length); err != nil {
		t.Fatalf("write a chain of %d issues: %v", length, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the chain fixture: %v", err)
	}
	return database
}

// repoName renders a resolved repository in a failure message.
func repoName(repo *string) string {
	if repo == nil {
		return "none"
	}
	return *repo
}

// The walk IssueRepository runs climbs at most issueRepoCap links, so an issue deeper than that
// has ancestors it never reads and no repository it can name. The read refuses it rather than
// answering with the project's default, which the unread ancestors may well override. The issue
// exactly issueRepoCap links below the root is still answered, so the refusal is the cap's own
// boundary and not a fixture that fails for every issue.
func TestIssueRepositoryRefusesAChainPastTheDepthCap(t *testing.T) {
	t.Parallel()
	database := newIssueRepoChain(t, issueRepoCap+2)
	ctx := context.Background()

	atCap := fmt.Sprintf("CORE-%d", issueRepoCap+1)
	resolved, err := IssueRepository(ctx, database.Pool, atCap)
	if err != nil {
		t.Fatalf("resolve %s, %d links below its root: %v", atCap, issueRepoCap, err)
	}
	if resolved == nil || *resolved != issueRepoChainDefault {
		t.Fatalf("%s, %d links below its root, resolved to %s, want the project's default %s",
			atCap, issueRepoCap, repoName(resolved), issueRepoChainDefault)
	}

	pastCap := fmt.Sprintf("CORE-%d", issueRepoCap+2)
	resolved, err = IssueRepository(ctx, database.Pool, pastCap)
	if err == nil {
		t.Fatalf("%s, %d links below its root, resolved to %s, want the read refused",
			pastCap, issueRepoCap+1, repoName(resolved))
	}
	if resolved != nil {
		t.Fatalf("the refusal of %s also answered %s, want no repository with it", pastCap, repoName(resolved))
	}
}

// A parent chain that loops has no root and so no repository to resolve to. The walk runs out of
// links as it does on a chain past the cap, and the read refuses it for the same reason rather
// than answering with the project's default. The same issue resolves before the loop is closed,
// so the refusal is the loop's.
func TestIssueRepositoryRefusesALoopedParentChain(t *testing.T) {
	t.Parallel()
	database := newIssueRepoChain(t, 2)
	ctx := context.Background()

	resolved, err := IssueRepository(ctx, database.Pool, "CORE-2")
	if err != nil {
		t.Fatalf("resolve CORE-2 under an unlooped parent: %v", err)
	}
	if resolved == nil || *resolved != issueRepoChainDefault {
		t.Fatalf("CORE-2 under an unlooped parent resolved to %s, want the project's default %s",
			repoName(resolved), issueRepoChainDefault)
	}

	// CORE-2's parent is CORE-1; making CORE-1's parent CORE-2 closes the loop, which no API
	// write can do.
	if _, err := database.Pool.Exec(ctx, `update issues set parent_key = 'CORE-2' where key = 'CORE-1'`); err != nil {
		t.Fatalf("loop the chain: %v", err)
	}
	for _, key := range []string{"CORE-1", "CORE-2"} {
		resolved, err := IssueRepository(ctx, database.Pool, key)
		if err == nil {
			t.Errorf("%s in a looped chain resolved to %s, want the read refused", key, repoName(resolved))
		} else if resolved != nil {
			t.Errorf("the refusal of %s also answered %s, want no repository with it", key, repoName(resolved))
		}
	}
}

// mainIssueRepoLateral is main's own SQL, held here letter for letter as the oracle of the
// comparison below, but for the one read this change moved: the project's default, which main
// read from the repo_projects row marked is_default, and which is now the project's own
// default_repo column (migration 20261119000230 copied each such row there). It is a copy of
// that text, not text built from the package's constants: a lateral built from those would move
// with them, and the two sides would agree however the resolution rule changed. IssueDepthCap
// is written as the 32 main spliced into it for the same reason.
const mainIssueRepoLateral = `
	left join lateral (
		with recursive chain as (
			select i.parent_key, i.repo, 0 as depth
			union all
			select p.parent_key, p.repo, c.depth + 1
			from issues p join chain c on p.key = c.parent_key
			where c.repo is null and c.depth < 32
		)
		select
			coalesce(
				(select c.repo from chain c where c.repo is not null order by c.depth limit 1),
				(select d.default_repo from projects d where d.key = i.project_key)
			) as resolved,
			exists (
				select 1 from chain c
				where c.depth = 32 and c.repo is null and c.parent_key is not null
			) as too_deep
	) repo on true`

// issueRepoOwnRepo is the repository CORE-100 names as its own: not the default of its project,
// so an issue resolving to it resolved through the chain and not through the default.
// issueRepoLoopDefault is project LOOP's own default, which its looped issues must not fall back
// to.
const (
	issueRepoOwnRepo     = "acme/gadgets"
	issueRepoLoopDefault = "acme/loops"
)

// newIssueRepoGround is one database holding every case this rule answers differently:
// project CORE's chain of issueRepoCap+2 issues, whose deepest issue the walk cannot finish and
// whose issue at the cap it can; CORE-100, which names a repository of its own, and CORE-101
// below it, which inherits that one; project LOOP, whose two issues are each other's parent
// though the project has a default the walk must not fall back to; and project BARE, which
// has no default at all.
func newIssueRepoGround(t *testing.T) *store.Store {
	t.Helper()
	database := newIssueRepoChain(t, issueRepoCap+2)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the ground fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "LOOP")
	createReferenceProject(t, tx, "BARE")
	if _, err := tx.Exec(ctx, `update projects set default_repo = $1 where key = 'LOOP'`, issueRepoLoopDefault); err != nil {
		t.Fatalf("set LOOP's default: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, repo, created_by, rank) values
			('CORE-100', 'CORE', 100, 'Owns a repository', null, $1, '{"kind":"user","id":"alice"}', '00000100'),
			('CORE-101', 'CORE', 101, 'Inherits from above', 'CORE-100', null, '{"kind":"user","id":"alice"}', '00000101'),
			('LOOP-1', 'LOOP', 1, 'Loop head', null, null, '{"kind":"user","id":"alice"}', '00000001'),
			('LOOP-2', 'LOOP', 2, 'Loop tail', 'LOOP-1', null, '{"kind":"user","id":"alice"}', '00000002'),
			('BARE-1', 'BARE', 1, 'No default above', null, null, '{"kind":"user","id":"alice"}', '00000001'),
			('BARE-2', 'BARE', 2, 'No default below', 'BARE-1', null, '{"kind":"user","id":"alice"}', '00000002')
	`, issueRepoOwnRepo); err != nil {
		t.Fatalf("write the ground's issues: %v", err)
	}
	// Closing the loop is a write no API makes, so it goes in after both rows exist.
	if _, err := tx.Exec(ctx, `update issues set parent_key = 'LOOP-2' where key = 'LOOP-1'`); err != nil {
		t.Fatalf("loop LOOP's chain: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the ground fixture: %v", err)
	}
	return database
}

// issueRepoMainAnswer is what main's lateral resolved for the key, rendered as
// issueRepoSetAnswer renders IssueRepository's answer: "refused" for a chain the walk could not
// finish, the repository otherwise, "none" for no repository and for a key no issue has.
func issueRepoMainAnswer(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var resolved *string
	var tooDeep bool
	err := database.Pool.QueryRow(context.Background(), `
		select repo.resolved, repo.too_deep
		from issues i`+mainIssueRepoLateral+`
		where i.key = $1
	`, key).Scan(&resolved, &tooDeep)
	if errors.Is(err, pgx.ErrNoRows) {
		return "none"
	}
	if err != nil {
		t.Fatalf("read main's lateral for %q: %v", key, err)
	}
	if tooDeep {
		return "refused"
	}
	return repoName(resolved)
}

// issueRepoSetAnswer is what IssueRepository resolves for the key, rendered the same way. An
// error that does not name the parent chain fails the test rather than reading as a refusal: a
// statement broken in any other way would otherwise pass for every key whose chain has no
// answer.
func issueRepoSetAnswer(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	resolved, err := IssueRepository(context.Background(), database.Pool, key)
	if err != nil {
		if !strings.Contains(err.Error(), "its parent chain is deeper than") {
			t.Fatalf("resolve %q: %v", key, err)
		}
		if resolved != nil {
			t.Errorf("the refusal of %q also answered %s, want no repository with it", key, repoName(resolved))
		}
		return "refused"
	}
	return repoName(resolved)
}

// IssueRepository runs the terms the issue reads run, for one key, so it answers key for key
// what main's lateral walk of that one chain answered: over a loop, a chain at the cap, a chain
// past it, an issue that names its own repository, an issue inheriting one from above, a
// project that has no default, and a key no issue has. The spread check below says the ground
// still holds all four answers, so an agreement on "none" everywhere cannot pass for one.
func TestIssueRepositoryAnswersWhatMainsLateralDid(t *testing.T) {
	t.Parallel()
	database := newIssueRepoGround(t)
	seen := map[string]bool{}
	for _, ground := range []struct{ key, is string }{
		{"CORE-1", "the chain's root, which takes its project's default"},
		{fmt.Sprintf("CORE-%d", issueRepoCap+1), "the issue exactly at the depth cap"},
		{fmt.Sprintf("CORE-%d", issueRepoCap+2), "the issue one link past the cap"},
		{"CORE-100", "an issue naming a repository of its own"},
		{"CORE-101", "an issue inheriting the repository above it"},
		{"LOOP-1", "an issue in a parent loop"},
		{"LOOP-2", "the other issue in that loop"},
		{"BARE-1", "an issue of a project that has no default"},
		{"BARE-2", "an issue below it"},
		{"CORE-9999", "a key no issue has"},
		{"", "the empty key"},
	} {
		want := issueRepoMainAnswer(t, database, ground.key)
		seen[want] = true
		if got := issueRepoSetAnswer(t, database, ground.key); got != want {
			t.Errorf("%q, %s: the terms answered %s, main's lateral %s", ground.key, ground.is, got, want)
		}
	}
	for _, answer := range []string{"refused", "none", issueRepoChainDefault, issueRepoOwnRepo} {
		if !seen[answer] {
			t.Errorf("no issue of the ground resolved to %s, so the comparison never covered it", answer)
		}
	}
}

// An issue's repository is read only from ancestors of its own project: the walk ends at its
// first ancestor of another project as at a root, and the issue takes its own project's default.
// BETA-1 sits under ALPHA-1, which names acme/alpha, and takes BETA's default acme/beta. BETA-3's
// chain leaves BETA and comes back, BETA-3 under ALPHA-2 under BETA-4, and it takes nothing from
// BETA-4's acme/beta4 either; ALPHA-2 takes nothing from BETA-4, and resolves to ALPHA's default
// alone. ALPHA-5 under ALPHA-1 and BETA-6 under BETA-5 still inherit, which is the control.
func TestIssueRepositoryStopsAtTheProjectEdge(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ALPHA")
	createReferenceProject(t, tx, "BETA")
	for project, repo := range map[string]string{"ALPHA": "acme/alpha-default", "BETA": "acme/beta"} {
		if _, err := tx.Exec(ctx, `update projects set default_repo = $1 where key = $2`, repo, project); err != nil {
			t.Fatalf("set %s's default repository: %v", project, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, repo, created_by, rank) values
			('ALPHA-1', 'ALPHA', 1, 'Names a repository', null, 'acme/alpha', '{"kind":"user","id":"alice"}', '00000001'),
			('ALPHA-5', 'ALPHA', 5, 'Under its own project', 'ALPHA-1', null, '{"kind":"user","id":"alice"}', '00000005'),
			('BETA-1', 'BETA', 1, 'Under another project', 'ALPHA-1', null, '{"kind":"user","id":"alice"}', '00000001'),
			('BETA-4', 'BETA', 4, 'Names a repository', null, 'acme/beta4', '{"kind":"user","id":"alice"}', '00000004'),
			('ALPHA-2', 'ALPHA', 2, 'Between two of BETA', 'BETA-4', null, '{"kind":"user","id":"alice"}', '00000002'),
			('BETA-3', 'BETA', 3, 'Back in its project', 'ALPHA-2', null, '{"kind":"user","id":"alice"}', '00000003'),
			('BETA-5', 'BETA', 5, 'Names a repository', null, 'acme/beta5', '{"kind":"user","id":"alice"}', '00000005'),
			('BETA-6', 'BETA', 6, 'Under its own project', 'BETA-5', null, '{"kind":"user","id":"alice"}', '00000006')
	`); err != nil {
		t.Fatalf("write the issues: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}

	for _, want := range []struct{ key, repo, is string }{
		{"BETA-1", "acme/beta", "an issue under an issue of another project"},
		{"BETA-3", "acme/beta", "an issue whose chain leaves its project and comes back"},
		{"ALPHA-2", "acme/alpha-default", "an issue under an issue of another project that names one"},
		{"ALPHA-5", "acme/alpha", "an issue under its own project's issue"},
		{"BETA-6", "acme/beta5", "another issue under its own project's issue"},
	} {
		resolved, err := IssueRepository(ctx, database.Pool, want.key)
		if err != nil {
			t.Fatalf("resolve %s: %v", want.key, err)
		}
		if resolved == nil || *resolved != want.repo {
			t.Errorf("%s, %s, resolved to %s, want %s", want.key, want.is, repoName(resolved), want.repo)
		}
	}
}

// issuePriorityStatement reads what IssuePriorityTerms resolve for every issue of the keys in $1,
// as an issue read splices them: a subjects term of those keys, the terms, the join and the
// columns.
var issuePriorityStatement = `
	with recursive subjects as (select unnest($1::text[]) as key), ` + IssuePriorityTerms + `
	select i.key, ` + IssuePriorityColumns + `
	from issues i ` + IssuePriorityJoin + `
	where i.key = any($1)`

// resolvedPriorities is what IssuePriorityTerms resolve for each of keys, rendered as a failure
// line prints it: "P<n>, its own", "P<n> from <key>", or "none". A key no issue has gets no
// entry.
func resolvedPriorities(t *testing.T, database *store.Store, keys ...string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), issuePriorityStatement, keys)
	if err != nil {
		t.Fatalf("resolve the priorities of %v: %v", keys, err)
	}
	defer rows.Close()
	answers := map[string]string{}
	for rows.Next() {
		var key string
		var priority *int
		var source *string
		if err := rows.Scan(&key, &priority, &source); err != nil {
			t.Fatalf("scan a resolved priority: %v", err)
		}
		switch {
		case priority == nil && source != nil:
			t.Errorf("%s resolves no priority but names %s as its source, want neither", key, *source)
			answers[key] = "none"
		case priority == nil:
			answers[key] = "none"
		case source == nil:
			answers[key] = fmt.Sprintf("P%d, its own", *priority)
		default:
			answers[key] = fmt.Sprintf("P%d from %s", *priority, *source)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("resolve the priorities of %v: %v", keys, err)
	}
	return answers
}

// An issue with no priority of its own takes the priority of the nearest issue above it that has
// one, in any project, and names it; an issue with one takes its own and names none. ALPHA-1, at
// P0, passes P0 down two links to ALPHA-3 and on into BETA-1 of another project; ALPHA-4, under
// ALPHA-1 at its own P2, passes P2 to ALPHA-5 below it rather than the root's P0; ALPHA-6 takes
// P1 from BETA-2 of another project above it; and ALPHA-7's chain, on which no issue sets a
// priority, resolves none all the way down. A walk of the root's priority alone answers ALPHA-5
// P0, and a walk that stops at the project's edge, as the repository's does, answers BETA-1 and
// ALPHA-6 none: each fails its own line.
func TestIssuePriorityTermsTakeTheNearestOwnPriorityInAnyProject(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ALPHA")
	createReferenceProject(t, tx, "BETA")
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, priority, created_by, rank) values
			('ALPHA-1', 'ALPHA', 1, 'A root at P0', null, 0, '{"kind":"user","id":"alice"}', '00000001'),
			('ALPHA-2', 'ALPHA', 2, 'Its child', 'ALPHA-1', null, '{"kind":"user","id":"alice"}', '00000002'),
			('ALPHA-3', 'ALPHA', 3, 'Its grandchild', 'ALPHA-2', null, '{"kind":"user","id":"alice"}', '00000003'),
			('ALPHA-4', 'ALPHA', 4, 'A middle at P2', 'ALPHA-1', 2, '{"kind":"user","id":"alice"}', '00000004'),
			('ALPHA-5', 'ALPHA', 5, 'Under the middle', 'ALPHA-4', null, '{"kind":"user","id":"alice"}', '00000005'),
			('BETA-1', 'BETA', 1, 'Under the grandchild, in another project', 'ALPHA-3', null, '{"kind":"user","id":"alice"}', '00000001'),
			('BETA-2', 'BETA', 2, 'A root at P1', null, 1, '{"kind":"user","id":"alice"}', '00000002'),
			('ALPHA-6', 'ALPHA', 6, 'Under a root of another project', 'BETA-2', null, '{"kind":"user","id":"alice"}', '00000006'),
			('ALPHA-7', 'ALPHA', 7, 'A root with none', null, null, '{"kind":"user","id":"alice"}', '00000007'),
			('ALPHA-8', 'ALPHA', 8, 'Its child', 'ALPHA-7', null, '{"kind":"user","id":"alice"}', '00000008'),
			('ALPHA-9', 'ALPHA', 9, 'Its grandchild', 'ALPHA-8', null, '{"kind":"user","id":"alice"}', '00000009')
	`); err != nil {
		t.Fatalf("write the issues: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}

	wants := []struct{ key, resolves, is string }{
		{"ALPHA-1", "P0, its own", "a root at its own P0"},
		{"ALPHA-2", "P0 from ALPHA-1", "a child with none under it"},
		{"ALPHA-3", "P0 from ALPHA-1", "a grandchild two links below it"},
		{"ALPHA-4", "P2, its own", "a middle at its own P2 under a P0 root"},
		{"ALPHA-5", "P2 from ALPHA-4", "an issue under that middle, which the nearest priority decides"},
		{"BETA-1", "P0 from ALPHA-1", "an issue of another project under the grandchild"},
		{"ALPHA-6", "P1 from BETA-2", "an issue under a root of another project"},
		{"ALPHA-7", "none", "a root with no priority"},
		{"ALPHA-8", "none", "its child, on a chain that sets none"},
		{"ALPHA-9", "none", "its grandchild, on a chain that sets none"},
	}
	keys := make([]string, 0, len(wants))
	for _, want := range wants {
		keys = append(keys, want.key)
	}
	resolved := resolvedPriorities(t, database, keys...)
	for _, want := range wants {
		if got := resolved[want.key]; got != want.resolves {
			t.Errorf("%s, %s, resolves %q, want %q", want.key, want.is, got, want.resolves)
		}
	}
}

// The walk IssuePriorityTerms run climbs at most issueRepoCap links, as every walk of the chain
// does: on a chain whose one priority is its root's P0, the issue exactly issueRepoCap links below
// the root takes it, and the issue one link deeper reads none, answered rather than refused as
// the repository walk refuses it, since no issue it can read sets a priority.
func TestIssuePriorityTermsReadNonePastTheDepthCap(t *testing.T) {
	t.Parallel()
	database := newIssueRepoChain(t, issueRepoCap+2)
	if _, err := database.Pool.Exec(context.Background(), `update issues set priority = 0 where key = 'CORE-1'`); err != nil {
		t.Fatalf("set the chain's root at P0: %v", err)
	}
	atCap, pastCap := fmt.Sprintf("CORE-%d", issueRepoCap+1), fmt.Sprintf("CORE-%d", issueRepoCap+2)
	resolved := resolvedPriorities(t, database, atCap, pastCap)
	if got := resolved[atCap]; got != "P0 from CORE-1" {
		t.Errorf("%s, %d links below the P0 root, resolves %q, want P0 from CORE-1", atCap, issueRepoCap, got)
	}
	if got := resolved[pastCap]; got != "none" {
		t.Errorf("%s, %d links below the P0 root, resolves %q, want none", pastCap, issueRepoCap+1, got)
	}
}

// descendantText renders an IssueDescendant as a failure line prints it.
func descendantText(row IssueDescendant) string {
	return fmt.Sprintf("%s under %s, %s, depth %d, in %s", row.Key, row.Parent, row.Status, row.Depth, row.Project)
}

// IssueInheritingDescendantRows descends from an issue through the children with no priority of
// their own, at any status and into any project, and stops below an issue that has one, leaving
// that issue out too. Under ALPHA-1: ALPHA-2, its done child ALPHA-3, BETA-1 of another project
// under ALPHA-2 and BETA-2 under that are in; ALPHA-4, with its own P2, ALPHA-5 under it, and
// ALPHA-6, with its own P0 under BETA-1, are not. The rows come nearest first, by project and
// number at one depth, each with its parent, status, depth and project. A descent that passed
// below an own priority lists ALPHA-5, and one that kept the issue with its own lists ALPHA-4 and
// ALPHA-6. On a chain of issueRepoCap+1 issues below its root, none with a priority, the descent
// reaches the issue issueRepoCap links down and no deeper. A leaf and the empty key answer none.
func TestIssueInheritingDescendantRowsStopBelowAnOwnPriority(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ALPHA")
	createReferenceProject(t, tx, "BETA")
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, priority, status, closed_at, created_by, rank) values
			('ALPHA-1', 'ALPHA', 1, 'A root at P1', null, 1, 'todo', null, '{"kind":"user","id":"alice"}', '00000001'),
			('ALPHA-2', 'ALPHA', 2, 'Its child', 'ALPHA-1', null, 'todo', null, '{"kind":"user","id":"alice"}', '00000002'),
			('ALPHA-3', 'ALPHA', 3, 'A done grandchild', 'ALPHA-2', null, 'done', now(), '{"kind":"user","id":"alice"}', '00000003'),
			('ALPHA-4', 'ALPHA', 4, 'A child at its own P2', 'ALPHA-1', 2, 'todo', null, '{"kind":"user","id":"alice"}', '00000004'),
			('ALPHA-5', 'ALPHA', 5, 'Under that child', 'ALPHA-4', null, 'todo', null, '{"kind":"user","id":"alice"}', '00000005'),
			('BETA-1', 'BETA', 1, 'A grandchild in another project', 'ALPHA-2', null, 'todo', null, '{"kind":"user","id":"alice"}', '00000001'),
			('BETA-2', 'BETA', 2, 'Under it', 'BETA-1', null, 'todo', null, '{"kind":"user","id":"alice"}', '00000002'),
			('ALPHA-6', 'ALPHA', 6, 'Under it at its own P0', 'BETA-1', 0, 'todo', null, '{"kind":"user","id":"alice"}', '00000006')
	`); err != nil {
		t.Fatalf("write the issues: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}

	rows, err := IssueInheritingDescendantRows(ctx, database.Pool, "ALPHA-1")
	if err != nil {
		t.Fatalf("read the descendants of ALPHA-1 that take its priority: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, descendantText(row))
	}
	want := []string{
		"ALPHA-2 under ALPHA-1, todo, depth 1, in ALPHA",
		"ALPHA-3 under ALPHA-2, done, depth 2, in ALPHA",
		"BETA-1 under ALPHA-2, todo, depth 2, in BETA",
		"BETA-2 under BETA-1, todo, depth 3, in BETA",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the descendants of ALPHA-1 that take its priority are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, key := range []string{"BETA-2", ""} {
		rows, err := IssueInheritingDescendantRows(ctx, database.Pool, key)
		if err != nil {
			t.Fatalf("read the descendants of %q that take its priority: %v", key, err)
		}
		if rows == nil || len(rows) != 0 {
			t.Errorf("the descendants of %q that take its priority are %v, want []", key, rows)
		}
	}

	t.Run(fmt.Sprintf("a chain of %d links below its root", issueRepoCap+1), func(t *testing.T) {
		t.Parallel()
		chain := newIssueRepoChain(t, issueRepoCap+2)
		rows, err := IssueInheritingDescendantRows(context.Background(), chain.Pool, "CORE-1")
		if err != nil {
			t.Fatalf("read the descendants of CORE-1 that take its priority: %v", err)
		}
		deepest := fmt.Sprintf("CORE-%d", issueRepoCap+1)
		if len(rows) != issueRepoCap || rows[len(rows)-1].Key != deepest {
			last := "none"
			if len(rows) > 0 {
				last = rows[len(rows)-1].Key
			}
			t.Errorf("the descent from CORE-1 reads %d issues ending at %s, want %d ending at %s, %d links down", len(rows), last, issueRepoCap, deepest, issueRepoCap)
		}
	})
}
