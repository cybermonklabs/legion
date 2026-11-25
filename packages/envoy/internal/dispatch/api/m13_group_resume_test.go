package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// m13Team seeds group team, holding dave, on PRIV's list, and dave's token that lists PRIV, so dave
// at a browser and his token each see PRIV through the group alone; it answers the token's caller.
func m13Team(t *testing.T, seed privateSeed, dave string) privateCaller {
	t.Helper()
	const daveToken privateCaller = "dave's token that lists PRIV"
	seedPeople(t, seed.database, dave)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{dave}}, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/team", nil, http.StatusOK)
	mintListingToken(t, seed, daveToken, dave, []string{seedPrivate})
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(dave)); err != nil || !slices.Contains(reader.Viewer.Sees, seedPrivate) {
		t.Fatalf("dave, in team on PRIV's list, reads as %+v (%v), want a reader that sees PRIV", reader.Viewer, err)
	}
	return daveToken
}
