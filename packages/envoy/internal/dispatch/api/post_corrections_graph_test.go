// post_corrections_graph_test.go is what the reference graph says about a post its author
// withdrew. A node of a deleted comment, read on its own or as the other end of an edge, carries
// the delete mark that every other read of that post carries: the graph read gives its text to
// nobody, and an edge row with no text and no mark is a row a reader cannot account for.
package api

import (
	"net/http"
	"net/url"
	"testing"
)

// graphBody reads GET /api/v1/references as alice and gives the answer as it stands on the wire,
// so a field the typed model does not carry is still read here.
func graphBody(t *testing.T, handler http.Handler, query url.Values) map[string]any {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	return decodeBody[map[string]any](t, response)
}

// deletedMarkOf is the delete mark of one node of that answer, and ok reports whether the node
// carries one at all.
func deletedMarkOf(node map[string]any) (at any, ok bool) {
	at, present := node["deleted_at"]
	return at, present && at != nil
}

// The graph read of a deleted comment says that it is deleted, both when the comment is the node
// the query names and when it is the other end of an edge of a post that still stands. The reply
// keeps its own text and its own edge; what the delete takes is the root's words, and a reader
// of that edge is told why the row carries none.
func TestTheGraphNodeOfADeletedPostSaysItIsDeleted(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	root := createReferenceAPIComment(t, handler, issue.Key, "A root comment whose edge row a reader meets.")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "A reply under that root.", "reply_to": root.ID,
	}, "alice")

	// The control, before the delete: the edge row carries the root's own words, so a row with
	// no text is the delete's doing and not the graph's shape.
	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + reply.ID}})
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Excerpt == nil ||
		outgoing.Edges[0].Excerpt.Text != "A root comment whose edge row a reader meets." {
		t.Fatalf("the reply's edges before the delete = %#v, want one replies_to carrying the root's text", outgoing.Edges)
	}
	if node := graphBody(t, handler, url.Values{"to": {"dispatch://" + issue.Key + "/comment/" + root.ID}}); func() bool {
		target, _ := node["node"].(map[string]any)
		_, marked := deletedMarkOf(target)
		return marked
	}() {
		t.Fatalf("the root's node carries a delete mark before anyone deleted it: %#v", node["node"])
	}

	deleteThreadComment(t, handler, root.ID, "alice")

	named := graphBody(t, handler, url.Values{"to": {"dispatch://" + issue.Key + "/comment/" + root.ID}})
	target, _ := named["node"].(map[string]any)
	if _, marked := deletedMarkOf(target); !marked {
		t.Fatalf("the deleted comment's own graph node = %#v, want the delete mark every other read of it carries", target)
	}

	edges := graphBody(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + reply.ID}})
	rows, _ := edges["edges"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the reply's edges after the delete = %#v, want the one replies_to row", edges["edges"])
	}
	row, _ := rows[0].(map[string]any)
	other, _ := row["node"].(map[string]any)
	if _, marked := deletedMarkOf(other); !marked {
		t.Fatalf("the edge row naming the deleted comment = %#v, want the delete mark beside its empty excerpt", row)
	}
}
