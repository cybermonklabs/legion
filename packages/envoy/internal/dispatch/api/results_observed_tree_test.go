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
