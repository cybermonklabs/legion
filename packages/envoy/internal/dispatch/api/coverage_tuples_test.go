package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestTheCoverageReadOverTheTuplesHoldsTheFourStatesApart: the coverage read over the tuples reads
// a requirement no check serves as a gap; one whose check holds no result, one whose check's newest
// result is a fail and one whose check passed against words it no longer holds as unproven, each
// with where its check stands; one whose check passed against its words as proved, and unproven
// once that result is retracted; an overtaken requirement as closed, with no gap and no check
// under it; and a proposed one as raising no gap.
func TestTheCoverageReadOverTheTuplesHoldsTheFourStatesApart(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	issue := createInteractionIssue(t, handler, "COVT", "Coverage states",
		":::requirement{#rg}\nNo check proves this.\n:::\n\n:::requirement{#ru1}\nA check with no result.\n:::\n\n"+
			":::requirement{#ru2}\nA check that failed.\n:::\n\n:::requirement{#ru3}\nA check reworded after its pass.\n:::\n\n"+
			":::requirement{#rp}\nA check that passed.\n:::\n\n:::requirement{#ro}\nOvertaken.\n:::\n\n:::requirement{#rq}\nProposed.\n:::\n")
	spec := "dispatch://" + issue.Key + "/spec#b-"
	checks := func(reworded string) string {
		return checkServing("cu1", spec+"ru1") + "\n" + checkServing("cu2", spec+"ru2") + "\n" +
			":::check{#cu3 serves=\"" + spec + "ru3\" surface=\"cli\" run=\"go test ./...\"}\n" + reworded + "\n:::\n\n" +
			checkServing("cp", spec+"rp") + "\n" + checkServing("co", spec+"ro")
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checks("The check that proves it."))
	document := readDecisionDocument(t, database, issue.Key, "spec")
	for block, state := range map[string]string{"ro": graphstore.StateOvertaken, "rq": graphstore.StateProposed} {
		succeeded(t, "set "+block+" "+state, dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/nodes/"+url.PathEscape(document.address(block))+"/state", map[string]any{"state": state}, "alice"))
	}
	record := func(check, verdict string, fields map[string]any) model.CheckResult {
		t.Helper()
		recorded := recordResult(t, handler, issue.Key, resultBody(check, verdict, headCommit, fields))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a %s of %s: status=%d body=%s", verdict, check, recorded.Code, recorded.Body.String())
		}
		return decodeBody[recordedResult](t, recorded).Result
	}
	record("cu2", "fail", map[string]any{"failed": 1})
	record("cu3", "pass", nil)
	passed := record("cp", "pass", nil)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checks("The check that proves it, reworded."))

	read := func(what string) model.IssueCoverage {
		t.Helper()
		coverage, err := directServer(deps).issueCoverageOverTuples(ctx, database.Pool, alice, issue.Key, nil)
		if err != nil {
			t.Fatalf("%s: read the coverage of %s over the tuples: %v", what, issue.Key, err)
		}
		return coverage
	}
	coverage := read("before the retraction")
	for _, want := range []struct {
		block, coverage, proves string
		gaps                    []string
	}{
		{"rg", "gap", "", []string{noCheckReason}},
		{"ru1", "unproven", provesNone, []string{}},
		{"ru2", "unproven", provesFail, []string{}},
		{"ru3", "unproven", provesReworded, []string{}},
		{"rp", "proved", provesPass, []string{}},
		{"ro", "closed", "", []string{}},
		{"rq", graphstore.StateProposed, "", []string{}},
	} {
		requirement := requirementOf(t, coverage, want.block)
		if requirement.Coverage != want.coverage || !slices.Equal(requirement.Gaps, want.gaps) {
			t.Errorf("%s reads %s with the gaps %q, want %s with %q", want.block, requirement.Coverage, requirement.Gaps, want.coverage, want.gaps)
		}
		switch {
		case want.proves == "" && len(requirement.Proof) > 0:
			t.Errorf("%s lists the checks %+v, want none", want.block, requirement.Proof)
		case want.proves != "" && (len(requirement.Proof) != 1 || requirement.Proof[0].Proves != want.proves || requirement.Proof[0].Issue != issue.Key):
			t.Errorf("%s lists the checks %+v, want its one check of %s at %s", want.block, requirement.Proof, issue.Key, want.proves)
		}
	}
	if response := retractAsPerson(t, handler, passed.ID, "The run named the wrong commit."); response.Code != http.StatusCreated {
		t.Fatalf("retract the pass of cp: status=%d body=%s", response.Code, response.Body.String())
	}
	if requirement := requirementOf(t, read("after the retraction"), "rp"); requirement.Coverage != "unproven" ||
		len(requirement.Proof) != 1 || requirement.Proof[0].Proves != provesNone {
		t.Errorf("after its one pass was retracted rp reads %s with %+v, want unproven, its check holding no result that stands", requirement.Coverage, requirement.Proof)
	}
}

// TestTheCoverageAndResultsReadsAnswerFromTheTuples: the coverage route and the results route
// answer over the store's tuples on an issue the release before would have written - not
// converted, its documents each a room of its own - and on an issue born converted, each with
// requirements, checks serving one and two of them, a plan task and a recorded result. A serves
// tuple written into the store alone, which no document's markdown holds, is read by the coverage
// route, proving the route answers from the store rather than walking the spec's markdown.
func TestTheCoverageAndResultsReadsAnswerFromTheTuples(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	createReferenceAPIProject(t, handler, "TWIN")
	seed := func(unconverted bool) string {
		t.Helper()
		issue := createDigestIssue(t, handler, database, "TWIN", "Tuples", "A paragraph of prose.\n\n"+
			":::requirement{#r1}\nThe list comes first.\n:::\n\n:::requirement{#r2}\nAn ask keeps its urgency.\n:::\n", unconverted)
		spec := "dispatch://" + issue.Key + "/spec#b-"
		owner := "/api/v1/issues/" + issue.Key
		uploadDocument(t, handler, owner, "verification.md", checkServing("c1", spec+"r1")+"\n"+
			":::check{#c2 serves=\""+spec+"r1 "+spec+"r2\" surface=\"cli\" run=\"go test ./...\"}\nThe second check.\n:::\n")
		uploadDocument(t, handler, owner, "plan.md", planTaskServing("t1", spec+"r1"))
		if recorded := recordResult(t, handler, issue.Key, resultBody("c1", "pass", headCommit, nil)); recorded.Code != http.StatusCreated {
			t.Fatalf("record a pass of c1 on %s: status=%d body=%s", issue.Key, recorded.Code, recorded.Body.String())
		}
		return issue.Key
	}
	for _, unconverted := range []bool{true, false} {
		key := seed(unconverted)
		coverage := readCoverage(t, handler, key)
		if !coverage.HasPlan || requirementOf(t, coverage, "r1").Checks != 2 || requirementOf(t, coverage, "r1").PlanTasks != 1 ||
			requirementOf(t, coverage, "r2").Checks != 1 || requirementOf(t, coverage, "r2").PlanTasks != 0 {
			t.Errorf("the coverage of %s (unconverted %t) is %+v: want c2 listed once under each requirement it serves", key, unconverted, coverage)
		}
		if want := []string{"r2 " + noPlanTaskReason}; !slices.Equal(coverage.Gaps, want) {
			t.Errorf("the gaps of %s (unconverted %t) are %q, want %q", key, unconverted, coverage.Gaps, want)
		}
		results := readResults(t, handler, key)
		if len(results.Checks) != 2 || results.Checks[0].BlockID != "c1" || results.Checks[1].BlockID != "c2" ||
			len(results.Checks[0].Results) != 1 || results.Checks[0].Results[0].Verdict != "pass" {
			t.Errorf("the results of %s (unconverted %t) are %+v: want c1 (with its pass) then c2", key, unconverted, results)
		}
	}

	// The planted serves tuple: a serves tuple of c1 to r2 that only the store holds.
	key := seed(false)
	checks := readDecisionDocument(t, database, key, "verification-md")
	requirements := readDecisionDocument(t, database, key, "spec")
	if err := graphstoreWrite(ctx, database, graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: checks.address("c1"), Kind: servesTupleKind,
		ToKind: refs.BlockNodeKind, ToID: requirements.address("r2"),
		At: time.Now().UTC(), Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
	}); err != nil {
		t.Fatalf("write the planted serves tuple: %v", err)
	}
	if coverage := readCoverage(t, handler, key); requirementOf(t, coverage, "r2").Checks != 2 {
		t.Errorf("with a serves tuple only the store holds, r2 of %s has %d checks, want 2", key, requirementOf(t, coverage, "r2").Checks)
	}
}

// TestARequirementPastedIntoAViewIsAGapOfItsOwnKind is c-child-view's first Go test. A parent's
// spec holds one requirement and a child of it, born converted, holds one check serving it, so the
// child is a view of the parent. A client that skips the editor's creation gate writes a
// requirement block into the child's spec fragment through the issue room, where no route's check
// runs; the room's settlement versions it and its block index states it. The child's tree read
// answers the block as a requirement node of the child, as its text and type say. The coverage
// read names it in the one line `<block id> is a requirement on a view` of its gaps and lists no
// requirement but the parent's that the child's check serves. The child is still a view of the
// parent, and a requirement written on it through
// POST /api/v1/artifacts/{id}/edits after the paste is still refused, since the pasted requirement
// does not end the view. The parent's coverage names no such gap.
func TestARequirementPastedIntoAViewIsAGapOfItsOwnKind(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "PASTE")
	parent := createDigestIssue(t, handler, database, "PASTE", "The parent", ":::requirement{#r1}\nThe parent states it.\n:::\n", false)
	child := createDigestChild(t, handler, database, "PASTE", "The view", "The view's design.\n", parent.Key, false)
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", checkServing("c1", "dispatch://"+parent.Key+"/spec#b-r1"))
	if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf == nil || *read.ViewOf != parent.Key {
		t.Fatalf("before the paste the tree read of %s answers view_of %v, want %s", child.Key, read.ViewOf, parent.Key)
	}

	const block = "pr1"
	pasteRequirement(t, handler, documents, child, block, "The pasted work.")

	read := treeRead(t, handler, child.Key, "", "alice", false)
	if node, held := blockNodeOf(read, block); !held || node.Supertag != requirementBlockType || node.Issue != "" {
		typ := "no node"
		if held {
			typ = node.Supertag
			if node.Issue != "" {
				typ += " of " + node.Issue
			}
		}
		t.Errorf("the tree read of %s answers the pasted block %s as %s, want requirement", child.Key, block, typ)
	}
	line := block + " " + viewRequirementReason
	coverage := readCoverage(t, handler, child.Key)
	if !slices.Contains(coverage.Gaps, line) {
		t.Errorf("the coverage of %s holds no gap for the pasted requirement %s: %q", child.Key, block, coverage.Gaps)
	}
	var listed []string
	for _, requirement := range coverage.Requirements {
		listed = append(listed, requirement.BlockID)
	}
	if slices.Contains(listed, block) {
		t.Errorf("the coverage of %s counts the pasted requirement %s as a requirement of the view", child.Key, block)
	}
	if !slices.Equal(listed, []string{"r1"}) || requirementOf(t, coverage, "r1").Checks != 1 {
		t.Errorf("the coverage of %s lists the requirements %q, want the parent's r1 alone, served by the child's check", child.Key, listed)
	}
	if read.ViewOf == nil || *read.ViewOf != parent.Key {
		t.Errorf("the tree read of %s answers view_of %s after the paste, want %s", child.Key, viewOfValue(read.ViewOf), parent.Key)
	}
	if response := insertRequirement(t, handler, child.PrimaryArtifactID, "cr9", "The child's own work."); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), codeRequirementBelongsToAncestor) || !strings.Contains(response.Body.String(), parent.Key) {
		t.Errorf("a requirement write on %s after the paste answered %d, want 409 REQUIREMENT_BELONGS_TO_ANCESTOR naming %s: %s",
			child.Key, response.Code, parent.Key, response.Body.String())
	}
	for _, gap := range readCoverage(t, handler, parent.Key).Gaps {
		if strings.Contains(gap, viewRequirementReason) {
			t.Errorf("the coverage of %s, which is no view, names the gap %q", parent.Key, gap)
		}
	}
}
