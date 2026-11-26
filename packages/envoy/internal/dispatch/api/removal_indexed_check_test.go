package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// removalIndexedColumns are the five columns whose foreign key to artifacts (id) no plain index
// led before this issue's work, each now led by an index of its own (R7 of this issue's spec):
// asks.block_artifact_id, whose asks_block_id_unique holds only the rows where block_id is not
// null; related_checks.source_artifact_id; user_document_reads.artifact_id, whose primary key leads
// login; write_suggestions.source_artifact_id, whose write_suggestions_pending_artifact holds only
// the rows where outcome = 'ignored'; and issue_room_blocks.document_id. Without such an index the
// check of each key, which the delete of a document's row runs under the issue's owner lock, reads
// its whole table.
var removalIndexedColumns = []artifactColumn{
	{table: "asks", column: "block_artifact_id"},
	{table: "related_checks", column: "source_artifact_id"},
	{table: "user_document_reads", column: "artifact_id"},
	{table: "write_suggestions", column: "source_artifact_id"},
	{table: "issue_room_blocks", column: "document_id"},
}

// removalIndexedLockTimeout is the longest lock_timeout the removal's transaction may set: it sets
// one of the code's two values, '200ms' (embeddingProjectKeysFilledLockWait) or '5s' (store's
// repoRenameLockTimeout), and the test bounds its wait by the longer, so the bound holds whichever
// it sets. removalIndexedSlack is what the test allows past it for the refusal to reach its answer.
const (
	removalIndexedLockTimeout = 5 * time.Second
	removalIndexedSlack       = 5 * time.Second
)

// removalIndexedKey is one foreign key that references artifacts (id), as the delete of a row of
// artifacts checks it: its name, its table, its columns in key order with the type of the column of
// artifacts each references, the attribute number of its first column, and what it does on that
// delete (pg_constraint.confdeltype).
type removalIndexedKey struct {
	name, table    string
	columns, types []string
	first          int16
	action         string
}

// removalIndexedKeys is every foreign key that references artifacts (id), read from pg_constraint.
func removalIndexedKeys(t *testing.T, database *store.Store) []removalIndexedKey {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select k.conname::text, k.conrelid::regclass::text, k.confdeltype::text, k.conkey[1],
		       array(select a.attname::text from unnest(k.conkey) with ordinality as c (attnum, n)
		             join pg_attribute a on a.attrelid = k.conrelid and a.attnum = c.attnum order by c.n),
		       array(select format_type(a.atttypid, a.atttypmod) from unnest(k.confkey) with ordinality as c (attnum, n)
		             join pg_attribute a on a.attrelid = k.confrelid and a.attnum = c.attnum order by c.n)
		from pg_constraint k
		where k.contype = 'f' and k.confrelid = 'artifacts'::regclass
		order by 2, 1
	`)
	if err != nil {
		t.Fatalf("read the foreign keys to artifacts (id): %v", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (removalIndexedKey, error) {
		var key removalIndexedKey
		err := row.Scan(&key.name, &key.table, &key.action, &key.first, &key.columns, &key.types)
		return key, err
	})
	if err != nil {
		t.Fatalf("read the foreign keys to artifacts (id): %v", err)
	}
	return keys
}

// statement is the statement the key's trigger runs when a row of artifacts is deleted, each value
// of the deleted row a parameter of the type of the column it comes from: the probe of the rows
// that still name it, locked for key share, for a key with no action (or one that restricts), the
// delete of those rows for a key that cascades, and their update for a key that sets null or its
// default.
func (key removalIndexedKey) statement() string {
	conditions := make([]string, len(key.columns))
	assignments := make([]string, len(key.columns))
	value := "null"
	if key.action == "d" {
		value = "default"
	}
	for index, column := range key.columns {
		quoted := pgx.Identifier{column}.Sanitize()
		conditions[index] = fmt.Sprintf("$%d::%s = %s", index+1, key.types[index], quoted)
		assignments[index] = quoted + " = " + value
	}
	where := strings.Join(conditions, " and ")
	switch key.action {
	case "c":
		return "delete from only " + key.table + " where " + where
	case "n", "d":
		return "update only " + key.table + " set " + strings.Join(assignments, ", ") + " where " + where
	default:
		return "select 1 from only " + key.table + " x where " + where + " for key share of x"
	}
}

// removalIndexedPlan is the generic plan of statement, the plan built with no value for its
// parameters (explain (generic_plan)), taken with sequential scans off: the test's tables are
// small, and on a small table the planner reads the whole table though an index is there. The
// explain goes through the simple query protocol, which carries the statement's parameters as
// written; pgx's extended protocol would ask for a value for each.
func removalIndexedPlan(t *testing.T, database *store.Store, statement string) []explaintest.Node {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan of %s: %v", statement, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatalf("turn sequential scans off: %v", err)
	}
	results, err := tx.Conn().PgConn().Exec(ctx, `explain (generic_plan, format json) `+statement).ReadAll()
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		t.Fatalf("explain %s answered %d results, want one plan", statement, len(results))
	}
	return explaintest.Nodes(t, explaintest.OnePlan(t, results[0].Rows[0][0]))
}

// removalIndexedLeads is the first key column of each index of table, by the index's name, as the
// attribute number pg_index.indkey[0] holds (0 for an expression).
func removalIndexedLeads(t *testing.T, database *store.Store, table string) map[string]int16 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select c.relname::text, x.indkey[0]
		from pg_index x join pg_class c on c.oid = x.indexrelid
		where x.indrelid = to_regclass($1::text)
	`, table)
	if err != nil {
		t.Fatalf("read the indexes of %s: %v", table, err)
	}
	defer rows.Close()
	leads := map[string]int16{}
	for rows.Next() {
		var index string
		var lead int16
		if err := rows.Scan(&index, &lead); err != nil {
			t.Fatalf("read an index of %s: %v", table, err)
		}
		leads[index] = lead
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the indexes of %s: %v", table, err)
	}
	return leads
}

// Each foreign-key check of a document's removal probes an index, and the removal's transaction
// sets a lock_timeout (c-removal-indexed; R7 of this issue's spec).
//
// On a database migrated to this head, every foreign key that references artifacts (id) is read
// from pg_constraint, the five of removalIndexedColumns among them. For each key the statement its
// trigger runs when a row of artifacts is deleted is planned with no value for its uuid parameter
// (explain (generic_plan)) and sequential scans off: the plan holds an Index Scan, an Index Only
// Scan or a Bitmap Index Scan on an index whose first key column (pg_index.indkey[0]) is the key's
// column, and no Seq Scan. An index that serves the probe through a later column, as
// user_document_reads_pkey (login, artifact_id) does, does not count.
//
// Then a document made by typing a title (POST /api/v1/issues/{key}/artifacts) is removed
// (DELETE /api/v1/artifacts/{id}) while a second transaction holds user_document_reads in access
// exclusive mode: the removal answers a status other than 204 within its lock_timeout and
// removalIndexedSlack more, and the document's row stands. The second transaction ends once that
// time has passed, whatever the removal answered, and the removal sent again answers 204.
func TestEachForeignKeyCheckOfARemovalProbesAnIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})

	keys := removalIndexedKeys(t, database)
	t.Logf("%d foreign keys reference artifacts (id)", len(keys))
	for _, want := range removalIndexedColumns {
		if !slices.ContainsFunc(keys, func(key removalIndexedKey) bool {
			return key.table == want.table && len(key.columns) == 1 && key.columns[0] == want.column
		}) {
			t.Errorf("no foreign key on %s", want)
		}
	}
	scans := []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"}
	for _, key := range keys {
		statement := key.statement()
		nodes := removalIndexedPlan(t, database, statement)
		leads := removalIndexedLeads(t, database, key.table)
		sequential, led := false, false
		for _, node := range nodes {
			switch {
			case node.Type == "Seq Scan":
				sequential = true
			case slices.Contains(scans, node.Type):
				if lead, held := leads[node.Index]; held && lead == key.first {
					led = true
				}
			}
		}
		if sequential || !led {
			t.Errorf("the foreign key %s of %s.%s plans no scan of an index led by %s", key.name, key.table, key.columns[0], key.columns[0])
			t.Logf("the generic plan of %s: %v", statement, nodes)
		}
	}

	// The removal behind a held lock.
	createTestProject(t, handler, "TEST")
	issue := titleIssue(t, handler, "A document removed behind a held lock")
	document := emptyDocument(t, handler, issue.Key, "Removal notes")
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds user_document_reads: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table user_document_reads in access exclusive mode`); err != nil {
		t.Fatalf("lock user_document_reads: %v", err)
	}
	type answer struct {
		response *httptest.ResponseRecorder
		after    time.Duration
	}
	answered := make(chan answer, 1)
	sent := time.Now()
	go func() {
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/artifacts/"+document.ID, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		answered <- answer{response: response, after: time.Since(sent)}
	}()
	bound := removalIndexedLockTimeout + removalIndexedSlack
	var refused *answer
	select {
	case got := <-answered:
		refused = &got
	case <-time.After(bound):
	}
	// The lock is held until the bound has passed, whatever the removal answered.
	if wait := bound - time.Since(sent); wait > 0 {
		time.Sleep(wait)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("end the transaction that holds user_document_reads: %v", err)
	}
	if refused == nil {
		select {
		case late := <-answered:
			t.Fatalf("the removal waited %.1f s on a held lock, past its lock_timeout of %s", late.after.Seconds(), removalIndexedLockTimeout)
		case <-time.After(testwait.Remaining(t, 30*time.Second)):
			t.Fatalf("the removal waited %.1f s on a held lock, past its lock_timeout of %s", time.Since(sent).Seconds(), removalIndexedLockTimeout)
		}
	}
	if refused.response.Code == http.StatusNoContent {
		t.Fatalf("the removal answered 204 after %.1f s while user_document_reads stood locked, want it refused: the lock it waits behind held nothing of it, so the case proves nothing",
			refused.after.Seconds())
	}
	t.Logf("the removal behind the held lock answered %d after %.1f s: %s", refused.response.Code, refused.after.Seconds(), refused.response.Body.String())
	var stands bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from artifacts where id = $1)`, document.ID).Scan(&stands); err != nil {
		t.Fatalf("read whether the document's row stands: %v", err)
	}
	if !stands {
		t.Errorf("the removal refused with %d left the document's row gone, want it standing", refused.response.Code)
	}
	removedDocument(t, handler, document.ID)
}
