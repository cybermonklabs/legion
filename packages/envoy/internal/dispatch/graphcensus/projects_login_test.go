package graphcensus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The census names a message's author party in SQL (authorParty), and the writer names it in Go
// (graphstore.AuthorOf, through model.CanonicalLogin). The two must give one party for every
// actor a row can store, or the census reads a message written by that person as differing from
// its own node. CanonicalLogin trims every Unicode white space and lowers the ASCII letters and
// nothing else, so the logins here are the ones a Unicode lower() or a space-only trim would
// spell otherwise: a KELVIN SIGN, a dotted capital I, a no-break space, an ideographic space, a
// tab and a plain mixed case, as a person's login, as a personal token's owner and as a session.
func TestTheCensusNamesAnAuthorAsTheWriterDoes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	owner := func(login string) *string { return &login }
	for _, actor := range []model.Actor{
		{Kind: "user", ID: "Alice"},
		{Kind: "user", ID: "Kelvin\u212a"},
		{Kind: "user", ID: "\u0130stanbul"},
		{Kind: "user", ID: "\u00a0bob@example.com\t"},
		{Kind: "user", ID: "\u3000Carol\u3000"},
		{Kind: "token", Owner: owner(" Dana\u00a0")},
		{Kind: "session", ID: "Session-1"},
	} {
		kind, id, named := graphstore.AuthorOf(actor)
		if !named {
			t.Fatalf("graphstore.AuthorOf names nobody for %+v, so this row proves nothing", actor)
		}
		want := kind + ":" + id
		stored, err := json.Marshal(actor)
		if err != nil {
			t.Fatalf("encode %+v: %v", actor, err)
		}
		var got string
		if err := database.Pool.QueryRow(ctx, `select `+authorParty("actor.value")+` from (select $1::jsonb as value) as actor`,
			string(stored)).Scan(&got); err != nil {
			t.Fatalf("name the author of %s in SQL: %v", stored, err)
		}
		if got != want {
			t.Errorf("the census names the author of %s as %q, and the writer names it %q", stored, got, want)
		}
	}
}
