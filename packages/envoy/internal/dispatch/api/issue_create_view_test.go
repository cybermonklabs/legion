package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAnIssueCreateRefusesARequirementOnAView: an issue born converted under a parent is a view on
// the spec of the nearest issue of its parent's chain that holds requirements, while that issue is
// not done, so a create whose spec holds a requirement there is refused 409
// REQUIREMENT_BELONGS_TO_ANCESTOR, naming that issue and the requirement, and writes nothing: under
// the parent itself, and under a child of it that holds none, from which the chain climbs to the
// parent. The same spec is created as a root issue, as a child the release before writes, which
// is not converted, and under the parent once it is done; none of those is a view.
func TestAnIssueCreateRefusesARequirementOnAView(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "BORN")
	parent := createDigestIssue(t, handler, database, "BORN", "The parent", ":::requirement{#r1}\nThe parent states it.\n:::\n", false)
	view := createDigestChild(t, handler, database, "BORN", "The view", "The view's design.\n", parent.Key, false)
	if ancestor, err := docs.ViewOf(ctx, database.Pool, view.Key); err != nil || ancestor != parent.Key {
		t.Fatalf("%s is a view of %q (%v), want %s", view.Key, ancestor, err, parent.Key)
	}
	const requirement = ":::requirement{#cr9}\nThe child's own work.\n:::\n"

	// written is what a create writes, counted: issues, documents, events, nodes, tuples, trees,
	// near-copy records and the project's next number.
	written := func() string {
		t.Helper()
		var counts string
		if err := database.Pool.QueryRow(ctx, `
			select concat_ws(' ', (select count(*) from issues), (select count(*) from artifacts),
			       (select count(*) from events), (select count(*) from graph_nodes),
			       (select count(*) from graph_tuples), (select count(*) from issue_trees),
			       (select count(*) from near_copy_checks), (select next_number from projects where key = 'BORN'))`,
		).Scan(&counts); err != nil {
			t.Fatalf("count what the store holds: %v", err)
		}
		return counts
	}
	before := written()
	for _, under := range []model.Issue{parent, view} {
		what := "a child of " + under.Key + " whose spec holds a requirement"
		refusal := requireRefused(t, what, createIssueRequest(t, handler, map[string]any{
			"project": "BORN", "title": "A child of " + under.Key, "spec": requirement, "parent": under.Key, "force": true,
		}), http.StatusConflict, codeRequirementBelongsToAncestor, parent.Key)
		if !strings.Contains(refusal.Error, "cr9") {
			t.Errorf("%s is refused %q, want the requirement it adds named", what, refusal.Error)
		}
	}
	if after := written(); after != before {
		t.Errorf("the refused creates moved the counts of issues, artifacts, events, nodes, tuples, trees, near-copy records and the next number from %s to %s, want nothing written", before, after)
	}

	for _, made := range []model.Issue{
		createDigestIssue(t, handler, database, "BORN", "A root", requirement, false),
		createDigestChild(t, handler, database, "BORN", "The release before's child", requirement, parent.Key, true),
	} {
		if ancestor, err := docs.ViewOf(ctx, database.Pool, made.Key); err != nil || ancestor != "" {
			t.Errorf("%s, created with a requirement, is a view of %q (%v), want none", made.Key, ancestor, err)
		}
	}
	succeeded(t, "close the parent", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+parent.Key, map[string]any{"status": "done"}, "alice"))
	closed := createDigestChild(t, handler, database, "BORN", "A child of the done parent", requirement, parent.Key, false)
	if ancestor, err := docs.ViewOf(ctx, database.Pool, closed.Key); err != nil || ancestor != "" {
		t.Errorf("%s, created with a requirement under a done parent, is a view of %q (%v), want none", closed.Key, ancestor, err)
	}
}
