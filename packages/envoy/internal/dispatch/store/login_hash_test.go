package store

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The statement model.LoginHash's comment gives an operator reads, from the people table, the one
// address a log line's login_hash stands for: each person's value finds that person's row alone,
// an address with a letter outside ASCII included (its hash is of the address's UTF-8 bytes on
// both sides), and a value of nobody's address finds no row.
func TestTheLoginHashResolveQueryFindsThePerson(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := []string{"ada@example.com", "zoë@example.com", "bob+dispatch@sub.example.org"}
	for _, email := range people {
		if _, err := database.Pool.Exec(ctx, `insert into people (email) values ($1)`, email); err != nil {
			t.Fatalf("insert %s: %v", email, err)
		}
	}
	resolve := func(value string) []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx,
			`select email from people where left(encode(sha256(convert_to(email, 'UTF8')), 'hex'), 16) = $1`, value)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var email string
			if err := rows.Scan(&email); err != nil {
				t.Fatal(err)
			}
			found = append(found, email)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, email := range people {
		if found := resolve(model.LoginHash(email)); len(found) != 1 || found[0] != email {
			t.Errorf("the resolve query for LoginHash(%q) = %s found %q, want [%s]", email, model.LoginHash(email), found, email)
		}
	}
	if found := resolve(model.LoginHash("nobody@example.com")); len(found) != 0 {
		t.Errorf("the resolve query for an address no row holds found %q, want none", found)
	}
}
