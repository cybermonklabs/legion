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
// does, so the read copies the stored bytes rather than reading the markdown again. Both versions
// of the read answer it alike, each in its own envelope: GET /api/v1/issues/{key}/results and GET
// /api/v2/issues/{key}/checks, which the page reads, both read the checks through readIssueChecks.
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

	for _, read := range []struct{ target, path string }{
		{"/api/v1/issues/" + issue.Key + "/results", "checks.0.results.0.observed_tree"},
		{"/api/v2/issues/" + issue.Key + "/checks", "items.0.results.0.observed_tree"},
	} {
		tree := treeAt(t, handler, read.target, read.path)
		if got, want := treeText(t, tree), "Ran go test: pass."; got != want {
			t.Errorf("GET %s: %s reads %q, want the observed text's words without its marks, %q",
				read.target, read.path, got, want)
		}
		for _, markup := range []string{"**", "`"} {
			if strings.Contains(string(tree), markup) {
				t.Errorf("GET %s: %s holds the markup %q:\n\t%s", read.target, read.path, markup, tree)
			}
		}
		requireAnsweredTree(t, treesRequest(t, handler, read.target, true), read.path, observed)
	}
}
