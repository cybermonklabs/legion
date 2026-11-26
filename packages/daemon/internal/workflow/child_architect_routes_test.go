package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The CI message of a child goes to the architect that owns the child, as its phase's notices do
// (owningArchitect): the child's own architect while its claim holds its role, and once it has
// failed, or while the child has none, the nearest architect above it whose claim holds its role
// (here the tree root's). The child's phase worker and its implementer are told as before, and the
// message is queued on the child either way. A child in accepting with no plan rows, whose
// acceptance is its architect's own, tells that architect once.
func TestASettlementOfAChildTellsTheArchitectThatOwnsIt(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-208", "LEGION-209"
	token := func(issue string, role claim.Role) string {
		t.Helper()
		token, err := claim.NewToken("legion", issue, role)
		if err != nil {
			t.Fatalf("role token of %s's %s: %v", issue, role, err)
		}
		return string(token)
	}
	architectIn := func(t *testing.T, pool *pgxpool.Pool, issue string, state supervise.ClaimState) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $3, 'architect', 1, 'ses', '/sessions/ses.jsonl', $4, 0, 0, 0, 0)`,
			token(issue, claim.RoleArchitect), root, issue, string(state)); err != nil {
			t.Fatalf("seed the architect claim of %s: %v", issue, err)
		}
	}
	for _, tc := range []struct {
		name   string
		state  supervise.ClaimState
		at     phase.Phase
		status string
		// owner is the issue whose architect is told; roles are the child's roles told after it.
		owner string
		roles []claim.Role
	}{
		{"its architect working", supervise.StateWorking, phase.Testing, "testing", child, []claim.Role{claim.RoleTester, claim.RoleImplementer}},
		{"its architect working, in accepting", supervise.StateWorking, phase.Accepting, "needs_review", child, []claim.Role{claim.RoleImplementer}},
		{"its architect failed", supervise.StateFailed, phase.Testing, "testing", root, []claim.Role{claim.RoleTester, claim.RoleImplementer}},
		{"its architect never started", "", phase.Testing, "testing", root, []claim.Role{claim.RoleTester, claim.RoleImplementer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root, Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root, Parent: new(root), Project: "LEGION", Title: "child",
				Phase: tc.at, Generation: 1, Status: tc.status, Rank: "V"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child, Repo: "sjawhar/legion", Number: 42, Branch: "legion/" + child, HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "implement-claim"})
			if role := RoleFor(tc.at); role != "" && role != claim.RoleImplementer {
				seedPhase(t, pool, record.PhaseRow{Issue: child, Role: role, Claim: claim.Token(string(role) + "-claim")})
			}
			architectIn(t, pool, root, supervise.StateWorking)
			if tc.state != "" {
				architectIn(t, pool, child, tc.state)
			}

			applyRefusingNothing(t, pool, mergingEngine(), settledAt("head", 1, "unit"))
			body := toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")
			want := []record.RoleMessage{{Role: token(tc.owner, claim.RoleArchitect), Body: body}}
			for _, role := range tc.roles {
				want = append(want, record.RoleMessage{Role: token(child, role), Body: body})
			}
			assertRoleMessages(t, pool, child, want)
			assertRoleMessages(t, pool, root, nil)
		})
	}
}

// A child whose repository Legion does not serve (acme/gadgets, whose review App is not installed)
// is not recorded, and the architect told why is the one that owns its Dispatch parent: the notice
// is queued on that parent when the record holds it in the tree, so a child released under a child
// of the root reaches that child's architect while it holds its role, and a child released under
// the root reaches the root's.
func TestAChildNotRecordedIsToldOnItsRecordedParent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, parent string
	}{
		{"under a child of the root", "LEGION-209"},
		{"under the root", "LEGION-208"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			openTree(t, pool)
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Parent: new("LEGION-208"),
				Project: "LEGION", Title: "child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "V"})
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("unserved-todo", intake.DispatchIssue{Key: "LEGION-210", Seq: 1, Type: "issue.updated", Status: "todo", Title: "unserved", Parent: tc.parent, Rank: "W", ResolvedRepo: "acme/gadgets"})
			if got := issueRecord(t, pool, "LEGION-210"); got != nil {
				t.Fatalf("LEGION-210 recorded as %#v, want none", got)
			}
			notices := outboxOf(t, pool, "notice")
			if len(notices) != 1 || notices[0].issue != tc.parent || notices[0].payload["kind"] != "child-not-recorded" {
				t.Fatalf("notices = %#v, want one child-not-recorded on %s, the child's recorded parent", notices, tc.parent)
			}
		})
	}
}
