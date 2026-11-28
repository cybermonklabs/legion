package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
	t.Parallel()
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
		"your issue had no plan rows when you were started, so each of its roles is its own agent until it runs a plan: " +
		"a sibling role on your issue is your topic with the trailing `-<role>` replaced, " +
		"and once your issue runs a plan, `dispatch steps --issue " + child + "` gives the agent of each of its steps."
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

// The plan `implementer:a tester:a reviewer` runs key a's agent, `…-implementer`, on the
// implementer's step and on the tester's, so the addressing the reviewer is told names the
// tester's step on `…-implementer` and each step on its agent's topic. The token is the one
// claim.AgentToken spells from the step's agent's name
// (record.PlanStep.AgentName). An issue with no plan rows is told the rule it was always told,
// with the case it holds in (each role its own agent until the issue runs a plan) and the read
// that names the agent of each step of a plan it runs later.
func TestTheAddressingNamesTheAgentOfEachStep(t *testing.T) {
	t.Parallel()
	steps := []record.PlanStep{
		{Issue: "LEGION-209", Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", AgentKey: "a", Agent: "implementer"},
		{Issue: "LEGION-209", Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing", AgentKey: "a", Agent: "implementer"},
		{Issue: "LEGION-209", Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review", Agent: "reviewer"},
	}
	workers, err := stepWorkersOf("legion", "LEGION-209", "LEGION-208", &record.StoredPlan{Steps: map[string][]record.PlanStep{"a": steps}})
	if err != nil {
		t.Fatalf("the step agents: %v", err)
	}
	reviewer := supervise.Claim{Token: mustClaimToken(t, "LEGION-209", claim.RoleReviewer), Issue: "LEGION-209", Tree: "LEGION-208", Role: claim.RoleReviewer}
	architect := mustClaimToken(t, "LEGION-208", claim.RoleArchitect)
	told := addressingFragment("legion", reviewer, architect, workers, ghrepo.Repository{}, "", nil)
	for _, want := range []string{
		"step 1 of task a (implementer) `" + notify.RoleTopicPrefix + "legion-legion-legion-209-implementer`",
		"step 2 of task a (tester) `" + notify.RoleTopicPrefix + "legion-legion-legion-209-implementer`",
		"step 3 of task a (reviewer) `" + notify.RoleTopicPrefix + "legion-legion-legion-209-reviewer`",
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the reviewer is told %q; want it naming %q", told, want)
		}
	}
	if bare := addressingFragment("legion", reviewer, architect, nil, ghrepo.Repository{}, "", nil); !strings.HasSuffix(bare,
		"your issue had no plan rows when you were started, so each of its roles is its own agent until it runs a plan: "+
			"a sibling role on your issue is your topic with the trailing `-<role>` replaced, "+
			"and once your issue runs a plan, `dispatch steps --issue LEGION-209` gives the agent of each of its steps.") {
		t.Errorf("an issue with no plan rows is told %q; want the rule it was always told, its case and where a later plan's agents are read", bare)
	}
}
