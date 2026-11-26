package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// claimsTreeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimsTreeVersion = 20261125212800

// 20261125212800 adds claims_tree, a btree index of claims on tree, the column
// record.Store.SessionClaimsTree and record.Store.ArchitectIssueOfSession read a session's claims
// in one tree by, and leaves every claim the table holds as it stands. It builds over a claim whose
// session is 3,200 bytes of incompressible text, past the size of a btree row, which an index that
// holds session too cannot be built over. The negative control is the store before the migration,
// whose claims carry no such index.
func TestTheClaimsOfATreeAreIndexedFrom20261125212800(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimsTreeVersion-1)
	const index = `select indexdef from pg_indexes where tablename = 'claims' and indexname = 'claims_tree'`
	if got := rowsAsText(t, store, index); len(got) != 0 {
		t.Fatalf("claims_tree before the migration = %v, want none", got)
	}
	long := incompressible(3200)
	for _, claim := range []struct{ token, session string }{{"claim-208", "ses-208"}, {"claim-209", long}} {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', 'LEGION-208', 'LEGION-208', 'architect', 0, $2, '', 'working', 0, 0, 0, 0)`, claim.token, claim.session); err != nil {
			t.Fatalf("seed claim %s: %v", claim.token, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claims index over a claim whose session is %d bytes: %v", len(long), err)
	}
	got := rowsAsText(t, store, index)
	if len(got) != 1 || !strings.Contains(got[0], "USING btree (tree)") {
		t.Fatalf("claims_tree after the migration = %v, want one btree index on (tree)", got)
	}
	const claims = `select row(token, tree, length(session))::text from claims order by token`
	if got, want := rowsAsText(t, store, claims), []string{"(claim-208,LEGION-208,7)", "(claim-209,LEGION-208,3200)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (token, tree, session length) = %v, want %v: the claims it found, as they stood", got, want)
	}
}
