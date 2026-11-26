package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An adoption moves a member of a closed tree into a live one. The move queues the old tree's
// suspend of every stored claim of the member at its old generation, one row naming each claim
// (everyClaim), whatever tree or state it holds, and one row naming no claim for each role whose
// step's claim it named none of, which reaches a claim that comes up after the move. The outbox
// decides whether each claim still stands in that tree when its row runs.
func TestAnAdoptionQueuesTheOldTreesStopOfEachClaimItLeavesThere(t *testing.T) {
	t.Parallel()
	const closedRoot, member, liveRoot = "LEGION-207", "LEGION-208", "LEGION-300"
	pool := migratedPool(t)
	now := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)
	parent := closedRoot
	seedIssue(t, pool, record.Issue{Key: closedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: closedRoot,
		Project: "LEGION", Title: "closed root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "S", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: member, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: closedRoot,
		Parent: &parent, Project: "LEGION", Title: "member", Phase: phase.Done, Generation: 3, Status: "done", Rank: "U", LastDispatchSeq: 1})
	root := liveRootOf(t, pool, liveRoot, 0)
	for _, seeded := range []struct {
		role        claim.Role
		tree, state string
	}{
		{claim.RoleTester, closedRoot, "working"},
		{claim.RoleArchitect, closedRoot, "suspended"},
		{claim.RoleImplementer, closedRoot, "retired"},
		{claim.RoleReviewer, "LEGION-999", "ready"},
	} {
		token, err := claim.NewToken("legion", member, seeded.role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $3, $4, 3, 'ses', '/sessions/ses.jsonl', $5, 0, 0, 0, 0)`,
			string(token), seeded.tree, member, string(seeded.role), seeded.state); err != nil {
			t.Fatalf("seed the %s claim of %s: %v", seeded.role, member, err)
		}
	}

	stored := issueRecord(t, pool, member)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.AdoptMember(ctx, tx, *stored, root, intake.AdoptedChild{Issue: member, Parent: liveRoot, Status: "done"})
	})
	if got := issueRecord(t, pool, member); got.Tree != liveRoot || got.Generation != 4 {
		t.Fatalf("the adopted member stands in tree %s at generation %d, want %s at generation 4", got.Tree, got.Generation, liveRoot)
	}

	rows := issueOutboxOf(t, pool, member, "supervise")
	got := make([]record.SuperviseRequest, 0, len(rows))
	for _, row := range rows {
		got = append(got, decodeSupervise(t, row))
	}
	reason := member + " left tree " + closedRoot
	named := func(role claim.Role) claim.Token {
		token, err := claim.NewToken("legion", member, role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	want := []record.SuperviseRequest{
		{Op: "suspend", Tree: closedRoot, Role: claim.RoleArchitect, Claim: named(claim.RoleArchitect), Generation: 3, Reason: reason},
		{Op: "suspend", Tree: closedRoot, Role: claim.RoleImplementer, Claim: named(claim.RoleImplementer), Generation: 3, Reason: reason},
		{Op: "suspend", Tree: closedRoot, Role: claim.RoleReviewer, Claim: named(claim.RoleReviewer), Generation: 3, Reason: reason},
		{Op: "suspend", Tree: closedRoot, Role: claim.RoleTester, Claim: named(claim.RoleTester), Generation: 3, Reason: reason},
		{Op: "suspend", Tree: closedRoot, Role: claim.RolePlanner, Generation: 3, Reason: reason},
		{Op: "suspend", Tree: closedRoot, Role: claim.RoleMerger, Generation: 3, Reason: reason},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the adoption queued supervise rows %+v for %s; want the old tree's suspend of every stored claim, naming it, the reviewer already in another tree included: %+v",
			got, member, want)
	}
}
