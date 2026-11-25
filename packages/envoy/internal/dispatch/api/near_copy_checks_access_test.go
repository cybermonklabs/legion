package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheNearCopyChecksOfAPrivateProjectAnswerAsAProjectNobodyHolds: PRIV's create leaves a
// near-copy check, which carol, who sees PRIV, reads in PRIV's page and by its id. alice, who does
// not, reads PRIV's page as the page of a project nobody holds, empty, and the row by its id as
// an id nobody holds, 404 NEAR_COPY_CHECK_NOT_FOUND, each byte for byte with its id substituted.
//
// Control: the page and the row read with no project clause, and alice reads PRIV's row.
func TestTheNearCopyChecksOfAPrivateProjectAnswerAsAProjectNobodyHolds(t *testing.T) {
	seed := seedPrivateProject(t)
	var id string
	if err := seed.database.Pool.QueryRow(context.Background(), `
		select id::text from near_copy_checks where project_key = $1 order by created_at desc, id desc limit 1
	`, seedPrivate).Scan(&id); err != nil {
		t.Fatalf("read the near-copy check of PRIV's create: %v", err)
	}
	const nobody = "00000000-0000-4000-8000-0000000000ff"

	page := decodeBody[model.NearCopyChecksPage](t, seed.mustRequest(t, callerCarol, http.MethodGet,
		"/api/v1/near-copy-checks?project="+seedPrivate, nil, http.StatusOK))
	if len(page.Checks) == 0 {
		t.Fatalf("carol reads no near-copy check of PRIV, so the comparison proves nothing")
	}
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/near-copy-checks/"+id, nil, http.StatusOK)

	hidden := seed.request(t, callerAlice, http.MethodGet, "/api/v1/near-copy-checks?project="+seedPrivate, nil)
	missing := seed.request(t, callerAlice, http.MethodGet, "/api/v1/near-copy-checks?project=NOPE", nil)
	wantPage := strings.ReplaceAll(missing.Body.String(), `"NOPE"`, `"`+seedPrivate+`"`)
	if hidden.Code != missing.Code || hidden.Body.String() != wantPage {
		t.Errorf("alice reads PRIV's near-copy checks as %d %s, want what a project nobody holds answers, %d %s",
			hidden.Code, hidden.Body.String(), missing.Code, wantPage)
	}
	row := seed.request(t, callerAlice, http.MethodGet, "/api/v1/near-copy-checks/"+id, nil)
	absent := seed.request(t, callerAlice, http.MethodGet, "/api/v1/near-copy-checks/"+nobody, nil)
	wantRow := strings.ReplaceAll(absent.Body.String(), nobody, id)
	if absent.Code != http.StatusNotFound || row.Code != absent.Code || row.Body.String() != wantRow {
		t.Errorf("alice reads PRIV's near-copy check %s as %d %s, want what an id nobody holds answers, %d %s",
			id, row.Code, row.Body.String(), absent.Code, wantRow)
	}
}
