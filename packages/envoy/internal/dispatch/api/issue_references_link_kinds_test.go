package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// GET /api/v1/issues/{key}/references lists a link only where it is a mention, or a typed
// block's pointer of a kind the installed block schema declares (refs.Closure): a decision's
// decided_by, which the server states to the person who wrote the decision and stores as a
// block_refs row to that person, is neither. An issue whose first document holds a decision citing
// another issue in its uncertainty_reason lists that issue by the decision's mention, and not
// alice, who wrote the decision, by its decided_by.
func TestTheIssueReferencesRouteListsNoDecidedBy(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	cited := createReferenceAPIIssue(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE",
		":::decision{#d1 uncertainty=\"low\" uncertainty_reason=\"dispatch://"+cited.Key+" may reopen it\"}\nWe chose REST.\n:::\n")

	var decided int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from block_refs r
		where `+refs.DocumentRoomBlocksSQL("r", "$1::uuid")+` and r.kind = $2 and r.to_kind = $3 and r.to_id = 'alice'
	`, issue.PrimaryArtifactID, graphstore.TupleDecidedBy, graphstore.KindPerson).Scan(&decided); err != nil {
		t.Fatalf("count the decided_by rows of %s's document: %v", issue.Key, err)
	}
	if decided != 1 {
		t.Fatalf("%s's document holds %d decided_by rows to person alice, want the one the server states for d1", issue.Key, decided)
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/references: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	var links []string
	for _, row := range decodeBody[model.IssueReferences](t, response).Links {
		links = append(links, row.Node.Kind+" "+row.Node.ID+" ["+strings.Join(row.Kinds, ",")+"]")
	}
	if want := []string{"issue " + cited.Key + " [mentions]"}; !slices.Equal(links, want) {
		t.Errorf("GET /api/v1/issues/%s/references lists the links %v, want %v: the decision's mention, and not its decided_by", issue.Key, links, want)
	}
}
