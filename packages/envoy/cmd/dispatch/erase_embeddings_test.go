package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R5: once the erase has run, no column of any table holds the erased bytes, and the embedding
// store is one of those tables. Its rows carry no foreign key to the rows their text was cut
// from, so an erase that never names them leaves the text in embeddings.text_snapshot wherever
// the block-index tick's window arm had written a window before the run - which graph-erase.sh
// met as `public.embeddings.text_snapshot 2` on the runs where the arm came first.
//
// The order is forced rather than waited for. The test holds the block-index pass's own lease
// (docs.BlockIndexLease) from before anything is written to the end, so no tick of the server's
// window arm runs while it does, and it writes the windows itself through the writers that arm
// calls for one owner (refs.WriteItemWindows for the comment, refs.WriteDocumentWindows for each
// document that quotes it, as docs.Service.windowItem and windowDocument call them). It then
// reads that the comment's windows, both documents' windows and the issue's own row - which the
// spec's version wrote through migration 0140's trigger - hold the secret, and the search of
// every column of every table finds it in embeddings.text_snapshot before the erase, which is
// what proves the same search can find it afterwards.
//
// The notes held the secret in their second version only, so their first version is left, and
// with no tick to write their windows again they are still marked owed them when the test reads
// them; the spec's only version held it, so the spec has nothing left to window. The issue's row
// is written again from its title, the spec it stood on being gone.
func TestTheEraseTakesTheErasedTextOutOfTheEmbeddingStore(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	// No settlement runs during the test either: one that ran after the erase would write a
	// version of the redacted live text, and that version's index marks the notes owed their
	// windows whether or not the erase did.
	server := startDispatchProcessWithSettleDelay(t, databaseURL, time.Hour)
	server.waitHealthy(t)
	release := eraseHoldBlockIndex(t, database)
	defer release()
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERE","name":"Erase embeddings"}`, http.StatusCreated)

	secret := eraseTestSecret()
	leak := "The passphrase is " + secret + " and nobody else may read it."
	issue, spec := createEraseIssue(t, server, "ERE", "A spec whose windows held a secret", "# Spec\n\n"+leak+"\n")
	notes := uploadEraseDocument(t, server, issue, "notes.md", "# Notes\n\nNothing secret is written here yet.\n")
	uploadEraseDocument(t, server, issue, "notes.md", "# Notes\n\n"+leak+"\n")
	comment := writeEraseComment(t, server, issue, leak)

	if !writeItemWindowsNow(t, database, comment) {
		t.Fatalf("the window writer wrote no windows for comment %s, so this test holds nothing", comment)
	}
	for _, document := range []string{spec, notes} {
		writeDocumentWindowsNow(t, server, database, document)
	}
	commentOwner := refs.ItemComment.WindowOwnerKey(comment)
	for _, held := range []struct{ what, kind, owner string }{
		{"the comment's windows", refs.ItemComment.WindowKind(), commentOwner},
		{"the spec's windows", refs.DocumentWindowKind, spec},
		{"the notes' windows", refs.DocumentWindowKind, notes},
	} {
		if rows := windowsHolding(t, database, held.kind, held.owner, secret); rows == 0 {
			t.Fatalf("before the erase none of %s holds the secret, so this test proves nothing of them", held.what)
		}
	}
	if rows := issueEmbeddingsHolding(t, database, issue, secret); rows != 1 {
		t.Fatalf("before the erase %d embedding row(s) of issue %s hold the secret, want its one row", rows, issue)
	}
	if places, _ := columnsHolding(t, database, secret); !containsString(places, "public.embeddings.text_snapshot") {
		t.Fatalf("before the erase the search of every column finds the secret in %v and not in public.embeddings.text_snapshot, so it would prove nothing there afterwards", places)
	}

	erasure, err := graphcensus.Erase(ctx, databaseURL, "comment", comment, true, eraseStores(eraseTestWindow))
	if err != nil {
		t.Fatalf("erase %s: %v", comment, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing", comment)
	}
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())

	places, searched := columnsHolding(t, database, secret)
	if len(places) > 0 {
		t.Errorf("after the erase the secret is still in %s (%d columns searched)", strings.Join(places, ", "), searched)
	}
	for _, line := range []string{"  rows: embeddings ", "  rows: embedding_window_states 1\n", "  redacted: embeddings 1\n"} {
		if !strings.Contains(report.String(), line) {
			t.Errorf("the report has no line %q:\n%s", line, report.String())
		}
	}

	// The issue keeps a row, written again from what it now holds and waiting for a vector of
	// its own: no vector of the erased text stands for it.
	var snapshot string
	var vector bool
	if err := database.Pool.QueryRow(ctx,
		`select text_snapshot, embedding is not null from embeddings where kind = 'issue' and id = $1`,
		issue).Scan(&snapshot, &vector); err != nil {
		t.Errorf("read the embedding row of issue %s after the erase: %v", issue, err)
	} else if strings.Contains(snapshot, secret) || vector {
		t.Errorf("issue %s's embedding row reads %q with a vector %t after the erase, want its text without the secret and no vector yet",
			issue, snapshot, vector)
	}

	// The notes keep a version, so they are marked owed the windows the erase took from them;
	// the comment's windows and their state are gone with it.
	var notesOwed bool
	var notesWindows int
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select windows_owed_at is not null from document_block_versions where artifact_id = $1), false),
		       (select count(*) from embeddings where kind = $2 and split_part(id, '#', 1) = $3)`,
		notes, refs.DocumentWindowKind, notes).Scan(&notesOwed, &notesWindows); err != nil {
		t.Fatalf("read whether the notes are owed their windows: %v", err)
	}
	if !notesOwed || notesWindows != 0 {
		t.Errorf("the notes keep version 1 and hold %d window(s) after the erase, owed their windows %t; want none held and the notes owed them, or meaning search has lost them for good",
			notesWindows, notesOwed)
	}
	var commentWindows, commentState int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from embeddings where kind = $1 and split_part(id, '#', 1) = $2),
		       (select count(*) from embedding_window_states where kind = $1 and owner_key = $2)`,
		refs.ItemComment.WindowKind(), commentOwner).Scan(&commentWindows, &commentState); err != nil {
		t.Fatalf("read what the embedding store holds of the comment: %v", err)
	}
	if commentWindows != 0 || commentState != 0 {
		t.Errorf("the embedding store holds %d window(s) and %d window state row(s) of the erased comment, want none",
			commentWindows, commentState)
	}
}

// eraseLeaseNamespace is the first key every loop lease is taken under - duty's leaseNamespace,
// which stays unexported - the four bytes of "LOOP". The second key is the lease's own name,
// which Postgres hashes with hashtext. graph_cost_test.go and memory_harness_test.go state the
// same two for the same lease, behind build tags this test does not carry.
const eraseLeaseNamespace int32 = 0x4C4F4F50

// eraseLeaseWait bounds the wait for the block-index lease. A pass reads one batch of each queue
// it serves and gives the lease back, so this is a hang guard on a wait nothing healthy reaches.
const eraseLeaseWait = 2 * time.Minute

// eraseHoldBlockIndex takes the block-index pass's own lease and answers the call that gives it
// back, as graph_cost_test.go's costHoldBlockIndex does: duty.Exclusive takes that lease before
// every pass and skips the pass where another session holds it, so no tick of the block index,
// its window arm included, runs while the test holds it.
func eraseHoldBlockIndex(t *testing.T, database *store.Store) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the block-index lease: %v", err)
	}
	deadline := time.Now().Add(eraseLeaseWait)
	for {
		var held bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1, hashtext($2))`,
			eraseLeaseNamespace, docs.BlockIndexLease).Scan(&held); err != nil {
			conn.Release()
			t.Fatalf("take the block-index lease: %v", err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			conn.Release()
			t.Fatalf("the block-index pass held the %q lease for more than %s", docs.BlockIndexLease, eraseLeaseWait)
		}
		// pg_try_advisory_lock answers false rather than queueing behind the holder, so a pass
		// running at this moment is waited out here: it gives the lease back when it ends.
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		if err := duty.FreeSessionLock(conn, `select pg_advisory_unlock($1, hashtext($2))`,
			eraseLeaseNamespace, docs.BlockIndexLease); err != nil {
			t.Errorf("give the block-index lease back: %v", err)
		}
	}
}

// writeItemWindowsNow writes comment's windows through the writer the window arm calls for one
// item (docs.Service.windowItem), in a transaction of its own, and reports whether it wrote them.
func writeItemWindowsNow(t *testing.T, database *store.Store, comment string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment's windows: %v", err)
	}
	defer tx.Rollback(ctx)
	windowed, err := refs.WriteItemWindows(ctx, tx, refs.ItemComment, comment)
	if err != nil {
		t.Fatalf("write the windows of comment %s: %v", comment, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the windows of comment %s: %v", comment, err)
	}
	return windowed
}

// writeDocumentWindowsNow writes document's windows through the writer the window arm calls for
// one document (docs.Service.windowDocument), over the text and blocks the server serves, which
// are the ones the arm windows when the live text is its latest version's markdown byte for
// byte; the test fails if it is not, since the arm would window another text.
func writeDocumentWindowsNow(t *testing.T, server *dispatchProcess, database *store.Store, document string) {
	t.Helper()
	ctx := context.Background()
	markdown := eraseDocumentText(t, server, document)
	var stored string
	if err := database.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and markdown is not null
		order by number desc limit 1`, document).Scan(&stored); err != nil {
		t.Fatalf("read the latest version of %s: %v", document, err)
	}
	if markdown != stored {
		t.Fatalf("the live text of %s is not its latest version's markdown, so the arm would window another text:\n%q\n%q",
			document, markdown, stored)
	}
	var blocks []model.ArtifactBlock
	answer := server.call(t, http.MethodGet, "/api/v1/artifacts/"+document+"/blocks", "", http.StatusOK)
	if err := json.Unmarshal(answer, &blocks); err != nil || len(blocks) == 0 {
		t.Fatalf("read the blocks of %s: %v: %s", document, err, answer)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the windows of %s: %v", document, err)
	}
	defer tx.Rollback(ctx)
	if err := refs.WriteDocumentWindows(ctx, tx, document, markdown, blocks); err != nil {
		t.Fatalf("write the windows of %s: %v", document, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the windows of %s: %v", document, err)
	}
}

// windowsHolding counts the windows of kind under owner whose text holds text.
func windowsHolding(t *testing.T, database *store.Store, kind, owner, text string) int {
	t.Helper()
	var held int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings
		where kind = $1 and split_part(id, '#', 1) = $2 and position($3 in text_snapshot) > 0`,
		kind, owner, text).Scan(&held); err != nil {
		t.Fatalf("count the %s windows of %s holding the text: %v", kind, owner, err)
	}
	return held
}

// issueEmbeddingsHolding counts the embedding rows of issue whose text holds text.
func issueEmbeddingsHolding(t *testing.T, database *store.Store, issue, text string) int {
	t.Helper()
	var held int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings where kind = 'issue' and id = $1 and position($2 in text_snapshot) > 0`,
		issue, text).Scan(&held); err != nil {
		t.Fatalf("count the embedding rows of %s holding the text: %v", issue, err)
	}
	return held
}

// columnsHolding searches every column of every table of the database for needle, as
// graph-erase.sh's held_anywhere searches it: each column read as text, a bytea column as the
// needle's UTF-8 bytes and a bytea array element by element, the columns of a type no text
// is written into left out. It answers each schema.table.column that holds it, with how many
// rows do, and how many columns it searched.
func columnsHolding(t *testing.T, database *store.Store, needle string) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select c.table_schema, c.table_name, c.column_name, c.data_type, c.udt_name
		from information_schema.columns c
		join information_schema.tables t
		  on t.table_schema = c.table_schema and t.table_name = c.table_name
		where t.table_type = 'BASE TABLE'
		  and c.table_schema not in ('pg_catalog', 'information_schema')
		  and c.data_type not in ('smallint', 'integer', 'bigint', 'numeric', 'real', 'double precision',
		    'boolean', 'uuid', 'date', 'interval', 'time with time zone', 'time without time zone',
		    'timestamp with time zone', 'timestamp without time zone', 'inet', 'cidr', 'macaddr',
		    'macaddr8', 'oid', 'xid', 'xid8', 'pg_lsn', 'money', 'bit', 'bit varying')
		order by 1, 2, 3`)
	if err != nil {
		t.Fatalf("list the columns of the database: %v", err)
	}
	type column struct{ schema, table, name, dataType, udtName string }
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
		var held column
		err := row.Scan(&held.schema, &held.table, &held.name, &held.dataType, &held.udtName)
		return held, err
	})
	if err != nil {
		t.Fatalf("list the columns of the database: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("the database lists no column to search, so the search proves nothing")
	}
	var places []string
	for _, held := range columns {
		name := pgx.Identifier{held.name}.Sanitize()
		test := "position($1 in " + name + "::text) > 0"
		switch {
		case held.dataType == "bytea":
			test = "position(convert_to($1, 'UTF8') in " + name + ") > 0"
		case held.udtName == "_bytea":
			test = "exists (select 1 from unnest(" + name + ") element where position(convert_to($1, 'UTF8') in element) > 0)"
		}
		var count int64
		if err := database.Pool.QueryRow(ctx,
			`select count(*) from `+pgx.Identifier{held.schema, held.table}.Sanitize()+` where `+test,
			needle).Scan(&count); err != nil {
			t.Fatalf("search %s.%s.%s: %v", held.schema, held.table, held.name, err)
		}
		if count > 0 {
			places = append(places, fmt.Sprintf("%s.%s.%s %d", held.schema, held.table, held.name, count))
		}
	}
	return places, len(columns)
}

// containsString reports whether one of places starts with the column name place.
func containsString(places []string, place string) bool {
	for _, held := range places {
		if strings.HasPrefix(held, place+" ") {
			return true
		}
	}
	return false
}
