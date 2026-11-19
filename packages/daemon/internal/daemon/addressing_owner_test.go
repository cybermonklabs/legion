package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The addressing each launch is told names the architect that owns the claim, as the store records
// the architect claims of its tree at that launch (specs.ownerOf over the workflow's
// owningArchitect). A child's planner launched while the child's own architect holds its role is
// told that architect, and so is a worker on an issue under the child that has no architect of its
// own; launched again once the child's architect has failed, the planner is told the architect
// above it, the root's. An architect on a child owns that child itself, so it is told the architect
// above it, in those words, by the same walk from its parent. Every claim on the tree's root, its
// architect's own included, is told the root's architect, as before. Every other part of the
// sentence is the one a claim was always told.
func TestTheAddressingNamesTheIssuesOwnArchitect(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	const root, child, grandchild = "LEGION-1", "LEGION-2", "LEGION-3"
	for _, member := range []struct{ key, parent string }{{root, ""}, {child, root}, {grandchild, child}} {
		issue := record.Issue{Key: member.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root,
			Title: member.key, Phase: phase.Planning, Generation: 1, Status: "in_progress"}
		if member.parent != "" {
			issue.Parent = new(member.parent)
		}
		putOutboxIssue(t, pool, records, issue)
	}
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	answers := &workflowRuntime{pool: pool, records: records}
	s := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, appOwners: noAppOwners, owner: answers.owningArchitect}
	architect := func(issue string, state supervise.ClaimState) {
		t.Helper()
		if err := st.PutClaim(ctx, supervise.Claim{Token: mustClaimToken(t, issue, claim.RoleArchitect), Project: "legion", Tree: root, Issue: issue,
			Role: claim.RoleArchitect, Generation: 1, State: state}); err != nil {
			t.Fatalf("record the architect claim of %s %s: %v", issue, state, err)
		}
	}
	addressing := func(issue string, role claim.Role) string {
		t.Helper()
		spec, err := s.SpawnSpec(ctx, supervise.Claim{Token: mustClaimToken(t, issue, role), Project: "legion", Tree: root, Issue: issue, Role: role})
		if err != nil {
			t.Fatalf("SpawnSpec of %s's %s: %v", issue, role, err)
		}
		return spec.Prompt.Addressing
	}
	owns := func(issue string) string {
		return "; the architect that owns your issue is `" + architectTopic(t, issue) + "`; "
	}
	above := func(issue string) string { return "; the architect above you is `" + architectTopic(t, issue) + "`; " }
	told := func(issue string, role claim.Role, want string) {
		t.Helper()
		got := addressing(issue, role)
		if !strings.Contains(got, want) {
			t.Errorf("%s's %s is told %q; want it to say %q", issue, role, got, want)
		}
		if role != claim.RoleArchitect && strings.Contains(got, "the architect above you") {
			t.Errorf("%s's %s is told %q; only an architect on a child is told the architect above it", issue, role, got)
		}
	}

	architect(root, supervise.StateWorking)
	architect(child, supervise.StateWorking)
	architect(grandchild, supervise.StateWorking)
	// The whole sentence a child's planner is told, so nothing but the architect it names moved.
	want := "Legion addressing: your role topic is `" + notify.RoleTopicPrefix + string(mustClaimToken(t, child, claim.RolePlanner)) + "`" + owns(child) +
		"the project's controller is `" + notify.RoleTopicPrefix + string(claim.ControllerToken("legion")) + "`; " +
		"a sibling role on your issue is your topic with the trailing `-<role>` replaced."
	if got := addressing(child, claim.RolePlanner); got != want {
		t.Errorf("the child's planner, while the child's architect works, is told %q; want %q", got, want)
	}
	told(child, claim.RoleImplementer, owns(child))
	told(child, claim.RoleArchitect, above(root))
	told(grandchild, claim.RoleArchitect, above(child))
	told(root, claim.RolePlanner, owns(root))
	told(root, claim.RoleArchitect, owns(root))

	// The grandchild's architect has failed: a worker on the grandchild is the child's architect's.
	architect(grandchild, supervise.StateFailed)
	told(grandchild, claim.RoleTester, owns(child))

	// The child's architect has failed: its planner's next launch is told the architect above it.
	architect(child, supervise.StateFailed)
	told(child, claim.RolePlanner, owns(root))
	told(grandchild, claim.RoleTester, owns(root))
	told(child, claim.RoleArchitect, above(root))
	told(root, claim.RolePlanner, owns(root))
}
