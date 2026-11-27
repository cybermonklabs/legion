package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAParentsProgressCountsOnlyTheChildrenItsReaderSees: a child may be of another project than
// its parent (main's #4896), so PUB's parent holds an open PUB child and a done PRIV child, and the
// open PUB child holds a done PRIV grandchild. The single read's progress, the listing's progress
// and the children rollup count only the issues their reader sees: carol, who sees PRIV, reads the
// parent's children 1 done of 2, and its PUB child's subtree 1 done of 2; alice, who does not see
// PRIV, reads 0 done of 1 and 0 done of 1, and the children list names no PRIV issue. With PRIV
// public, alice reads what carol reads (R9).
//
// Control: the three counts with no project guard (issueProgressLateral, issueSummaryJoins' grouped
// count and issueChildrenStatement's subtree), and alice reads 1 done of 2 in each, the hidden child
// counted.
func TestAParentsProgressCountsOnlyTheChildrenItsReaderSees(t *testing.T) {
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	parent := issueIn(t, handler, accessCarol, publicProject)
	child := func(project, parent string) string {
		t.Helper()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "A child of " + parent, "parent": parent,
		}, accessCarol)
		if created.Code != http.StatusCreated {
			t.Fatalf("carol files a %s child of %s: status=%d body=%s; the leak needs a child of another project", project, parent, created.Code, created.Body.String())
		}
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, created).Key
	}
	open := child(publicProject, parent)
	hiddenChild := child(privateProject, parent)
	hiddenGrandchild := child(privateProject, open)
	for _, key := range []string{hiddenGrandchild, hiddenChild} {
		if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
			map[string]any{"status": "done"}, accessCarol); closed.Code != http.StatusOK {
			t.Fatalf("carol closes %s: status=%d body=%s", key, closed.Code, closed.Body.String())
		}
	}

	type count struct{ done, total int }
	type detail struct {
		Progress model.IssueProgress `json:"progress"`
		Children []struct {
			Key          string `json:"key"`
			SubtreeDone  int    `json:"subtree_done"`
			SubtreeTotal int    `json:"subtree_total"`
		} `json:"children"`
	}
	check := func(who string, children, subtree count) {
		t.Helper()
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent, nil, who)
		if read.Code != http.StatusOK {
			t.Fatalf("%s reads %s: status=%d body=%s", who, parent, read.Code, read.Body.String())
		}
		single := decodeBody[detail](t, read)
		if got := single.Progress.Children; got == nil || got.Done != children.done || got.Total != children.total {
			t.Errorf("%s reads %s's children as %+v, want %d done of %d", who, parent, got, children.done, children.total)
		}
		if len(single.Children) != children.total {
			t.Errorf("%s reads %s's children list as %+v, want %d children", who, parent, single.Children, children.total)
		}
		for _, listed := range single.Children {
			if listed.Key == open && (listed.SubtreeDone != subtree.done || listed.SubtreeTotal != subtree.total) {
				t.Errorf("%s reads %s's subtree as %d done of %d, want %d done of %d", who, open, listed.SubtreeDone, listed.SubtreeTotal, subtree.done, subtree.total)
			}
			if listed.Key == hiddenChild && children.total == 1 {
				t.Errorf("%s reads %s's children list naming %s", who, parent, hiddenChild)
			}
		}
		listing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project="+publicProject, nil, who)
		if listing.Code != http.StatusOK {
			t.Fatalf("%s lists %s: status=%d body=%s", who, publicProject, listing.Code, listing.Body.String())
		}
		found := false
		for _, summary := range decodeBody[model.IssueSummaryPage](t, listing).Issues {
			if summary.Key != parent {
				continue
			}
			found = true
			if got := summary.Progress.Children; got == nil || got.Done != children.done || got.Total != children.total {
				t.Errorf("%s lists %s's children as %+v, want %d done of %d", who, parent, got, children.done, children.total)
			}
		}
		if !found {
			t.Fatalf("%s lists %s without %s", who, publicProject, parent)
		}
	}
	check(accessCarol, count{1, 2}, count{1, 2})
	check("alice", count{0, 1}, count{0, 1})

	if _, err := database.Pool.Exec(t.Context(), `update projects set visibility = 'public' where key = $1`, privateProject); err != nil {
		t.Fatalf("make %s public: %v", privateProject, err)
	}
	check("alice", count{1, 2}, count{1, 2})
}
