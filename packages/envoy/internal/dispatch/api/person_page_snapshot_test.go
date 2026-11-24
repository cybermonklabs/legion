package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// TestAPersonPageDecidesAndListsInOneSnapshot holds the person page to deciding 200 or 404 and
// listing its rows in one snapshot (listPersonNodes). alice writes one comment through the
// route, project-visible, which is the one node of hers bob may see, and bob reads her page.
// Between the route's decision and its list (server.betweenPersonPageReads), the test commits a
// write on another connection that reserves that comment for alice, as a later release's
// author-private write marks one: the store's own writer (graphstore.Write) moves the comment's
// node to visibility author and states that attribute beside it. The route decided on a snapshot
// in which bob may see the comment, so it answers 200 with that comment on its row, and never 200
// with no rows, which is what a list read outside the decision's snapshot answers. bob's next read
// of the page answers 404, which proves the write committed and took the comment out of his view.
func TestAPersonPageDecidesAndListsInOneSnapshot(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "SNAP", "A person's page", "# Spec\n\nA passage.\n")
	const person, reader = "alice", "bob"
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "Alice wrote this for everyone."}, person)
	if created.Code != http.StatusCreated {
		t.Fatalf("write %s's comment: status=%d body=%s", person, created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	page := "/api/v1/people/" + person + "/nodes"

	ctx := context.Background()
	reserved := 0
	served.betweenPersonPageReads = func() {
		reserved++
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			node, found, err := graphstore.ReadNode(ctx, tx, graphstore.KindComment, comment)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("the store holds no node of the comment")
			}
			node.Visibility = graphstore.VisibilityAuthor
			return graphstore.Write(ctx, tx, []graphstore.Node{node}, nil)
		}); err != nil {
			t.Errorf("reserve %s's comment %s for her between the route's two reads: %v", person, comment, err)
		}
	}
	answered := dispatchRequest(t, handler, http.MethodGet, page, nil, reader)
	served.betweenPersonPageReads = nil
	t.Logf("%s reads %s's page with her comment reserved for her between the two reads: %d %s",
		reader, person, answered.Code, answered.Body.String())
	if reserved != 1 {
		t.Fatalf("the route ran the seam between its two reads %d times, want once: %d %s",
			reserved, answered.Code, answered.Body.String())
	}
	if answered.Code != http.StatusOK {
		t.Fatalf("%s reads %s's page as %d %s, want 200: the route decides before the write, in a snapshot in which %s may see her comment %s",
			reader, person, answered.Code, answered.Body.String(), reader, comment)
	}
	read := decodeBody[personNodes](t, answered)
	if len(read.Rows) == 0 {
		t.Fatalf("%s reads %s's page as 200 with no rows (%s): the route decided on one snapshot that %s holds a node %s may see, and listed her rows on another, in which the write reserving her comment %s for her had committed",
			reader, person, answered.Body.String(), person, reader, comment)
	}
	if read.Person != person || read.Total != 1 || len(read.Rows) != 1 || read.Rows[0].ID != comment {
		t.Errorf("%s reads %s's page as %+v, want her one comment %s on its row, as the page stood when the route decided",
			reader, person, read, comment)
	}

	after := dispatchRequest(t, handler, http.MethodGet, page, nil, reader)
	if after.Code != http.StatusNotFound {
		t.Fatalf("once the write has committed, %s reads %s's page as %d %s, want 404: the write did not take her comment %s out of his view, so the read before it proved nothing",
			reader, person, after.Code, after.Body.String(), comment)
	}
}
