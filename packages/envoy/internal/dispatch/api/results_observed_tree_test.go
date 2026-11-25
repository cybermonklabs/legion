package api

import (
	"net/http"
	"strings"
	"testing"
)

// A result's observed text is markdown, and the results read answers the tree a reader is shown
// of it as `observed_tree` under `Dispatch-Trees: 1`, as it answers a check's `text_tree`, so the
// page parses no result's text: the tree holds the text's words without its marks. A read without
// the header carries none. The result's write stores that tree, as every write of a stored text
// does, so the read copies the stored bytes rather than reading the markdown again.
func TestTheResultsReadCarriesEachResultsObservedTree(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "OBST", "Observed trees", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	const observed = "Ran `go test`: **pass**."
	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit,
		map[string]any{"surface": "cli", "observed": observed}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if _, held := storedTreeOf(t, database, observed); !held {
		t.Errorf("the result's write stored no display tree of %q", observed)
	}

	target := "/api/v1/issues/" + issue.Key + "/results"
	const path = "checks.0.results.0.observed_tree"
	tree := treeAt(t, handler, target, path)
	if got, want := treeText(t, tree), "Ran go test: pass."; got != want {
		t.Errorf("%s reads %q, want the observed text's words without its marks, %q", path, got, want)
	}
	for _, markup := range []string{"**", "`"} {
		if strings.Contains(string(tree), markup) {
			t.Errorf("%s holds the markup %q:\n\t%s", path, markup, tree)
		}
	}
	requireAnsweredTree(t, treesRequest(t, handler, target, true), path, observed)
}

// A check CI proves holds the results its job recorded in recorded_results, apart from results,
// where the result derived from the job's run stands (keepFieldsOlderCommandsRead moves them
// there). Each of them carries its observed text's tree as a result in results does: the read
// registers each result's tree only once no result is moved any more.
func TestACICheckRecordedResultsCarryTheirObservedTrees(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	const observed = "The job ran `go test`: **pass**."
	result := jobResultOf("c1", "pass")
	result["observed"] = observed
	jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), result))

	target := "/api/v1/issues/" + rig.issue + "/results"
	if check, _ := valueAt(t, treesRequest(t, rig.handler, target, true), "checks.0.block_id"); string(check) != `"c1"` {
		t.Fatalf("the read's first check is %s, want c1, the check CI proves", check)
	}
	const path = "checks.0.recorded_results.0.observed_tree"
	tree := treeAt(t, rig.handler, target, path)
	if got, want := treeText(t, tree), "The job ran go test: pass."; got != want {
		t.Errorf("%s reads %q, want the observed text's words without its marks, %q", path, got, want)
	}
	requireAnsweredTree(t, treesRequest(t, rig.handler, target, true), path, observed)
}
