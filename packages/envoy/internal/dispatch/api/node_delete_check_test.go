package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-node-delete's own helpers.

// holdDeleteVersions reads, at the start of the first query drain of any of issues
// (docs.Service.HoldDrains), each of documents' version count and newest markdown: the versions the
// delete wrote, apart from the drain's. Its function takes the hold off and answers them.
func holdDeleteVersions(t *testing.T, service *docs.Service, database *store.Store, documents map[string]string, issues ...string) func() (map[string]int, map[string]string) {
	t.Helper()
	var once sync.Once
	versions, texts := map[string]int{}, map[string]string{}
	read := func() {
		for name, id := range documents {
			versions[name], texts[name] = versionCount(t, database, id), newestQueryVersion(t, database, id).markdown
		}
	}
	service.HoldDrains(func(issueKey string) {
		if slices.Contains(issues, issueKey) {
			once.Do(read)
		}
	})
	t.Cleanup(func() { service.HoldDrains(nil) })
	return func() (map[string]int, map[string]string) {
		service.HoldDrains(nil)
		once.Do(read)
		return versions, texts
	}
}

// emptyCallouts is the id of each callout markdown holds with no content.
func emptyCallouts(t *testing.T, markdown string) []string {
	t.Helper()
	parsed, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse a version's markdown: %v\n%s", err, markdown)
	}
	var empty []string
	pmdoc.Walk(parsed, func(node *pmdoc.Node) bool {
		if node.Type == "callout" && len(node.Children) == 0 {
			empty = append(empty, fmt.Sprint(node.Attrs["id"]))
		}
		return true
	})
	return empty
}

// placementValueOf is the newest placement value the block node address holds, "" for none.
func placementValueOf(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	var placement string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select to_id from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and asserted
		                 order by seq desc limit 1), '')`, refs.BlockNodeKind, address, refs.AttributePlacement, graphstore.KindValue).Scan(&placement); err != nil {
		t.Fatalf("read the placement of %s: %v", address, err)
	}
	return placement
}

// requireNoEventTuples fails unless every tuple out of the node kind and id past the sequence after
// stands at graphstore.NoEvent, as the node routes write their tuples, and one does.
func requireNoEventTuples(t *testing.T, database *store.Store, what, kind, id string, after int64) {
	t.Helper()
	var written, evented int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where source_event <> $4) from graph_tuples where from_kind = $1 and from_id = $2 and seq > $3`,
		kind, id, after, graphstore.NoEvent).Scan(&written, &evented); err != nil {
		t.Fatalf("read the tuples %s wrote out of %s %s: %v", what, kind, id, err)
	}
	if written == 0 || evented != 0 {
		t.Errorf("%s wrote %d tuples out of %s %s, %d of them under an event, want them written at graphstore.NoEvent", what, written, kind, id, evented)
	}
}

// requireCoverageChecks fails unless the coverage read of issueKey counts len(checks) checks under
// the requirement blockID and lists each of checks among them, by its node, and not never.
func requireCoverageChecks(t *testing.T, handler http.Handler, issueKey, blockID, never string, checks ...string) {
	t.Helper()
	requirement := requirementOf(t, readCoverage(t, handler, issueKey), blockID)
	listed := make([]string, len(requirement.Proof))
	for index, proof := range requirement.Proof {
		listed[index] = proof.Check
	}
	missing := slices.ContainsFunc(checks, func(check string) bool { return !slices.Contains(listed, check) })
	if requirement.Checks != len(checks) || missing || slices.Contains(listed, never) {
		t.Errorf("%s's coverage counts %d checks and lists %v after the delete, want %v and not %s", blockID, requirement.Checks, listed, checks, never)
	}
}

// anchorWords is a comment as the comments read answers it with node_anchor=true, by value.
func anchorWords(comment anchoredComment) string {
	words := "{id:" + comment.ID
	if comment.Anchor != nil {
		words += fmt.Sprintf(" anchor:{orphaned:%t quote:%q}", comment.Anchor.Orphaned, comment.Anchor.Quote)
	}
	if anchor := comment.NodeAnchor; anchor != nil {
		words += fmt.Sprintf(" node_anchor:{node:%s state:%s orphaned:%t quote:%q}", anchor.Node.ID, anchor.Node.State, anchor.Orphaned, anchor.Quote)
	}
	return words + "}"
}

// nodeTagOf is the If-Match of the node a store address names: its current part_of's seq, else its
// child_of's; `"1"` for a node holding neither, which no tag matches (matchesPlacement).
func nodeTagOf(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refsKindOf(address), storeIDOf(address), []string{graphstore.TuplePartOf, graphstore.TupleChildOf})
	if err != nil {
		t.Fatalf("read the placement of %s: %v", address, err)
	}
	for _, kind := range []string{graphstore.TuplePartOf, graphstore.TupleChildOf} {
		if at := slices.IndexFunc(held, func(tuple graphstore.Tuple) bool { return tuple.Kind == kind }); at >= 0 {
			return `"` + strconv.FormatInt(held[at].Seq, 10) + `"`
		}
	}
	return `"1"`
}

// requireSameRefusal fails unless the dry run's answer and the write's to the same delete hold the
// same status, the same code and the same visible words.
func requireSameRefusal(t *testing.T, what string, dry, write *httptest.ResponseRecorder) {
	t.Helper()
	dryRefusal, writeRefusal := decodeBody[refusalAnswer](t, dry), decodeBody[refusalAnswer](t, write)
	if dry.Code != write.Code || dryRefusal != writeRefusal {
		t.Errorf("%s: the dry run answered %d %+v and the write %d %+v, want the same status, code and words", what, dry.Code, dryRefusal, write.Code, writeRefusal)
	}
}

// holdWriteLocks opens a transaction of the test's own holding the write locks of issues a delete
// takes, room's: each issue's owner lock, else the lock of each issue's room.
func holdWriteLocks(t *testing.T, database *store.Store, room bool, issues []string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of the write locks of %v: %v", issues, err)
	}
	if room {
		_, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t where t.issue_key = any($1)`, issues)
	}
	for _, issue := range issues {
		if !room {
			err = errors.Join(err, store.LockOwner(ctx, tx, store.IssueOwner(issue)))
		}
	}
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("take the write locks of %v (rooms %t): %v", issues, room, err)
	}
	return tx
}

// waitingIn is the statement each backend of the database that waits on a lock is running.
func waitingIn(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select left(query, 240) from pg_stat_activity where wait_event_type = 'Lock' and datname = current_database()`)
	if err == nil {
		var waiting []string
		if waiting, err = pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
			return waiting
		}
	}
	t.Fatalf("read the backends that wait on a lock: %v", err)
	return nil
}

// requireDryRunTakesNoWriteLock fails unless dry, a delete's dry run under issues, answers while the
// test holds each issue's owner lock, and again each room's lock (holdWriteLocks).
func requireDryRunTakesNoWriteLock(t *testing.T, database *store.Store, what string, dry func() *httptest.ResponseRecorder, issues ...string) *httptest.ResponseRecorder {
	t.Helper()
	var response *httptest.ResponseRecorder
	for _, room := range []bool{false, true} {
		tx := holdWriteLocks(t, database, room, issues)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- dry() }()
		select {
		case response = <-answered:
		case <-time.After(10 * time.Second):
			t.Errorf("%s waited 10s on the write locks (rooms %t) of %v a transaction of the test holds, in %q: a dry run takes no write lock", what, room, issues, waitingIn(t, database))
			tx.Rollback(context.Background())
			response = <-answered
		}
		tx.Rollback(context.Background())
	}
	return response
}

// requireDeleteWaitsOnOwnerLock fails unless write, a delete under issues, waits in the exclusive
// owner lock while the test holds the issues' owner locks; it then lets them go and answers.
func requireDeleteWaitsOnOwnerLock(t *testing.T, database *store.Store, what string, write func() *httptest.ResponseRecorder, issues ...string) *httptest.ResponseRecorder {
	t.Helper()
	tx := holdWriteLocks(t, database, false, issues)
	defer tx.Rollback(context.Background())
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- write() }()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case response := <-answered:
			t.Errorf("%s answered %d while a transaction of the test held the owner locks of %v, want it waiting in the owner lock the delete takes", what, response.Code, issues)
			return response
		default:
		}
		if slices.ContainsFunc(waitingIn(t, database), func(query string) bool { return strings.Contains(query, "owner_lock('exclusive'") }) {
			tx.Rollback(context.Background())
			return <-answered
		}
	}
	t.Errorf("%s did not wait in an owner lock within 20s while the test held the owner locks of %v", what, issues)
	tx.Rollback(context.Background())
	return <-answered
}
