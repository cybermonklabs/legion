package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-search-key's read of R17's window owner rule: this build's search by meaning reads the windows
// of a block's current owner alone, never those its old owner key still holds.

// searchKeyOldOwnerQuery is a word no text of TestSearchFollowsABlockThatChangesOwner holds, which
// its fake embedder maps to searchKeyOldOwnerVector: a search for it can answer by meaning alone.
const searchKeyOldOwnerQuery = "quillenfrost"

// searchKeyOldOwnerVector is the vector the old owner's windows are written at for the search that
// they alone match: the unit vector of axis 5, orthogonal to every other vector the test embeds -
// queryVector, the windows' angledVector(0.9, 2), the issue's angledVector(0, 3) and the default.
func searchKeyOldOwnerVector() []float32 { return angledVector(0, 5) }

// searchKeyWindowOwners is the owner key of every document window the store holds, each once, in
// order: what owners a writer of windows has written windows under so far.
func searchKeyWindowOwners(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `select distinct split_part(id, '#', 1) from embeddings where kind = $1 order by 1`, refs.DocumentWindowKind)
	if err != nil {
		t.Fatalf("read the owners of the document windows: %v", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the owners of the document windows: %v", err)
	}
	return owners
}

// requireOldOwnerWindowsUnread writes each of the windows the old owner key owner still holds, as
// many as windows, at searchKeyOldOwnerVector, and searches by meaning for searchKeyOldOwnerQuery,
// which no keyword matches and which those windows alone match, the windows under the issue's tree
// node tree standing orthogonal to it. It fails unless the search route answers no hit of the
// document: this build's search reads the windows of the current owner alone (R17), and a build
// that read every owner's windows would answer the moved block movedBlock from its old owner's key
// as well as from its window under the tree, twice where it is answered once.
func requireOldOwnerWindowsUnread(t *testing.T, handler http.Handler, database *store.Store, owner, tree, document, movedBlock string, windows int) {
	t.Helper()
	tag, err := database.Pool.Exec(t.Context(), `update embeddings set embedding = $3::vector where kind = $1 and split_part(id, '#', 1) = $2`,
		refs.DocumentWindowKind, owner, embed.Literal(searchKeyOldOwnerVector()))
	if err != nil || tag.RowsAffected() != int64(windows) {
		t.Fatalf("write the %d windows of the old owner %s at the vector the search matches: %v (%d rows)", windows, owner, err, tag.RowsAffected())
	}
	requireNoKeywordHit(t, handler, "q="+searchKeyOldOwnerQuery)
	response := searchResponse(t, handler, "q="+url.QueryEscape(searchKeyOldOwnerQuery))
	if response.Degraded != "" {
		t.Fatalf("a search by meaning for %q is degraded (%q), want it read by meaning", searchKeyOldOwnerQuery, response.Degraded)
	}
	for _, hit := range searchKeyHits(response) {
		if hit.kind == "document" && hit.id == document {
			t.Errorf("a search by meaning that only the windows of the old owner %s match answers the document %s at the block %q (%+v), want no hit: this build reads the windows of the current owner %s alone, so the moved block %s answers once, not once under each owner key",
				owner, document, hit.block, response.Results, tree, movedBlock)
		}
	}
}
