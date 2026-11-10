package api

import (
	"net/http"
	"testing"
)

// requirementBlock is one `requirement` block declaring the two attributes this route filters on.
// An empty value is written as no attribute at all, which is how a document that declares none
// reaches the index.
func requirementBlock(id, target, status, words string) string {
	opening := ":::requirement{#" + id
	if target != "" {
		opening += " target=\"" + target + "\""
	}
	if status != "" {
		opening += " status=\"" + status + "\""
	}
	return opening + "}\n" + words + "\n:::\n"
}

// readRequirements calls the route and fails on any status but 200.
func readRequirements(t *testing.T, handler http.Handler, query string) listRequirementsResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/requirements?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list the requirements of %q: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[listRequirementsResponse](t, response)
}

// blockIDs is the answer's block ids, for comparing a result set without depending on its order
// beyond what the route promises.
func blockIDs(answer listRequirementsResponse) []string {
	ids := make([]string, 0, len(answer.Requirements))
	for _, line := range answer.Requirements {
		ids = append(ids, line.BlockID)
	}
	return ids
}

func holdsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The two reads the spec loop needs: every requirement about one target, and every requirement at
// one status, each answering the matching blocks and no others. A requirement that declares
// neither attribute is in no answer, which is what the partial indexes are partial on.
func TestRequirementsListByTargetAndByStatus(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	spec := requirementBlock("r1", "Firmware.TlbRt.fencedR", "met", "Restate the fence receipt.") +
		requirementBlock("r2", "Firmware.TlbRt.fencedR", "open", "State the covering entry.") +
		requirementBlock("r3", "Firmware.IpiRt.takeV", "met", "State the take receipt.") +
		requirementBlock("r4", "", "", "A requirement that declares neither attribute.")
	createInteractionIssue(t, handler, "REQ", "Requirements", spec)

	byTarget := readRequirements(t, handler, "target=Firmware.TlbRt.fencedR")
	ids := blockIDs(byTarget)
	if len(ids) != 2 || !holdsID(ids, "r1") || !holdsID(ids, "r2") {
		t.Fatalf("by target = %v, want exactly r1 and r2", ids)
	}
	if byTarget.Total != 2 {
		t.Fatalf("by target total = %d, want 2", byTarget.Total)
	}

	byStatus := readRequirements(t, handler, "status=met")
	ids = blockIDs(byStatus)
	if len(ids) != 2 || !holdsID(ids, "r1") || !holdsID(ids, "r3") {
		t.Fatalf("by status = %v, want exactly r1 and r3", ids)
	}

	both := readRequirements(t, handler, "target=Firmware.TlbRt.fencedR&status=met")
	if ids = blockIDs(both); len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("by target and status = %v, want r1 alone", ids)
	}

	for _, query := range []string{"target=Firmware.TlbRt.fencedR", "status=met", "status=open"} {
		if holdsID(blockIDs(readRequirements(t, handler, query)), "r4") {
			t.Fatalf("%q answered r4, which declares neither attribute", query)
		}
	}

	line := byStatus.Requirements[0]
	if line.Issue == "" || line.ArtifactID == "" || line.Ref == "" || line.Excerpt == "" {
		t.Fatalf("a row is missing where it lives or its words: %+v", line)
	}
}

// A request naming neither attribute is refused rather than answered with the whole corpus: the
// route exists because the unfiltered read is the expensive one.
func TestRequirementsWithNoFilterIsRefused(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createInteractionIssue(t, handler, "REQN", "Requirements",
		requirementBlock("r1", "Firmware.TlbRt.fencedR", "met", "Restate the fence receipt."))

	for _, query := range []string{"", "limit=10", "offset=0"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/requirements?"+query, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET /api/v1/requirements?%s = %d, want 400: %s", query, response.Code, response.Body.String())
		}
	}
}

// The index follows the document. A later version of the requirement must be the one the route
// answers with: the index row is rewritten on every accepted write, and a reader served the old
// words is reading an index that has drifted from the document it indexes.
func TestRequirementsFollowAnEditedRequirement(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "REQE", "Requirements",
		requirementBlock("r1", "Firmware.TlbRt.fencedR", "open", "State the covering entry."))

	before := readRequirements(t, handler, "status=open")
	if ids := blockIDs(before); len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("before the edit, status=open = %v, want r1", ids)
	}

	edited := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": "State the covering entry.", "with": "State the covering entry and its fence."}},
		}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the requirement: status=%d body=%s", edited.Code, edited.Body.String())
	}

	after := readRequirements(t, handler, "status=open")
	if ids := blockIDs(after); len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("after the edit, status=open = %v, want r1 still", ids)
	}
	if after.Requirements[0].Excerpt == before.Requirements[0].Excerpt {
		t.Fatalf("the excerpt did not follow the edit: still %q, so the index row was not rewritten",
			after.Requirements[0].Excerpt)
	}
	if after.Requirements[0].Target != "Firmware.TlbRt.fencedR" || after.Requirements[0].Status != "open" {
		t.Fatalf("the edit lost the attributes: %+v", after.Requirements[0])
	}
}

// One page at a time, with the total counting every match rather than the page.
func TestRequirementsPages(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	spec := ""
	for _, id := range []string{"r1", "r2", "r3"} {
		spec += requirementBlock(id, "Firmware.TlbRt.fencedR", "open", "Work to do in "+id+".")
	}
	createInteractionIssue(t, handler, "REQP", "Requirements", spec)

	page := readRequirements(t, handler, "target=Firmware.TlbRt.fencedR&limit=2")
	if len(page.Requirements) != 2 || page.Total != 3 || page.Limit != 2 || page.Offset != 0 {
		t.Fatalf("first page = %d rows, total %d, limit %d, offset %d; want 2, 3, 2, 0",
			len(page.Requirements), page.Total, page.Limit, page.Offset)
	}
	rest := readRequirements(t, handler, "target=Firmware.TlbRt.fencedR&limit=2&offset=2")
	if len(rest.Requirements) != 1 || rest.Total != 3 {
		t.Fatalf("second page = %d rows, total %d; want 1 and 3", len(rest.Requirements), rest.Total)
	}

	response := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/requirements?target=Firmware.TlbRt.fencedR&limit=0", nil, "alice")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400: %s", response.Code, response.Body.String())
	}
}
