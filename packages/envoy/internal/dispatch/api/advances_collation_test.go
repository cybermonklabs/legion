// advances_collation_test.go reads the `advances` relation on a database whose collation does not
// order the priority ids as Go does. advancesScan.resolve merges an issue's own ids and the ids it
// inherits in one pass with Go's comparison of the bytes, and drops an id the issue holds both
// ways only where the two lists meet it in the same step. So the two lists must come back in that
// same byte order, which issueAdvancesTerms names as `collate "C"` rather than leaving to the
// database's default collation.
package api

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two ids of the measured case. `ci-fixes` sorts before `cicd` by the bytes, since `-` is
// below every letter, while a collation that weighs punctuation last, which is glibc's en_US,
// compares `cifixes` with `cicd` first and puts `cicd` ahead.
const (
	punctuatedPriority = "ci-fixes"
	letteredPriority   = "cicd"
)

// collatePriorityIDsAsADeploymentDoes holds the priority ids under a collation that sorts `cicd`
// before `ci-fixes`, which is what a deployment's database does: its collation is the one the
// cluster's Postgres was created with, glibc's en_US on the measured one. The test database's own
// default (ICU en-US on the pod's test server) orders these two ids as Go does, so a test left to
// it cannot see the defect; the ids are put under one known collation of the test's own, as
// cireconcile's collateMappingsAsADeploymentDoes puts the mapped repositories.
func collatePriorityIDsAsADeploymentDoes(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	// A PostgreSQL built with ICU carries ICU collations of its own and one built without ICU
	// carries none, so this count is the server's own answer to whether the collation below can
	// be made. An error from the read itself ends the test.
	var icu bool
	if err := database.Pool.QueryRow(ctx, `select count(*) > 0 from pg_collation where collprovider = 'i'`).Scan(&icu); err != nil {
		t.Fatalf("ask whether this Postgres was built with ICU: %v", err)
	}
	if !icu {
		t.Skip("a PostgreSQL built with ICU must serve this test: ICU is what holds the priority ids under one known collation weighing punctuation last, an order the Go merge of the advances lists does not follow")
	}
	// ICU's `ka-shifted` weighs punctuation only where the letters are equal, which is the rule
	// glibc's en_US was measured to follow. The walk orders and groups priority_tuples.to_id and
	// joins it to priority_nodes.id, so both take the collation: one of them alone would leave the
	// join comparing two columns of different collations. priority_tuples.from_id is left alone,
	// since it holds issue keys and the walk joins it to issues.key.
	for _, statement := range []string{
		`create collation punctuation_last (provider = icu, locale = 'en-US-u-ka-shifted')`,
		`alter table priority_tuples alter column to_id type text collate punctuation_last`,
		`alter table priority_nodes alter column id type text collate punctuation_last`,
	} {
		// The server has ICU, so a statement it refuses here is a permission or a schema problem
		// rather than an absence to pass over, and the test says so rather than skips.
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the priority ids under a deployment's own collation (%s): %v", statement, err)
		}
	}
	// The case only tells if the collation does put the two ids in the other order; a server
	// whose ICU answers otherwise would leave the reads below proving nothing.
	var lettersFirst bool
	if err := database.Pool.QueryRow(ctx, `select $1::text collate punctuation_last < $2::text collate punctuation_last`,
		letteredPriority, punctuatedPriority).Scan(&lettersFirst); err != nil {
		t.Fatalf("compare the two ids under the collation: %v", err)
	}
	if !lettersFirst {
		t.Fatalf("punctuation_last does not sort %q before %q, so it is not the order the defect turns on", letteredPriority, punctuatedPriority)
	}
}

// Every read of an issue's `advances` lists each priority once, own before inherited where it is
// both, in the byte order of the id, however the database collates the ids. The issue states
// `ci-fixes` and its parent states `ci-fixes` and `cicd`, so the issue holds `ci-fixes` both ways:
// under a collation weighing punctuation last its inherited list comes back `cicd, ci-fixes`,
// and a merge that trusted that order met `ci-fixes` twice and listed it twice. The issue is
// parked, so the parked fault of GET /api/v1/priorities reads it as well.
func TestAdvancesListEachPriorityOnceHoweverTheDatabaseCollatesTheIDs(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	collatePriorityIDsAsADeploymentDoes(t, database)
	createTestProject(t, handler, "COLL")
	for _, id := range []string{punctuatedPriority, letteredPriority} {
		createTestPriority(t, handler, map[string]any{"id": id, "title": id}, "alice")
	}
	parent := seedPriorityIssue(t, handler, "COLL", "The parent stating both", "", "",
		[]string{punctuatedPriority, letteredPriority})
	child := seedPriorityIssue(t, handler, "COLL", "The child stating one", "backlog", parent,
		[]string{punctuatedPriority})

	want := map[string][]string{
		parent: {punctuatedPriority + " own", letteredPriority + " own"},
		child:  {punctuatedPriority + " own", letteredPriority + " inherited"},
	}
	for key, entries := range want {
		if got := advanceEntries(readTestIssue(t, handler, key).Advances); !slices.Equal(got, entries) {
			t.Errorf("the read of %s says it advances %v, want %v", key, got, entries)
		}
	}
	for _, target := range []string{
		"/api/v1/issues?advances=any",
		"/api/v1/issues?advances=" + punctuatedPriority,
		"/api/v1/issues?advances=" + letteredPriority,
	} {
		for key, entries := range want {
			if got := advanceEntries(listedIssue(t, handler, target, key).Advances); !slices.Equal(got, entries) {
				t.Errorf("%s says %s advances %v, want %v", target, key, got, entries)
			}
		}
	}
	body := readPriorities(t, handler, "")
	if got, entries := advanceEntries(parkedRow(t, body, child).Advances), want[child]; !slices.Equal(got, entries) {
		t.Errorf("the parked list says %s advances %v, want %v", child, got, entries)
	}
}
