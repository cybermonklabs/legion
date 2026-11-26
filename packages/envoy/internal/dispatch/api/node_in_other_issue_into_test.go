package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// An insert addressed through a parent issue whose into anchor names, by its store address, a node
// of its child issue - a criterion written into the child's own room - is refused 409
// NODE_IN_OTHER_ISSUE naming the child, as an anchor's after, before and block are, and writes
// nothing (refuseNodesOfOtherIssues).
func TestAnInsertIntoAChildsNodeThroughTheParentIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createRoomProject(t, handler, "INTO")
	parent, parentTree := createRoomIssue(t, handler, database, "INTO", "The parent", "The parent's spec.\n", "")
	child, childTree := createRoomIssue(t, handler, database, "INTO", "The child", "The child's spec.\n", parent.Key)
	if parentTree == "" || childTree == "" {
		t.Fatalf("the parent's tree is %q and the child's %q, want both", parentTree, childTree)
	}
	written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+childTree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": ":::criterion{#k1}\nThe child's criterion.\n:::\n"}},
	}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("write the child's criterion: status=%d body=%s", written.Code, written.Body.String())
	}
	var address string
	if err := database.Pool.QueryRow(ctx, `
		select n.id from graph_nodes n
		where n.kind = 'block' and n.id = $1 || '#k1'`, child.Key+"/tree").Scan(&address); err != nil {
		t.Fatalf("read the child's criterion node: %v", err)
	}
	before := artifactTextOf(t, handler, parentTree)
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+parentTree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "into": "block:" + address, "markdown": "A paragraph inside the child's criterion.\n"}},
	}, "alice")
	refusal := decodeBody[refusalAnswer](t, refused)
	if refused.Code != http.StatusConflict || refusal.Code != codeNodeInOtherIssue || !strings.Contains(refusal.Error, child.Key) {
		t.Errorf("an insert through %s into %s answered %d %s %q, want 409 %s naming %s", parent.Key, address, refused.Code, refusal.Code, refusal.Error, codeNodeInOtherIssue, child.Key)
	}
	if after := artifactTextOf(t, handler, parentTree); after != before {
		t.Errorf("the parent's tree reads %q after the refused insert, want %q", after, before)
	}
}
