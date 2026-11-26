package api

import (
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
	handler, parent, child, parentTree, _, address := childCriterionUnderParent(t, "INTO")
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
