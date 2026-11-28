package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// criterionPageServes is the kind of the tuple a `serves` pointer states, out of the node that
// holds the pointer to each node it names.
const criterionPageServes = "serves"

// criterionPageItemBlocks is the item's own blocks read, GET /api/v1/comments/{id}/blocks or GET
// /api/v1/messages/{id}/blocks as alice: its current blocks in body order, each with its id and its
// reference. It fails unless the read answers the item indexed.
func criterionPageItemBlocks(t *testing.T, handler http.Handler, kind refs.ItemKind, id string) []model.ItemBlock {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/"+string(kind)+"s/"+id+"/blocks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the blocks of the %s %s: status=%d body=%s", kind, id, response.Code, response.Body.String())
	}
	read := decodeBody[model.ItemBlocks](t, response)
	if !read.Indexed {
		t.Fatalf("the blocks read of the %s %s answers it not indexed: %s", kind, id, response.Body.String())
	}
	return read.Blocks
}

// criterionPageParent is the parent read answers for the node of kind and id, as `<kind>:<id>`,
// "none" where it answers the node with none, and "absent" where it answers no such node.
func criterionPageParent(read treeAnswer, kind, id string) string {
	held := read.nodesNamed(kind, id)
	switch {
	case len(held) == 0:
		return "absent"
	case held[0].Parent == nil:
		return "none"
	}
	return held[0].Parent.Kind + ":" + held[0].Parent.ID
}

// criterionPagePlacement is the placement read answers: every node's parent by the node's
// `<kind>:<id>` (criterionPageParent), and every serves tuple, `<kind>:<id> serves <kind>:<id>`,
// sorted.
func criterionPagePlacement(read treeAnswer) (map[string]string, []string) {
	parents := make(map[string]string, len(read.Nodes))
	for _, node := range read.Nodes {
		parents[node.Kind+":"+node.ID] = criterionPageParent(read, node.Kind, node.ID)
	}
	var serves []string
	for _, tuple := range read.Tuples {
		if tuple.Kind == criterionPageServes {
			serves = append(serves, tuple.From.Kind+":"+tuple.From.ID+" serves "+tuple.To.Kind+":"+tuple.To.ID)
		}
	}
	slices.Sort(serves)
	return parents, serves
}

// criterionPageServing is the id of each node from which read answers a serves tuple to the node
// of kind and id, in the answer's order.
func criterionPageServing(read treeAnswer, kind, id string) []string {
	var from []string
	for _, tuple := range read.Tuples {
		if tuple.Kind == criterionPageServes && tuple.To == treeEnd(kind, id) {
			from = append(from, tuple.From.ID)
		}
	}
	return from
}

// criterionPageRefusal writes markdown at the end of the issue key's document slug through the
// document's edit route, as alice, and answers the status and the refusal the route sends.
func criterionPageRefusal(t *testing.T, handler http.Handler, key, slug, markdown string) (int, refusalAnswer, string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/"+slug+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}}, "summary": "a pointer outside its targets",
	}, "alice")
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	return response.Code, refusal, response.Body.String()
}
