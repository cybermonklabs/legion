package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestTheTreeReadAndTheCoverageRouteCountTheChecksOfALiveRequirementUnderAnOvertakenOne holds the
// tree read and the coverage route to one rule for the checks that serve a requirement, as
// INFRA-1253's architect ruled it. The count is the fact of the live checks that serve the node,
// and "closed with its subtree" means only that no gap is listed for an overtaken node
// (dispatch://INFRA-1253/comment/cb7311d9-ca4b-4c63-b083-d8f49a9a7efa, item 2). A check counts
// where it is live: an overtaken check and a proposed check serve nothing, and the state of a node
// above a live check does not change it
// (dispatch://INFRA-1253/comment/c797249f-ab6c-4b52-a848-724932e12668).
//
// On an issue born converted and on one that has not converted, the spec holds five requirements:
// rp; rc, which its serves places under rp, the seeded case of
// dispatch://INFRA-4062/artifact/overtaken-parent-no-check-md; ro; rs; and rv. The Verification
// document holds c1, which serves rc; c2, which serves ro; c3, which serves ro and rs and so stands
// under ro; and cv, which serves rv alone. The plan holds t1, which serves rc, rs and rv. Then rp,
// ro and cv are set overtaken through the state route. So rc is live under an overtaken block, ro
// is overtaken itself, rp is overtaken and no check serves it, rs is live and served by a live
// check under an overtaken parent, and rv is live and served only by an overtaken check. The
// coverage route and the tree read, with no view and with ?view=spec, answer the same count of
// checks for each: rp 0, rc 1, ro 2, rs 1 and rv 0. The coverage route reads rp, rc and ro closed,
// with no gap and no proof row; rs unproven, with c3 its one proof row and no gap; and rv a gap,
// with the one gap the issue's gaps name, that no check serves it.
//
// Before the first ruling both reads answered 0 for rc and ro: the tree read set the count to 0
// under an overtaken block (requirementChecks), and the coverage route counted nothing for a closed
// requirement (coverTreeRequirements, api/coverage_tuples.go:248-249). rs and rv held without a
// change on this branch, since both reads leave out an overtaken check and read a check's own
// state alone (readServingChecks, requirementChecksStatement). The coverage route the second ruling
// names, which counted an overtaken check, is the markdown walk of the branch's base, which this
// branch replaced with the read over the tuples for every issue, converted or not.
func TestTheTreeReadAndTheCoverageRouteCountTheChecksOfALiveRequirementUnderAnOvertakenOne(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	for _, seeded := range []struct {
		name, project string
		unconverted   bool
	}{{"converted", "OVTC", false}, {"unconverted", "OVTU", true}} {
		t.Run(seeded.name, func(t *testing.T) {
			key := seeded.project + "-1"
			spec := "dispatch://" + key + "/spec#b-"
			createReferenceAPIProject(t, handler, seeded.project)
			issue := createDigestIssue(t, handler, database, seeded.project, "Live checks under overtaken nodes",
				":::requirement{#rp}\nThe parent.\n:::\n\n"+
					":::requirement{#rc serves=\""+spec+"rp\"}\nThe child.\n:::\n\n"+
					":::requirement{#ro}\nOvertaken itself.\n:::\n\n"+
					":::requirement{#rs}\nServed by a live check under an overtaken one.\n:::\n\n"+
					":::requirement{#rv}\nServed by an overtaken check alone.\n:::\n", seeded.unconverted)
			if issue.Key != key {
				t.Fatalf("the seeded issue is %s, want %s, which rc's serves names", issue.Key, key)
			}
			owner := "/api/v1/issues/" + key
			verification := uploadDocument(t, handler, owner, "verification.md", checkServing("c1", spec+"rc")+"\n"+
				checkServing("c2", spec+"ro")+"\n"+checkServing("c3", spec+"ro "+spec+"rs")+"\n"+checkServing("cv", spec+"rv"))
			uploadDocument(t, handler, owner, "plan.md", planTaskServing("t1", spec+"rc "+spec+"rs "+spec+"rv"))
			document := readDecisionDocument(t, database, key, "spec")
			checks := readDecisionDocument(t, database, key, verification.Artifact.Slug)
			for _, address := range []string{document.address("rp"), document.address("ro"), checks.address("cv")} {
				succeeded(t, "set "+address+" overtaken", writeStateAs(t, handler, key, address, graphstore.StateOvertaken, "alice"))
			}

			coverage := readCoverage(t, handler, key)
			reads := map[string]treeAnswer{
				"the tree read with no view":  treeRead(t, handler, key, "", "alice", false),
				"?view=spec of the tree read": treeRead(t, handler, key, "?view=spec", "alice", false),
			}
			requireUnder := func(read string, answer treeAnswer, address, parent string) {
				t.Helper()
				if node, _ := answer.node(t, refs.BlockNodeKind, address); node.Parent == nil || node.Parent.Kind != refs.BlockNodeKind || node.Parent.ID != parent {
					t.Fatalf("%s of %s places %s under %+v, want %s: this case proves nothing", read, key, address, node.Parent, parent)
				}
			}
			for read, answer := range reads {
				requireUnder(read, answer, document.address("rc"), document.address("rp"))
			}
			requireUnder("the tree read with no view", reads["the tree read with no view"], checks.address("c3"), document.address("ro"))
			if len(coverage.Requirements) != 5 {
				t.Fatalf("the coverage route of %s lists the requirements %+v, want rp, rc, ro, rs and rv", key, coverage.Requirements)
			}
			for _, want := range []struct {
				block, state, coverage string
				checks, planTasks      int
				gaps, proof            []string
			}{
				{"rp", graphstore.StateOvertaken, "closed", 0, 0, nil, nil},
				{"rc", graphstore.StateLive, "closed", 1, 1, nil, nil},
				{"ro", graphstore.StateOvertaken, "closed", 2, 0, nil, nil},
				{"rs", graphstore.StateLive, "unproven", 1, 1, nil, []string{checks.address("c3")}},
				{"rv", graphstore.StateLive, "gap", 0, 1, []string{noCheckReason}, nil},
			} {
				requirement := requirementOf(t, coverage, want.block)
				var proof []string
				for _, row := range requirement.Proof {
					proof = append(proof, row.Check)
				}
				if requirement.State != want.state || requirement.Coverage != want.coverage || requirement.Checks != want.checks ||
					requirement.PlanTasks != want.planTasks || !slices.Equal(requirement.Gaps, want.gaps) || !slices.Equal(proof, want.proof) {
					t.Errorf("the coverage route of %s answers %s as %+v, want state %s, %s, %d check(s) and %d plan task(s), the gaps %q and the proof rows %q",
						key, want.block, requirement, want.state, want.coverage, want.checks, want.planTasks, want.gaps, want.proof)
				}
				for read, answer := range reads {
					node, _ := answer.node(t, refs.BlockNodeKind, document.address(want.block))
					if node.Checks == nil || *node.Checks != want.checks {
						t.Errorf("%s of %s answers %s with checks %s, want %d, the count the coverage route answers", read, key, want.block, entryJSON(t, node.Checks), want.checks)
					}
				}
			}
			if want := []string{"rv " + noCheckReason}; !slices.Equal(coverage.Gaps, want) {
				t.Errorf("the coverage route of %s lists the gaps %q, want %q: rp, rc and ro are closed, and c3 serves rs", key, coverage.Gaps, want)
			}
		})
	}
}
