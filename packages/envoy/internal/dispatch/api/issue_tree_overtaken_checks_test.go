package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// overtakenQuestion is the `:::ask` block q1 the test's spec holds and its copy repeats word for
// word, which makes the copy's block a copy of q1's ask (docs' copiedAskSources).
const overtakenQuestion = ":::ask{#q1 urgency=\"med\"}\nWhich width does the pane keep?\n\n- 1024 px.\n- 1280 px.\n:::\n"

// TestTheTreeReadAndTheCoverageRouteCountTheChecksOfALiveRequirementUnderAnOvertakenOne holds the
// tree read and the coverage route to one rule for the checks that serve a requirement, as
// INFRA-1253's architect ruled it. The count is the fact of the live checks that serve the node,
// and "closed with its subtree" means only that no gap is listed for an overtaken node
// (dispatch://INFRA-1253/comment/cb7311d9-ca4b-4c63-b083-d8f49a9a7efa, item 2). A check counts
// where it is live: an overtaken check and a proposed check serve nothing, and the state of a node
// above a live check does not change it
// (dispatch://INFRA-1253/comment/c797249f-ab6c-4b52-a848-724932e12668). A question's state is
// its ask node's, not its `:::ask` block's (R2 and R3 of INFRA-1150), so a statement under an
// overtaken question is closed (dispatch://INFRA-1253/comment/afae111e-2b84-4a04-afd2-3f118c36d8b9),
// and a block copying an ask is that ask's block
// (dispatch://INFRA-1253/comment/2aa81020-a1e3-4037-b43b-d4b3104ab195).
//
// On an issue born converted and on one that has not converted, the spec holds rp; rc, which its
// serves places under rp, the seeded case of
// dispatch://INFRA-4062/artifact/overtaken-parent-no-check-md; ro; rs; rv; rb; rx; and the `:::ask`
// block q1, which settlement indexes into an ask. The Verification document holds c1, which serves
// rc; c2, which serves ro; c3, which serves ro and rs and so stands under ro; and cv, which serves
// rv alone. The plan holds t1, which serves rc, rs and rv. A version of the spec adds rq, whose
// parent is q1's ask, as INFRA-4063's seed writes a statement under a question. A document
// q-copy.md holds q1 word for word, and settlement claims its block as a copy of q1's ask, by the
// block's copied_from, opening no ask of its own. Then rp, ro, cv and q1's ask are set overtaken
// through the state route, rb's part_of is moved under the block q1 itself, and rx's under the
// copy's block q1. So rc is live under an overtaken block, ro is overtaken itself, rp is overtaken
// and no check serves it, rs is live and served by a live check under an overtaken parent, rv is
// live and served only by an overtaken check, and rq, rb and rx are live under an overtaken
// question, by its ask, by its block and by a copy of its block, and no check or plan task serves
// them.
//
// The coverage route and the tree read, with no view and with ?view=spec, answer the same count of
// checks for each: rp 0, rc 1, ro 2, rs 1, rv 0, rq 0, rb 0 and rx 0. The coverage route reads rp,
// rc, ro, rq, rb and rx closed, with no gap and no proof row; rs unproven, with c3 its one proof
// row and no gap; and rv a gap, with the one gap the issue's gaps name, that no check serves it.
// The tree read with no view places rq under q1's ask and answers that ask overtaken, which is how
// its reader finds rq closed; it lists no gap of its own.
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
					":::requirement{#rv}\nServed by an overtaken check alone.\n:::\n\n"+
					":::requirement{#rb}\nUnder the block of an overtaken question.\n:::\n\n"+
					":::requirement{#rx}\nUnder a copy of an overtaken question.\n:::\n\n"+overtakenQuestion, seeded.unconverted)
			if issue.Key != key {
				t.Fatalf("the seeded issue is %s, want %s, which rc's serves names", issue.Key, key)
			}
			owner := "/api/v1/issues/" + key
			verification := uploadDocument(t, handler, owner, "verification.md", checkServing("c1", spec+"rc")+"\n"+
				checkServing("c2", spec+"ro")+"\n"+checkServing("c3", spec+"ro "+spec+"rs")+"\n"+checkServing("cv", spec+"rv"))
			uploadDocument(t, handler, owner, "plan.md", planTaskServing("t1", spec+"rc "+spec+"rs "+spec+"rv"))
			ask := askOfBlock(t, database, key, "q1")
			question := "dispatch://" + key + "/ask/" + ask
			copied := readDecisionDocument(t, database, key, uploadDocument(t, handler, owner, "q-copy.md", overtakenQuestion).Artifact.Slug)
			testwait.UntilPolling(t, 20*time.Millisecond, "the copy's block "+copied.address("q1")+" to hold a current copied_from to q1's ask "+ask+", without which this case proves nothing", func() ([]graphstore.Tuple, bool) {
				ties := currentTies(t, database, copied.address("q1"), graphstore.TupleCopiedFrom)
				return ties, len(ties) == 1 && ties[0].ToKind == graphstore.KindAsk && ties[0].ToID == ask
			})
			if owned := countRows(t, database, `select count(*) from asks where block_artifact_id = $1::uuid`, copied.id); owned != 0 {
				t.Fatalf("the copy opened %d asks of its own, want none: this case proves nothing", owned)
			}
			editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end",
				"markdown": ":::requirement{#rq parent=\"" + question + "\"}\nUnder an overtaken question.\n:::\n"}})
			document := readDecisionDocument(t, database, key, "spec")
			checks := readDecisionDocument(t, database, key, verification.Artifact.Slug)
			for _, address := range []string{document.address("rp"), document.address("ro"), checks.address("cv"), question} {
				succeeded(t, "set "+address+" overtaken", writeStateAs(t, handler, key, address, graphstore.StateOvertaken, "alice"))
			}
			movePartOf(t, database, key, refs.BlockNodeKind, document.address("rb"), refs.BlockNodeKind, document.address("q1"), false)
			movePartOf(t, database, key, refs.BlockNodeKind, document.address("rx"), refs.BlockNodeKind, copied.address("q1"), false)

			walk, err := docs.WalkIssueTree(context.Background(), database.Pool, key, refs.TreeCut{})
			if err != nil {
				t.Fatalf("walk the tree of %s: %v", key, err)
			}
			walked := map[refs.Target]docs.TreeNode{}
			for _, node := range walk.Nodes {
				walked[refs.Target{Kind: node.Kind, ID: node.ID}] = node
			}
			for _, under := range []struct{ requirement, question string }{{"rb", document.address("q1")}, {"rx", copied.address("q1")}} {
				block := refs.Target{Kind: refs.BlockNodeKind, ID: under.question}
				if node := walked[refs.Target{Kind: refs.BlockNodeKind, ID: document.address(under.requirement)}]; node.Parent != block || walked[block].Type != graphstore.SupertagAsk {
					t.Fatalf("the walk of %s places %s under %+v and types %s %q, want it under that `:::ask` block: this case proves nothing",
						key, under.requirement, node.Parent, block.ID, walked[block].Type)
				}
			}
			coverage := readCoverage(t, handler, key)
			reads := map[string]treeAnswer{
				"the tree read with no view":  treeRead(t, handler, key, "", "alice", false),
				"?view=spec of the tree read": treeRead(t, handler, key, "?view=spec", "alice", false),
			}
			whole := reads["the tree read with no view"]
			requireUnder := func(read string, answer treeAnswer, address string, parent refs.Target) {
				t.Helper()
				if node, _ := answer.node(t, refs.BlockNodeKind, address); node.Parent == nil || node.Parent.Kind != parent.Kind || node.Parent.ID != parent.ID {
					t.Fatalf("%s of %s places %s under %+v, want %s %s: this case proves nothing", read, key, address, node.Parent, parent.Kind, parent.ID)
				}
			}
			for read, answer := range reads {
				requireUnder(read, answer, document.address("rc"), refs.Target{Kind: refs.BlockNodeKind, ID: document.address("rp")})
			}
			requireUnder("the tree read with no view", whole, checks.address("c3"), refs.Target{Kind: refs.BlockNodeKind, ID: document.address("ro")})
			requireUnder("the tree read with no view", whole, document.address("rq"), refs.Target{Kind: graphstore.KindAsk, ID: ask})
			if node, _ := whole.node(t, graphstore.KindAsk, ask); node.State != graphstore.StateOvertaken {
				t.Errorf("the tree read with no view of %s answers q1's ask %s at %q, want overtaken, the state its reader closes rq by", key, ask, node.State)
			}
			if len(coverage.Requirements) != 8 {
				t.Fatalf("the coverage route of %s lists the requirements %+v, want rp, rc, ro, rs, rv, rb, rx and rq", key, coverage.Requirements)
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
				{"rq", graphstore.StateLive, "closed", 0, 0, nil, nil},
				{"rb", graphstore.StateLive, "closed", 0, 0, nil, nil},
				{"rx", graphstore.StateLive, "closed", 0, 0, nil, nil},
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
				t.Errorf("the coverage route of %s lists the gaps %q, want %q: rp, rc, ro, rq, rb and rx are closed, and c3 serves rs", key, coverage.Gaps, want)
			}
		})
	}
}
