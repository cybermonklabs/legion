package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestARestrictedWritersShortIDResolvesOnlyAmongTheNodesOfProjectsTheySee: a short id a comment
// writes resolves for the person its row records, as that person sees now (refs.Reader.seenBy,
// access.Resolve). bob is a restricted member PRIV's list names and PUB's does not, so he sees no
// node of PUB, a public project. carol writes a comment on PUB's issue; bob and carol each write
// on PRIV's issue a comment naming its short id. carol's comment stores the PUB comment's whole id;
// bob's stores the short id as he wrote it, as for a comment nobody holds, since a resolution would
// tell him that a node of a project he does not see carries it.
//
// Control: seenBy resolving every writer as the server's reader, which sees every node; bob's
// comment then stores the whole id, and the test fails naming him.
func TestARestrictedWritersShortIDResolvesOnlyAmongTheNodesOfProjectsTheySee(t *testing.T) {
	seed := seedPrivateProject(t)
	target := decodeBody[struct {
		ID string `json:"id"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "A remark on PUB."}, http.StatusCreated)).ID
	short := target[:8]
	body := "See dispatch://" + seed.issue[seedPublic] + "/comment/" + short + "."
	// stored is the comment references a comment caller writes with body on PRIV's issue stores.
	stored := func(caller privateCaller) []string {
		t.Helper()
		written := decodeBody[struct {
			ID string `json:"id"`
		}](t, seed.mustRequest(t, caller, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
			map[string]any{"body": body}, http.StatusCreated)).ID
		rows, err := seed.database.Pool.Query(context.Background(), `
			select to_id from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'comment' order by to_id
		`, written)
		if err != nil {
			t.Fatalf("read the references of %s's comment: %v", caller, err)
		}
		cited, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the references of %s's comment: %v", caller, err)
		}
		return cited
	}
	if got := stored(callerCarol); !slices.Equal(got, []string{target}) {
		t.Fatalf("carol's comment naming PUB's comment by %s stores %v, want its whole id %s: the comparison proves nothing", short, got, target)
	}
	if got := stored(callerBob); !slices.Equal(got, []string{short}) {
		t.Errorf("bob's comment naming PUB's comment by %s stores %v, want the short id as he wrote it: he sees no node of PUB", short, got)
	}
}
