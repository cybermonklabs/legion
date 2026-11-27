package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// claimsIssueVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimsIssueVersion = 20261126035722

// 20261126035722 adds claims_issue and claims_project, btree indexes of claims on issue and on
// project, the columns the reads of an admission pass's claims go by (record.Store.SessionClaimsOf,
// ArchitectLaunchesOf, ArchitectServedOf, RoleHoldersAt, RoleHolders, IssuePodUID and
// ProjectClaims), and leaves every claim the table holds as it stands. It builds over a claim whose
// session is 3,200 bytes of incompressible text, past the size of a btree row, which an index that
// holds session too cannot be built over. The negative control is the store before the migration,
// whose claims carry neither index, and whose claims_tree (20261125212800) is the only index of the
// table beside its key and claims_boot_token_hash.
func TestTheClaimsOfAnIssueAndOfAProjectAreIndexedFrom20261126035722(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimsIssueVersion-1)
	const indexes = `select indexname from pg_indexes where tablename = 'claims' order by indexname`
	if got, want := rowsAsText(t, store, indexes), []string{"claims_boot_token_hash", "claims_pkey", "claims_tree"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the indexes of claims before the migration = %v, want %v", got, want)
	}
	long := incompressible(3200)
	for _, claim := range []struct{ token, session string }{{"claim-208", "ses-208"}, {"claim-209", long}} {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'architect', 0, $2, '', 'working', 0, 0, 0, 0)`, claim.token, claim.session); err != nil {
			t.Fatalf("seed claim %s: %v", claim.token, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claims indexes over a claim whose session is %d bytes: %v", len(long), err)
	}
	const definitions = `select indexdef from pg_indexes where tablename = 'claims' and indexname in ('claims_issue', 'claims_project') order by indexname`
	got := rowsAsText(t, store, definitions)
	if len(got) != 2 || !strings.Contains(got[0], "USING btree (issue)") || !strings.Contains(got[1], "USING btree (project)") {
		t.Fatalf("claims_issue and claims_project after the migration = %v, want one btree index on (issue) and one on (project)", got)
	}
	const claims = `select row(token, project, issue, length(session))::text from claims order by token`
	if got, want := rowsAsText(t, store, claims), []string{"(claim-208,legion,LEGION-208,7)", "(claim-209,legion,LEGION-208,3200)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (token, project, issue, session length) = %v, want %v: the claims it found, as they stood", got, want)
	}
}
