// advances_collation_test.go reads the `advances` relation on a database whose collation does not
// order the priority ids as Go does. advancesScan.resolve merges an issue's own ids and the ids it
// inherits in one pass with Go's comparison of the bytes, and drops an id the issue holds both
// ways only where the two lists meet it in the same step. So the two lists must come back in that
// same byte order, which issueAdvancesTerms names as `collate "C"` rather than leaving to the
// database's default collation.
package api

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two ids of the measured case. `ci-fixes` sorts before `cicd` by the bytes, since `-` is
// below every letter, while a collation that weighs punctuation last, which is glibc's en_US,
// compares `cifixes` with `cicd` first and puts `cicd` ahead.
const (
	punctuatedPriority = "ci-fixes"
	letteredPriority   = "cicd"
	// originPriority is one only the child in backlog states, so the child is its origin, and so
	// a fault of priority work in backlog or icebox: no issue above it carries it.
	originPriority = "docs"
)

// collatedAsADeploymentDoes answers a database of this tree's migrations whose own default
// collation sorts `cicd` before `ci-fixes`, which is what a deployment's database does: its
// collation is the one the cluster's Postgres was created with, glibc's en_US on the measured one.
// The test database's own default (ICU en-US on the pod's test server) orders these two ids as Go
// does, so a test left to it cannot see the defect. ICU's `ka-shifted` weighs punctuation only
// where the letters are equal, which is the rule glibc's en_US was measured to follow, so the
// database is created with that ICU locale as its default and migrated by this tree's runner.
//
// The whole database takes the collation, as a deployment's does, rather than one column: the
// priority ids are the far ends of `advances` tuples in graph_tuples, a column every walk of the
// store reads, and a walk whose recursive term reads a column of one collation and whose first
// term a text of the default's is one Postgres refuses (a recursive query's column takes one
// collation), a refusal no deployment meets. The database is dropped once the test ends.
func collatedAsADeploymentDoes(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	host := storetest.Open(t)
	// A PostgreSQL built with ICU carries ICU collations of its own and one built without ICU
	// carries none, so this count is the server's own answer to whether the database below can
	// be made. An error from the read itself ends the test.
	var icu bool
	if err := host.Pool.QueryRow(ctx, `select count(*) > 0 from pg_collation where collprovider = 'i'`).Scan(&icu); err != nil {
		t.Fatalf("ask whether this Postgres was built with ICU: %v", err)
	}
	if !icu {
		t.Skip("a PostgreSQL built with ICU must serve this test: ICU is what holds the priority ids under one known collation weighing punctuation last, an order the Go merge of the advances lists does not follow")
	}
	name := fmt.Sprintf("dispatch_test_collated_%d", time.Now().UnixNano())
	// The server has ICU, so a statement it refuses here is a permission or a schema problem
	// rather than an absence to pass over, and the test says so rather than skips.
	if _, err := host.Pool.Exec(ctx, `create database `+name+` template template0 encoding 'UTF8'
		locale_provider icu icu_locale 'en-US-u-ka-shifted' locale 'C'`); err != nil {
		t.Fatalf("create a database collated as a deployment's is: %v", err)
	}
	t.Cleanup(func() {
		if _, err := host.Pool.Exec(context.Background(), `drop database if exists `+name+` with (force)`); err != nil {
			t.Errorf("drop the collated database %s: %v", name, err)
		}
	})
	address, err := url.Parse(host.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("read the test database's address: %v", err)
	}
	address.Path = "/" + name
	database, err := store.Open(ctx, address.String())
	if err != nil {
		t.Fatalf("open the collated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the collated database: %v", err)
	}
	// The case only tells if the collation does put the two ids in the other order; a server
	// whose ICU answers otherwise would leave the reads below proving nothing.
	var lettersFirst bool
	if err := database.Pool.QueryRow(ctx, `select $1::text < $2::text`, letteredPriority, punctuatedPriority).Scan(&lettersFirst); err != nil {
		t.Fatalf("compare the two ids under the database's collation: %v", err)
	}
	if !lettersFirst {
		t.Fatalf("the collated database does not sort %q before %q, so it is not the order the defect turns on", letteredPriority, punctuatedPriority)
	}
	return database
}

// Every read of an issue's `advances` lists each priority once, own before inherited where it is
// both, in the byte order of the id, however the database collates the ids. The issue states
// `ci-fixes` and `docs`, and its parent states `ci-fixes` and `cicd`, so the issue holds `ci-fixes`
// both ways: under a collation weighing punctuation last its inherited list comes back
// `cicd, ci-fixes`, and a merge that trusted that order met `ci-fixes` twice and listed it twice.
// The issue stands in backlog and is the origin of `docs`, which no issue above it carries, so the
// second fault of GET /api/v1/priorities, priority work in backlog or icebox, reads it as well.
func TestAdvancesListEachPriorityOnceHoweverTheDatabaseCollatesTheIDs(t *testing.T) {
	t.Parallel()
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: collatedAsADeploymentDoes(t)})
	createTestProject(t, handler, "COLL")
	for _, id := range []string{punctuatedPriority, letteredPriority, originPriority} {
		createTestPriority(t, handler, map[string]any{"id": id, "title": id}, "alice")
	}
	parent := seedPriorityIssue(t, handler, "COLL", "The parent stating both", "", "",
		[]string{punctuatedPriority, letteredPriority})
	child := seedPriorityIssue(t, handler, "COLL", "The child stating two", "backlog", parent,
		[]string{punctuatedPriority, originPriority})

	want := map[string][]string{
		parent: {punctuatedPriority + " own", letteredPriority + " own"},
		child:  {punctuatedPriority + " own", letteredPriority + " inherited", originPriority + " own"},
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
	if got, entries := advanceEntries(backlogRow(t, body, child).Advances), want[child]; !slices.Equal(got, entries) {
		t.Errorf("the backlog-or-icebox list says %s advances %v, want %v", child, got, entries)
	}
}
