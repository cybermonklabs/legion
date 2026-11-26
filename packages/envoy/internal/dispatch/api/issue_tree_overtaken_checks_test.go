package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestTheTreeReadAndTheCoverageRouteCountTheChecksOfALiveRequirementUnderAnOvertakenOne is the
// seeded case of dispatch://INFRA-4062/artifact/overtaken-parent-no-check-md, as INFRA-1253's
// architect ruled on it (dispatch://INFRA-1253/comment/cb7311d9-ca4b-4c63-b083-d8f49a9a7efa, item
// 2): the count is a fact, and "closed with its subtree" means that no gap is listed for an
// overtaken node, not that nothing serves it. The spec holds rp and rc, which its serves places
// under rp, and ro beside them. The Verification document holds c1, which serves rc, and c2, which
// serves ro. The plan holds t1, which serves rc. Then rp and ro are set overtaken through the state
// route. So rc is live under an overtaken block, ro is overtaken itself, and rp is overtaken and no
// check serves it. The coverage route and the tree read, with no view and with ?view=spec, answer
// the same count of checks for each requirement: rc 1, ro 1 and rp 0. The coverage route also
// answers rc 1 plan task. It reads all three closed, with no gap and no proof row, and the issue's
// gaps name none of them. The tree read lists no gap of its own; its only coverage member is this
// count. Before this change both reads answered 0 for rc and ro. The tree read set the count to 0
// under an overtaken block (requirementChecks), and the coverage route counted nothing for a closed
// requirement (coverTreeRequirements, api/coverage_tuples.go:248-249).
func TestTheTreeReadAndTheCoverageRouteCountTheChecksOfALiveRequirementUnderAnOvertakenOne(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "OVTK", "A live requirement under an overtaken one",
		":::requirement{#rp}\nThe parent.\n:::\n\n"+
			":::requirement{#rc serves=\"dispatch://OVTK-1/spec#b-rp\"}\nThe child.\n:::\n\n"+
			":::requirement{#ro}\nOvertaken itself.\n:::\n")
	if issue.Key != "OVTK-1" {
		t.Fatalf("the seeded issue is %s, want OVTK-1, which rc's serves names", issue.Key)
	}
	spec := "dispatch://" + issue.Key + "/spec#b-"
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", checkServing("c1", spec+"rc")+"\n"+checkServing("c2", spec+"ro"))
	uploadDocument(t, handler, owner, "plan.md", planTaskServing("t1", spec+"rc"))
	document := readDecisionDocument(t, database, issue.Key, "spec")
	for _, block := range []string{"rp", "ro"} {
		succeeded(t, "set "+block+" overtaken", writeStateAs(t, handler, issue.Key, document.address(block), graphstore.StateOvertaken, "alice"))
	}

	coverage := readCoverage(t, handler, issue.Key)
	reads := map[string]treeAnswer{
		"the tree read with no view":  treeRead(t, handler, issue.Key, "", "alice", false),
		"?view=spec of the tree read": treeRead(t, handler, issue.Key, "?view=spec", "alice", false),
	}
	rp := document.address("rp")
	for read, answer := range reads {
		if rc, _ := answer.node(t, refs.BlockNodeKind, document.address("rc")); rc.Parent == nil || rc.Parent.Kind != refs.BlockNodeKind || rc.Parent.ID != rp {
			t.Fatalf("%s of %s places rc under %+v, want rp %s: this case proves nothing", read, issue.Key, rc.Parent, rp)
		}
	}
	if len(coverage.Requirements) != 3 {
		t.Fatalf("the coverage route of %s lists the requirements %+v, want rp, rc and ro", issue.Key, coverage.Requirements)
	}
	for _, want := range []struct {
		block, state      string
		checks, planTasks int
	}{
		{"rp", graphstore.StateOvertaken, 0, 0},
		{"rc", graphstore.StateLive, 1, 1},
		{"ro", graphstore.StateOvertaken, 1, 0},
	} {
		requirement := requirementOf(t, coverage, want.block)
		if requirement.State != want.state || requirement.Checks != want.checks || requirement.PlanTasks != want.planTasks ||
			requirement.Coverage != "closed" || len(requirement.Gaps) != 0 || len(requirement.Proof) != 0 {
			t.Errorf("the coverage route of %s answers %s as %+v, want state %s, %d check(s) and %d plan task(s), closed, with no gap and no proof row",
				issue.Key, want.block, requirement, want.state, want.checks, want.planTasks)
		}
		for read, answer := range reads {
			node, _ := answer.node(t, refs.BlockNodeKind, document.address(want.block))
			if node.Checks == nil || *node.Checks != want.checks {
				t.Errorf("%s of %s answers %s with checks %s, want %d, the count the coverage route answers", read, issue.Key, want.block, entryJSON(t, node.Checks), want.checks)
			}
		}
	}
	if len(coverage.Gaps) != 0 {
		t.Errorf("the coverage route of %s lists the gaps %q, want none: rp, rc and ro are each closed", issue.Key, coverage.Gaps)
	}
}
